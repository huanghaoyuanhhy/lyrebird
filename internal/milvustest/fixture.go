// Package milvustest provisions shared fixture collections for the
// env-guarded e2e suites (store executor, esserver). It holds no
// credentials: tests read them from environment variables and skip the
// whole suite when unset, so nothing secret lands in the repo.
package milvustest

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/index"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
)

// Environment variables that carry the e2e endpoint credentials.
const (
	URIEnv   = "LYREBIRD_TEST_MILVUS_URI"   // https://host:19530 (https:// enables TLS)
	TokenEnv = "LYREBIRD_TEST_MILVUS_TOKEN" // api key or user:password
)

// Collection is the scalar-fixture collection the suites rebuild per run.
const Collection = "lyrebird_e2e_store"

// TextCollection is the BM25 full-text fixture: an analyzed VarChar field
// wired to a sparse vector through a BM25 Function, exercising the
// match → TEXT_MATCH path end to end.
const TextCollection = "lyrebird_e2e_text"

// VectorCollection is the cosine-indexed vector fixture for the pgvector
// distance path: five unit-square vectors whose similarity to the query
// '[1, 0, 0, 0]' decreases strictly with id, so nearest-first order is
// [1 2 3 4 5] under both cosine and L2 (no ties to smooth over).
const VectorCollection = "lyrebird_e2e_vec"

// WriteCollection is the write-path fixture: the scalar shape with a
// nullable field and a small vector, seeded empty — the write suites fill
// it and read it back.
const WriteCollection = "lyrebird_e2e_write"

// AutoIDCollection is the auto-generated-key fixture: Milvus assigns ids.
const AutoIDCollection = "lyrebird_e2e_autoid"

// StringPKCollection is the string-key fixture: the URL id is the key.
const StringPKCollection = "lyrebird_e2e_strpk"

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

// Connect opens a client with the same settings the executor uses.
func Connect(ctx context.Context, cfg Config) (*milvusclient.Client, error) {
	return milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:       cfg.URI,
		APIKey:        cfg.Token,
		EnableTLSAuth: strings.HasPrefix(cfg.URI, "https://"),
	})
}

// Seed rebuilds both fixture collections. See seedDocs and seedText for the
// exact contents.
func Seed(ctx context.Context, cfg Config) error {
	cli, err := Connect(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connect for seeding: %w", err)
	}
	defer cli.Close(ctx)

	if err := seedDocs(ctx, cli); err != nil {
		return fmt.Errorf("scalar fixture: %w", err)
	}
	if err := seedText(ctx, cli); err != nil {
		return fmt.Errorf("text fixture: %w", err)
	}
	if err := seedVec(ctx, cli); err != nil {
		return fmt.Errorf("vector fixture: %w", err)
	}
	if err := seedWrite(ctx, cli); err != nil {
		return fmt.Errorf("write fixtures: %w", err)
	}
	return nil
}

// seedWrite rebuilds the write-path fixtures, all empty: the suites insert
// through lyrebird itself and read back.
//
//	lyrebird_e2e_write: id (int64 pk), name, price, active, note (null), emb (dim 2)
//	lyrebird_e2e_autoid: id (int64 pk, auto-generated), v
//	lyrebird_e2e_strpk:  id (varchar pk), v
func seedWrite(ctx context.Context, cli *milvusclient.Client) error {
	if err := recreate(ctx, cli, WriteCollection, entity.NewSchema().
		WithName(WriteCollection).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("name").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32)).
		WithField(entity.NewField().WithName("price").WithDataType(entity.FieldTypeDouble)).
		WithField(entity.NewField().WithName("active").WithDataType(entity.FieldTypeBool)).
		WithField(entity.NewField().WithName("note").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32).WithNullable(true)).
		WithField(entity.NewField().WithName("emb").WithDataType(entity.FieldTypeFloatVector).WithDim(2))); err != nil {
		return err
	}
	if err := finalize(ctx, cli, WriteCollection, "emb", entity.L2); err != nil {
		return err
	}

	// Milvus refuses collections without a vector field, so the scalar-key
	// fixtures carry a nullable dim-2 vector the write rows may omit.
	if err := recreate(ctx, cli, AutoIDCollection, entity.NewSchema().
		WithName(AutoIDCollection).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true).WithIsAutoID(true)).
		WithField(entity.NewField().WithName("v").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32)).
		WithField(entity.NewField().WithName("emb").WithDataType(entity.FieldTypeFloatVector).WithDim(2).WithNullable(true))); err != nil {
		return err
	}
	if err := finalize(ctx, cli, AutoIDCollection, "emb", entity.L2); err != nil {
		return err
	}

	if err := recreate(ctx, cli, StringPKCollection, entity.NewSchema().
		WithName(StringPKCollection).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("v").WithDataType(entity.FieldTypeVarChar).WithMaxLength(32)).
		WithField(entity.NewField().WithName("emb").WithDataType(entity.FieldTypeFloatVector).WithDim(2).WithNullable(true))); err != nil {
		return err
	}
	return finalize(ctx, cli, StringPKCollection, "emb", entity.L2)
}

