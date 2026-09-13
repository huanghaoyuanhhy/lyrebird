// Command lyrebird starts the protocol-translation gateway: it exposes
// Elasticsearch / PostgreSQL protocol entry points and translates queries
// into Milvus operations. See README.md for architecture and phasing.
package main

import (
	"flag"
	"net/http"

	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/esserver"
)

func main() {
	esAddr := flag.String("es-addr", "127.0.0.1:9200", "Elasticsearch-compatible entry point listen address")
	pgAddr := flag.String("pg-addr", "127.0.0.1:5433", "PostgreSQL wire entry point listen address (placeholder until Phase 2)")
	flag.Parse()

	// Development format for now; deployments switch to zap.NewProduction().
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()
	zap.ReplaceGlobals(logger)

	logger.Info("pg wire entry point not implemented yet (Phase 2)", zap.String("addr", *pgAddr))
	logger.Info("ES-compatible entry point listening", zap.String("addr", *esAddr))
	if err := http.ListenAndServe(*esAddr, esserver.New()); err != nil {
		logger.Fatal("es entry point failed", zap.Error(err))
	}
}
