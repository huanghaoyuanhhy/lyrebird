.PHONY: build test vet fmt run

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