// seedDocs rebuilds the scalar fixture: five docs with distinct prices
// (30, 20, 15, 10.5, 5), a three/two active split, and a nullable note
// field (null on id 2 and 5) so exists semantics are exercised for real.
//
//	id name    price active created_ms    note
//	1  alpha   10.5  true   1700000000000 "alpha note"
//	2  bravo   20.0  false  1700000100000 null
//	3  charlie  5.0  true   1700000200000 "charlie note"
//	4  delta   30.0  true   1700000300000 "delta note"
//	5  echo    15.0  false  1700000400000 null
func seedDocs(ctx context.Context, cli *milvusclient.Client) error {
	if err := recreate(ctx, cli, Collection, entity.NewSchema().
		WithName(Collection).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("name").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64)).
		WithField(entity.NewField().WithName("price").WithDataType(entity.FieldTypeDouble)).
		WithField(entity.NewField().WithName("qty").WithDataType(entity.FieldTypeInt32)).
		WithField(entity.NewField().WithName("active").WithDataType(entity.FieldTypeBool)).
		WithField(entity.NewField().WithName("created_ms").WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName("note").WithDataType(entity.FieldTypeVarChar).WithMaxLength(64).WithNullable(true)).
		WithField(entity.NewField().WithName("emb").WithDataType(entity.FieldTypeFloatVector).WithDim(4))); err != nil {
		return err
	}

	_, err := cli.Insert(ctx, milvusclient.NewColumnBasedInsertOption(Collection,
		column.NewColumnInt64("id", []int64{1, 2, 3, 4, 5}),
		column.NewColumnVarChar("name", []string{"alpha", "bravo", "charlie", "delta", "echo"}),
		column.NewColumnDouble("price", []float64{10.5, 20.0, 5.0, 30.0, 15.0}),
		column.NewColumnInt32("qty", []int32{1, 2, 3, 4, 5}),
		column.NewColumnBool("active", []bool{true, false, true, true, false}),
		column.NewColumnInt64("created_ms", []int64{1700000000000, 1700000100000, 1700000200000, 1700000300000, 1700000400000}),
		mustNullableVarChar("note",
			[]string{"alpha note", "charlie note", "delta note"}, // compact: non-null values only
			[]bool{true, false, true, true, false}),
		column.NewColumnFloatVector("emb", 4, [][]float32{
			{0.1, 0.2, 0.3, 0.4}, {0.5, 0.6, 0.7, 0.8}, {0.9, 1.0, 1.1, 1.2},
			{1.3, 1.4, 1.5, 1.6}, {1.7, 1.8, 1.9, 2.0},
		})))
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return finalize(ctx, cli, Collection, "emb", entity.L2)
}

