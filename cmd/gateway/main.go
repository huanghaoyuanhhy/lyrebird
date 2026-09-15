// Command lyrebird starts the protocol-translation gateway: it exposes
// Elasticsearch / PostgreSQL protocol entry points and translates queries
// into Milvus operations. See README.md for architecture and phasing.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/huanghaoyuanhhy/lyrebird/internal/esserver"
	"github.com/huanghaoyuanhhy/lyrebird/internal/pgserver"
	"github.com/huanghaoyuanhhy/lyrebird/internal/store"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var esAddr, pgAddr, milvusURI, milvusToken string

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

			exec, err := buildExecutor(cmd.Context(), milvusURI, milvusToken, logger)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// The ES front is the primary process: it owns the run error and
			// outlives the pg front's shutdown on Ctrl-C.
			pgSrv := pgserver.New(exec, logger)
			pgErr := make(chan error, 1)
			go func() { pgErr <- pgSrv.ListenAndServe(ctx, pgAddr) }()

			httpSrv := &http.Server{Addr: esAddr, Handler: esserver.New(exec, logger)}
			go func() {
				<-ctx.Done()
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = httpSrv.Shutdown(shutdownCtx)
			}()

			logger.Info("ES-compatible entry point listening", zap.String("addr", esAddr))
			runErr := httpSrv.ListenAndServe()
			stop() // the pg front shuts down on the same signal
			if pgWaitErr := <-pgErr; pgWaitErr != nil && runErr == nil {
				runErr = pgWaitErr
			}
			if errors.Is(runErr, http.ErrServerClosed) {
				return nil
			}
			return runErr
		},
	}

	cmd.Flags().StringVar(&esAddr, "es-addr", "127.0.0.1:9200", "Elasticsearch-compatible entry point listen address")
	cmd.Flags().StringVar(&pgAddr, "pg-addr", "127.0.0.1:5433", "PostgreSQL wire entry point listen address")
	cmd.Flags().StringVar(&milvusURI, "milvus-uri", "", "Milvus/Zilliz Cloud endpoint (https://host:19530); empty runs the log-only dev executor")
	cmd.Flags().StringVar(&milvusToken, "milvus-token", "", "Milvus auth token (API key or user:password)")

	return cmd
}

// buildExecutor picks the backing store: the real Milvus executor when an
// endpoint is configured, the log stand-in otherwise so the gateway still
// boots for development.
func buildExecutor(ctx context.Context, uri, token string, logger *zap.Logger) (store.Executor, error) {
	if uri == "" {
		logger.Info("no --milvus-uri given; searches will log and return empty results")
		return &store.LogExecutor{Logger: logger}, nil
	}
	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return store.NewMilvusExecutor(connectCtx, store.MilvusConfig{URI: uri, Token: token})
}
