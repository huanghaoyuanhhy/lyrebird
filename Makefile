.PHONY: build test vet fmt run e2e-up e2e e2e-down

build:
	go build -o bin/lyrebird ./cmd/gateway

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run:
	go run ./cmd/gateway

# E2E stacks: milvus (the gateway's backend) plus the reference
# elasticsearch / postgres+pgvector the parity suites replay the same
# queries against. Host ports dodge the classics: PG 15432, ES 19200,
# milvus stays on 19530/9091 (no in-container healthcheck — poll
# /healthz until it answers).
COMPOSE = docker compose -f docker-compose.e2e.yml

e2e-up:
	$(COMPOSE) up -d --wait
	@for i in $$(seq 1 60); do \
		curl -sf http://127.0.0.1:9091/healthz > /dev/null && exit 0; \
		sleep 2; \
	done; \
	echo "milvus did not become healthy in 120s" >&2; \
	$(COMPOSE) logs milvus >&2; \
	exit 1

# The full-stack suites (store executor, ES surface, PG wire) run against
# the compose milvus; the parity suites additionally replay every query
# against the compose elasticsearch and postgres. The milvus token is a
# placeholder — standalone ships with authorization disabled.
e2e: e2e-up
	LYREBIRD_TEST_MILVUS_URI=http://127.0.0.1:19530 \
	LYREBIRD_TEST_MILVUS_TOKEN=local \
	LYREBIRD_TEST_ES_ADDR=http://127.0.0.1:19200 \
	LYREBIRD_TEST_PG_DSN='postgres://postgres:postgres@127.0.0.1:15432/postgres?sslmode=disable' \
	go test -p 1 ./internal/... -run 'E2E|Parity' -count=1

e2e-down:
	$(COMPOSE) down -v
