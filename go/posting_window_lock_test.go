package isureal

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

// Opt in with a disposable local PostgreSQL database. No production DSN is used.
func localPostgresTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DATABASE_URL is not set; requires disposable local PostgreSQL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "localhost" && cfg.Host != "127.0.0.1" && cfg.Host != "::1" && !strings.HasPrefix(cfg.Host, "/") {
		t.Fatal("test database must be local")
	}
	db, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPostingWindowSharedLocksPostgres(t *testing.T) {
	db := localPostgresTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schema := "lock_test_" + uuid.NewString()[:8]
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	if _, err := db.ExecContext(ctx, `CREATE TABLE `+schema+`.posting_windows (id text PRIMARY KEY, region text, local_date date, starts_at timestamptz, ended_at timestamptz)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO `+schema+`.posting_windows VALUES ('window','jp',CURRENT_DATE,now(),NULL)`); err != nil {
		t.Fatal(err)
	}
	begin := func() *sqlx.Tx {
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback() })
		if _, err := tx.ExecContext(ctx, `SET LOCAL search_path TO `+schema); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	one, two := begin(), begin()
	lock := func(tx *sqlx.Tx) {
		var window struct {
			ID        string    `db:"id"`
			LocalDate time.Time `db:"local_date"`
			StartsAt  time.Time `db:"starts_at"`
		}
		if err := tx.GetContext(ctx, &window, activePostingWindowQuery, "jp"); err != nil {
			t.Fatal(err)
		}
	}
	lock(one)
	lock(two) // Must succeed while the first posting transaction still holds its lock.
	ending := begin()
	_, err := ending.ExecContext(ctx, `UPDATE posting_windows SET ended_at=now() WHERE region='jp' AND ended_at IS NULL`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("ending window should wait for posters: %v", err)
	}
	ending.Rollback()
	if err := one.Commit(); err != nil {
		t.Fatal(err)
	}
	ending = begin()
	_, err = ending.ExecContext(ctx, `UPDATE posting_windows SET ended_at=now() WHERE region='jp' AND ended_at IS NULL`)
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("second poster must still prevent ending: %v", err)
	}
	ending.Rollback()
	if err := two.Commit(); err != nil {
		t.Fatal(err)
	}
	ending = begin()
	if _, err := ending.ExecContext(ctx, `UPDATE posting_windows SET ended_at=now() WHERE region='jp' AND ended_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if err := ending.Commit(); err != nil {
		t.Fatal(err)
	}
	after := begin()
	var count int
	if err := after.GetContext(ctx, &count, `SELECT count(*) FROM posting_windows WHERE ended_at IS NULL`); err != nil || count != 0 {
		t.Fatalf("ended window still available: count=%d err=%v", count, err)
	}
	after.Rollback()
}