// seedText rebuilds the BM25 fixture: an analyzed body field feeding a
// sparse vector through a BM25 Function.
//
//	id body
//	1  "the quick brown fox jumps over the lazy dog"
//	2  "a fast brown dog barks at night"
//	3  "completely unrelated filler text lives here"
//	4  "quick quick slow turtle"
//	5  "foxes and quick rabbits play"
func seedText(ctx context.Context, cli *milvusclient.Client) error {
	if err := recreate(ctx, cli, TextCollection, entity.NewSchema().
		WithName(TextCollection).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("body").WithDataType(entity.FieldTypeVarChar).WithMaxLength(256).WithEnableAnalyzer(true).WithEnableMatch(true)).
		WithField(entity.NewField().WithName("sparse").WithDataType(entity.FieldTypeSparseVector)).
		WithFunction(entity.NewFunction().
			WithName("body_bm25").
			WithType(entity.FunctionTypeBM25).
			WithInputFields("body").
			WithOutputFields("sparse"))); err != nil {
		return err
	}

	_, err := cli.Insert(ctx, milvusclient.NewColumnBasedInsertOption(TextCollection,
		column.NewColumnInt64("id", []int64{1, 2, 3, 4, 5}),
		column.NewColumnVarChar("body", []string{
			"the quick brown fox jumps over the lazy dog",
			"a fast brown dog barks at night",
			"completely unrelated filler text lives here",
			"quick quick slow turtle",
			"foxes and quick rabbits play",
		})))
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return finalize(ctx, cli, TextCollection, "sparse", entity.BM25)
}

// seedVec rebuilds the cosine vector fixture:
//
//	id tag   emb
//	1  odd   [1.0, 0.0, 0.0, 0.0]
//	2  even  [0.8, 0.2, 0.0, 0.0]
//	3  odd   [0.6, 0.4, 0.0, 0.0]
//	4  even  [0.4, 0.6, 0.0, 0.0]
//	5  odd   [0.2, 0.8, 0.0, 0.0]
func seedVec(ctx context.Context, cli *milvusclient.Client) error {
	if err := recreate(ctx, cli, VectorCollection, entity.NewSchema().
		WithName(VectorCollection).
		WithField(entity.NewField().WithName("id").WithDataType(entity.FieldTypeInt64).WithIsPrimaryKey(true)).
		WithField(entity.NewField().WithName("tag").WithDataType(entity.FieldTypeVarChar).WithMaxLength(16)).
		WithField(entity.NewField().WithName("emb").WithDataType(entity.FieldTypeFloatVector).WithDim(4))); err != nil {
		return err
	}

	_, err := cli.Insert(ctx, milvusclient.NewColumnBasedInsertOption(VectorCollection,
		column.NewColumnInt64("id", []int64{1, 2, 3, 4, 5}),
		column.NewColumnVarChar("tag", []string{"odd", "even", "odd", "even", "odd"}),
		column.NewColumnFloatVector("emb", 4, [][]float32{
			{1.0, 0.0, 0.0, 0.0}, {0.8, 0.2, 0.0, 0.0}, {0.6, 0.4, 0.0, 0.0},
			{0.4, 0.6, 0.0, 0.0}, {0.2, 0.8, 0.0, 0.0},
		})))
	if err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return finalize(ctx, cli, VectorCollection, "emb", entity.COSINE)
}

// mustNullableVarChar adapts the (column, error) nullable constructor to a
// plain column value for the insert option's variadic list.
func mustNullableVarChar(name string, values []string, valid []bool) column.Column {
	col, err := column.NewNullableColumnVarChar(name, values, valid)
	if err != nil {
		panic(fmt.Sprintf("fixture column %s: %v", name, err))
	}
	return col
}

// recreate drops and creates a collection with the given schema.
func recreate(ctx context.Context, cli *milvusclient.Client, name string, schema *entity.Schema) error {
	has, err := cli.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
	if err != nil {
		return fmt.Errorf("has collection: %w", err)
	}
	if has {
		if err := cli.DropCollection(ctx, milvusclient.NewDropCollectionOption(name)); err != nil {
			return fmt.Errorf("drop stale fixture: %w", err)
		}
	}
	if err := cli.CreateCollection(ctx, milvusclient.NewCreateCollectionOption(name, schema).WithShardNum(1)); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	return nil
}

// finalize flushes, indexes the vector field, and loads the collection.
func finalize(ctx context.Context, cli *milvusclient.Client, name, vectorField string, metric entity.MetricType) error {
	if _, err := cli.Flush(ctx, milvusclient.NewFlushOption(name)); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	_, err := cli.CreateIndex(ctx, milvusclient.NewCreateIndexOption(name, vectorField, index.NewAutoIndex(metric)))
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	task, err := cli.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(name))
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	return task.Await(ctx)
}
