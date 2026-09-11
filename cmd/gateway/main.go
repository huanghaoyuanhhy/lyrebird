// Command lyrebird starts the protocol-translation gateway: it exposes
// Elasticsearch / PostgreSQL protocol entry points and translates queries
// into Milvus operations. See README.md for architecture and phasing.
package main

import (
	"flag"
	"log"
	"net/http"

	"lyrebird/internal/esserver"
)

func main() {
	esAddr := flag.String("es-addr", "127.0.0.1:9200", "Elasticsearch-compatible entry point listen address")
	pgAddr := flag.String("pg-addr", "127.0.0.1:5433", "PostgreSQL wire entry point listen address (placeholder until Phase 2)")
	flag.Parse()

	log.Printf("pg wire entry point %s not implemented yet (Phase 2)", *pgAddr)
	log.Printf("ES-compatible entry point listening on %s", *esAddr)
	if err := http.ListenAndServe(*esAddr, esserver.New()); err != nil {
		log.Fatal(err)
	}
}
