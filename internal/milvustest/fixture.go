// Package milvustest provisions a shared fixture collection for the
// env-guarded e2e suites (store executor, esserver). It holds no
// credentials: tests read them from environment variables and skip the
// whole suite when unset, so nothing secret lands in the repo.
package milvustest

import (
	"context"
	"fmt"
	"os"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
)

// Environment variables that carry the e2e endpoint credentials.
const (
	URIEnv   = "LYREBIRD_TEST_MILVUS_URI"   // https://host:19530 (https:// enables TLS)
	TokenEnv = "LYREBIRD_TEST_MILVUS_TOKEN" // api key or user:password
)

// Collection is the fixture collection the suites rebuild per run.
const Collection = "lyrebird_e2e_store"

// Config is a parsed endpoint.
type Config struct {
	URI   string
	Token string
}

// FromEnv returns the configured endpoint; ok is false when either variable
// is unset (the suites then skip themselves).
func FromEnv() (Config, bool) {
	uri, token := os.Getenv(URIEnv), os.Getenv(TokenEnv)
	if uri == "" || token == "" {
		return Config{}, false
	}
	return Config{URI: uri, Token: token}, true
}

// Seed rebuilds the fixture: five docs with distinct prices
// (30, 20, 15, 10.5, 5) and a three/two active split, plus a 4-dim vector
// field because Milvus collections require one.
//
//	id name    price active created_ms
//	1  alpha   10.5  true   1700000000000
//	2  bravo   20.0  false  1700000100000
//	3  charlie  5.0  true   1700000200000
//	4  delta   30.0  true   1700000300000
//	5  echo    15.0  false  1700000400000
func Seed(ctx context.Context, cfg Config) error {
	cli, err := client.NewClient(ctx, client.Config{
		Address:       cfg.URI,
		APIKey:        cfg.Token,
		EnableTLSAuth: len(cfg.URI) >= 8 && cfg.URI[:8] == "https://",
	})
	if err != nil {
		return fmt.Errorf("connect for seeding: %w", err)
	}
	defer cli.Close()

	has, err := cli.HasCollection(ctx, Collection)
	if err != nil {
		return fmt.Errorf("has collection: %w", err)
	}
	if has {
		if err := cli.DropCollection(ctx, Collection); err != nil {
			return fmt.Errorf("drop stale fixture: %w", err)
		}
	}

	err = cli.CreateCollection(ctx, entity.NewSchema().
		WithName(Collection).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("name").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
		WithField(entity.NewField().WithName("price").WithDataType(entity.FieldTypeDouble)).
		WithField(entity.NewField().WithName("qty").WithDataType(entity.FieldTypeInt32)).
		WithField(entity.NewField().WithName("active").WithDataType(entity.FieldTypeBool)).
		WithField(entity.NewField().WithName("created_ms").WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName("emb").WithDataType(entity.FieldTypeFloatVector).WithDim(4)), 1)
	if err != nil {
		return fmt.Errorf("create fixture: %w", err)
	}

	_, err = cli.Insert(ctx, Collection, "",
		entity.NewColumnInt64("id", []int64{1, 2, 3, 4, 5}),
		entity.NewColumnVarChar("name", []string{"alpha", "bravo", "charlie", "delta", "echo"}),
		entity.NewColumnDouble("price", []float64{10.5, 20.0, 5.0, 30.0, 15.0}),
		entity.NewColumnInt32("qty", []int32{1, 2, 3, 4, 5}),
		entity.NewColumnBool("active", []bool{true, false, true, true, false}),
		entity.NewColumnInt64("created_ms", []int64{1700000000000, 1700000100000, 1700000200000, 1700000300000, 1700000400000}),
		entity.NewColumnFloatVector("emb", 4, [][]float32{
			{0.1, 0.2, 0.3, 0.4}, {0.5, 0.6, 0.7, 0.8}, {0.9, 1.0, 1.1, 1.2},
			{1.3, 1.4, 1.5, 1.6}, {1.7, 1.8, 1.9, 2.0},
		}))
	if err != nil {
		return fmt.Errorf("insert fixture: %w", err)
	}
	if err := cli.Flush(ctx, Collection, false); err != nil {
		return fmt.Errorf("flush fixture: %w", err)
	}
	if err := cli.CreateIndex(ctx, Collection, "emb",
		entity.NewGenericIndex("emb_idx", entity.AUTOINDEX, map[string]string{"metric_type": "L2"}), false); err != nil {
		return fmt.Errorf("index fixture: %w", err)
	}
	if err := cli.LoadCollection(ctx, Collection, false); err != nil {
		return fmt.Errorf("load fixture: %w", err)
	}
	return nil
}
