package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func TestWarmImageURLs(t *testing.T) {
	hash := hex.EncodeToString(make([]byte, 32))
	fixture := &feedFixture{t: t, queries: []feedQuery{
		{contains: "profile_image_id", args: []any{}, columns: []string{"secret_key_hash", "image_id"}, rows: [][]driver.Value{{hash, "profile"}}},
		{contains: "FROM posts", args: []any{}, columns: []string{"secret_key_hash", "image_id"}, rows: [][]driver.Value{{hash, "post"}}},
		{contains: "FROM realmojis", args: []any{}, columns: []string{"secret_key_hash", "image_id"}, rows: [][]driver.Value{{hash, "realmoji"}}},
	}}
	for i := range fixture.queries {
		for j := range 64 {
			fixture.queries[i].rows = append(fixture.queries[i].rows, []driver.Value{hash, fmt.Sprintf("stage-%d-image-%d", i, j)})
		}
	}

	db := sqlx.NewDb(sql.OpenDB(feedConnector{fixture}), "fixture")
	defer db.Close()
	var cache imageURLCache
	if count := warmImageURLs(context.Background(), db, &cache); count != 195 {
		t.Fatalf("warmed %d", count)
	}
	if fixture.calls != 3 || cache.misses != 195 {
		t.Fatal("did not warm each image class")
	}
	for _, id := range []string{"profile", "post", "realmoji"} {
		if _, ok := cache.lookup(imageURLCacheKey{key: string(make([]byte, 32)), imageID: id}, time.Now()); !ok {
			t.Fatalf("missing %s", id)
		}
	}
	// Warming is never required for a new image or a fresh process.
	cache.get(make([]byte, 32), "new-image", time.Now())
	var fresh imageURLCache
	fresh.get(make([]byte, 32), "post", time.Now())
	if fresh.misses != 1 {
		t.Fatal("fresh cache did not sign on demand")
	}
}

func TestWarmImageURLsCancellationAndDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if warmImageURLs(ctx, nil, new(imageURLCache)) != 0 {
		t.Fatal("cancelled warmup did work")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if warmImageURLs(ctx, nil, new(imageURLCache)) != 0 {
		t.Fatal("warmup ignored response deadline reserve")
	}
}

func TestWarmImageURLsInvalidKey(t *testing.T) {
	f := &feedFixture{t: t, queries: []feedQuery{{contains: "profile_image_id", args: []any{}, columns: []string{"secret_key_hash", "image_id"}, rows: [][]driver.Value{{"bad", "image"}}}}}
	db := sqlx.NewDb(sql.OpenDB(feedConnector{f}), "fixture")
	defer db.Close()
	var cache imageURLCache
	if count, err := warmImageURLQuery(context.Background(), db, &cache, imageURLWarmupQueries[0]); err == nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if len(cache.entries) != 0 {
		t.Fatal("invalid signing key cached")
	}
}

func TestWarmImageURLQueryWaitsForWorkersOnError(t *testing.T) {
	hash := hex.EncodeToString(make([]byte, 32))
	values := make([][]driver.Value, 0, 33)
	for i := range 32 {
		values = append(values, []driver.Value{hash, fmt.Sprintf("image-%d", i)})
	}
	values = append(values, []driver.Value{"invalid-key", "bad-image"})
	f := &feedFixture{t: t, queries: []feedQuery{{contains: "profile_image_id", args: []any{}, columns: []string{"secret_key_hash", "image_id"}, rows: values}}}
	db := sqlx.NewDb(sql.OpenDB(feedConnector{f}), "fixture")
	defer db.Close()
	var cache imageURLCache
	count, err := warmImageURLQuery(context.Background(), db, &cache, imageURLWarmupQueries[0])
	if err == nil || count != 32 || len(cache.entries) != 32 {
		t.Fatalf("workers not finished: count=%d entries=%d err=%v", count, len(cache.entries), err)
	}
}

// Opt-in read-only check: run on isu01 with the service's DB environment. This
// does not initialize/reset data or modify the running service's cache.
func TestWarmImageURLsReadOnlyPostgres(t *testing.T) {
	if os.Getenv("ISUREAL_TEST_WARM_IMAGE_URLS") != "1" {
		t.Skip("opt-in read-only warmup validation")
	}
	cfg := ConfigFromEnv()
	if cfg.Region != "asia" {
		t.Fatal("warmup probe must run in asia")
	}
	db, err := sqlx.Connect("pgx", cfg.DatabaseDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var cache imageURLCache
	count := warmImageURLs(context.Background(), db, &cache)
	t.Logf("warmed=%d entries=%d misses=%d", count, len(cache.entries), cache.misses)
	if count == 0 {
		t.Fatal("no images warmed")
	}
}
