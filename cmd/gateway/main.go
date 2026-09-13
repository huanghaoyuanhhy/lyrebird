// Command lyrebird starts the protocol-translation gateway: it exposes
// Elasticsearch / PostgreSQL protocol entry points and translates queries
// into Milvus operations. See README.md for architecture and phasing.
package main

import (
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/esserver"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var esAddr, pgAddr string

	cmd := &cobra.Command{
		Use:   "lyrebird",
		Short: "Elasticsearch / PostgreSQL protocol gateway backed by Milvus",
		// Runtime failures should print an error, not the help text.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Development format for now; deployments switch to zap.NewProduction().
			logger, err := zap.NewDevelopment()
			if err != nil {
				return err
			}
			defer logger.Sync()
			zap.ReplaceGlobals(logger)

			logger.Info("pg wire entry point not implemented yet (Phase 2)", zap.String("addr", pgAddr))
			logger.Info("ES-compatible entry point listening", zap.String("addr", esAddr))
			return http.ListenAndServe(esAddr, esserver.New())
		},
	}

	cmd.Flags().StringVar(&esAddr, "es-addr", "127.0.0.1:9200", "Elasticsearch-compatible entry point listen address")
	cmd.Flags().StringVar(&pgAddr, "pg-addr", "127.0.0.1:5433", "PostgreSQL wire entry point listen address (placeholder until Phase 2)")

	return cmd
}
