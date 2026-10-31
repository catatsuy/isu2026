package isureal

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// Only creates tables in an isolated test schema of the explicitly configured
// disposable local database. Application schema files are never modified.
func registrationPostgresDB(t *testing.T) *sqlx.DB {
	t.Helper()
	admin := localPostgresTestDB(t)
	schema := "registration_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg, err := pgx.ParseConfig(os.Getenv("ISUREAL_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE users (
 id text PRIMARY KEY, username text NOT NULL, secret_key_hash text NOT NULL,
 display_name text NOT NULL, bio text NOT NULL DEFAULT '', profile_image_id text,
 region text NOT NULL, account_type text NOT NULL DEFAULT 'personal', status text NOT NULL DEFAULT 'active');
 CREATE UNIQUE INDEX arbitrary_username_index ON users(username)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestConcurrentRegistrationPostgres(t *testing.T) {
	db := registrationPostgresDB(t)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/register", registerHandler(&HandlerContext{DB: db}))
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			req := httptest.NewRequest("POST", "/register", strings.NewReader(`{"username":"alice","displayName":"Alice","region":"asia"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			responses <- rec
		}()
	}
	close(start)
	statuses := map[int]int{}
	for range 2 {
		select {
		case rec := <-responses:
			statuses[rec.Code]++
			if rec.Code == 409 && strings.TrimSpace(rec.Body.String()) != `{"error":"username_taken"}` {
				t.Fatal(rec.Body.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent registration timed out")
		}
	}
	if statuses[201] != 1 || statuses[409] != 1 {
		t.Fatalf("statuses=%v", statuses)
	}
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM users WHERE username='alice'`); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestRegistrationPostingWindowBoundaryPostgres(t *testing.T) {
	db := registrationPostgresDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	begin := func() *sqlx.Tx {
		tx, err := db.BeginTxx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback() })
		return tx
	}
	insertSQL := `INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES($1,$1,'hash','Name','asia')`
	registration := begin()
	if _, err := registration.ExecContext(ctx, insertSQL, "before"); err != nil {
		t.Fatal(err)
	}
	opener := begin()
	if _, err := opener.ExecContext(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err := opener.ExecContext(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("opener must wait for registration: %v", err)
	}
	opener.Rollback()
	if err := registration.Commit(); err != nil {
		t.Fatal(err)
	}
	opener = begin()
	if _, err := opener.ExecContext(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var count int
	// Same predicate as the opener's notification INSERT SELECT.
	if err := opener.GetContext(ctx, &count, `SELECT count(*) FROM users WHERE region='asia' AND status='active'`); err != nil || count != 1 {
		t.Fatalf("notification targets=%d err=%v", count, err)
	}
	registration = begin()
	if _, err := registration.ExecContext(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = registration.ExecContext(ctx, insertSQL, "after")
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("registration must wait for opener: %v", err)
	}
	registration.Rollback()
	if err := opener.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, insertSQL, "after"); err != nil {
		t.Fatal(err)
	}
}
