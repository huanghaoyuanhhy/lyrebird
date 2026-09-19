#!/usr/bin/env bash
# pg-play.sh — boot the gateway against a Milvus cluster and drop into psql.
#
#   source <your-credentials>.env    # exports LYREBIRD_TEST_MILVUS_URI / _TOKEN
#   ./scripts/pg-play.sh
#
# The gateway (ES + PG fronts) lives for the psql session; exiting psql shuts
# it down. The fixture collections (lyrebird_e2e_store / lyrebird_e2e_text)
# are the same the e2e suite seeds; if they are missing, seed first with:
#
#   source <your-credentials>.env && go test ./internal/pgserver/ -run TestPgserverFullStackE2E
#
# Catalog UAT, once inside psql:
#   \l                          -- pg_database: every Milvus database
#   \dt                         -- pg_class/pg_namespace: collections as tables
#   \d+ lyrebird_e2e_store      -- pg_attribute: fields, types, nullability
#   SELECT current_database();  -- the connected Milvus database
# GUI: point DataGrip at jdbc:postgresql://127.0.0.1:5433/postgres and let it
# browse (see README "Connecting GUI tools"); the ES REST plugin connects the
# same way against the gateway's 9200 front.

set -euo pipefail

: "${LYREBIRD_TEST_MILVUS_URI:?set LYREBIRD_TEST_MILVUS_URI (https://host:19530)}"
: "${LYREBIRD_TEST_MILVUS_TOKEN:?set LYREBIRD_TEST_MILVUS_TOKEN}"

PSQL=${PSQL:-psql}
if ! command -v "$PSQL" >/dev/null; then
	PSQL=/opt/homebrew/opt/postgresql@16/bin/psql
fi

PORT=${PORT:-5433}

go build -o bin/lyrebird ./cmd/gateway
./bin/lyrebird --pg-addr "127.0.0.1:$PORT" \
	--milvus-uri "$LYREBIRD_TEST_MILVUS_URI" \
	--milvus-token "$LYREBIRD_TEST_MILVUS_TOKEN" &
gateway_pid=$!
trap 'kill "$gateway_pid" 2>/dev/null' EXIT

sleep 1 # let the listeners come up

"$PSQL" "postgres://player@127.0.0.1:$PORT/postgres"
