package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

type optimizationFixture struct {
	query                      func(string, []driver.NamedValue) (driver.Rows, error)
	exec                       func(string, []driver.NamedValue) (driver.Result, error)
	begins, commits, rollbacks int
	commitErr                  error
}
type optimizationConnector struct{ f *optimizationFixture }

func (c optimizationConnector) Driver() driver.Driver { return imageDriver{} }
func (c optimizationConnector) Connect(context.Context) (driver.Conn, error) {
	return optimizationConn{c.f}, nil
}

type optimizationConn struct{ f *optimizationFixture }

func (c optimizationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c optimizationConn) Close() error              { return nil }
func (c optimizationConn) Begin() (driver.Tx, error) { c.f.begins++; return c, nil }
func (c optimizationConn) Commit() error             { c.f.commits++; return c.f.commitErr }
func (c optimizationConn) Rollback() error           { c.f.rollbacks++; return nil }
func (c optimizationConn) QueryContext(_ context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	return c.f.query(q, a)
}
func (c optimizationConn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.f.exec(q, a)
}

type optimizationRows struct {
	columns []string
	rows    [][]driver.Value
}

func (r *optimizationRows) Columns() []string { return r.columns }
func (r *optimizationRows) Close() error      { return nil }
func (r *optimizationRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}
func optimizationDB(t *testing.T, f *optimizationFixture) *sqlx.DB {
	t.Helper()
	db := sql.OpenDB(optimizationConnector{f})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return sqlx.NewDb(db, "fixture")
}
func TestRegistrationUniqueViolation(t *testing.T) {
	for _, tc := range []struct {
		name, code, table string
		username          bool
		status            int
	}{
		{"username index", "23505", "users", true, 409}, {"other index", "23505", "users", false, 500}, {"other table", "23505", "other", true, 500}, {"other code", "23503", "users", true, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &optimizationFixture{}
			f.exec = func(q string, _ []driver.NamedValue) (driver.Result, error) {
				if !strings.HasPrefix(q, "INSERT INTO users") {
					t.Fatalf("unexpected SQL: %s", q)
				}
				return nil, fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: tc.code, TableName: tc.table, SchemaName: "public", ConstraintName: "arbitrary_index_name"})
			}
			f.query = func(q string, a []driver.NamedValue) (driver.Rows, error) {
				if f.rollbacks != 1 || !strings.Contains(q, "pg_index") || a[1].Value != "arbitrary_index_name" {
					t.Fatalf("unexpected catalog query: %s", q)
				}
				return &imageRows{columns: []string{"exists"}, values: []driver.Value{tc.username}}, nil
			}
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.POST("/register", registerHandler(&HandlerContext{DB: optimizationDB(t, f)}))
			req := httptest.NewRequest("POST", "/register", strings.NewReader(`{"username":"alice","displayName":"Alice","region":"asia"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.status || f.commits != 0 || f.rollbacks != 1 {
				t.Fatalf("status=%d body=%s fixture=%+v", rec.Code, rec.Body, f)
			}
			if tc.status == 409 && strings.TrimSpace(rec.Body.String()) != `{"error":"username_taken"}` {
				t.Fatal(rec.Body.String())
			}
		})
	}
}
func TestSearchUsersBatches(t *testing.T) {
	for _, total := range []int{0, 50, 51, 256, 2000, 10000} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			calls := 0
			f := &optimizationFixture{}
			f.query = func(q string, a []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(q, "block_relations") {
					return &optimizationRows{columns: []string{"id"}, rows: [][]driver.Value{{"0255"}}}, nil
				}
				if !strings.Contains(q, "ORDER BY username, id LIMIT $4") {
					t.Fatalf("unbounded search: %s", q)
				}
				calls++
				start := 0
				if a[1].Value != "" {
					start, _ = strconv.Atoi(a[2].Value.(string))
					start++
				}
				limit := int(a[3].Value.(int64))
				if limit > 4096 {
					t.Fatalf("unbounded batch: %d", limit)
				}
				r := &optimizationRows{columns: userColumns}
				for i := start; i < min(start+limit, total); i++ {
					name := fmt.Sprintf("%04d", i)
					display := "no match"
					if total <= 51 || (total != 10000 && i >= 250) {
						display = "CAFE\u0301 日本語"
					}
					r.rows = append(r.rows, []driver.Value{name, name, strings.Repeat("00", 32), display, "private bio", nil, "asia", "personal", "active"})
				}
				return r, nil
			}
			db := optimizationDB(t, f)
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.GET("/users", func(c *echo.Context) error {
				c.Set("session", &Session{UserID: "viewer"})
				return searchUsersHandler(&HandlerContext{DB: db})(c)
			})
			fetch := func(cursor string) searchUsersResponse {
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, httptest.NewRequest("GET", "/users?q=caf%C3%A9"+cursor, nil))
				if rec.Code != 200 {
					t.Fatalf("status=%d %s", rec.Code, rec.Body)
				}
				var got searchUsersResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				return got
			}
			got := fetch("")
			matches := total
			if total > 51 {
				matches = max(0, total-250)
				if total > 255 {
					matches--
				}
			}
			if total == 10000 {
				matches = 0
				if calls != 6 {
					t.Fatalf("sparse scan calls=%d", calls)
				}
			}
			if len(got.Items) != min(matches, 50) || got.HasMore != (matches > 50) {
				t.Fatalf("unexpected page: %+v", got)
			}
			for _, u := range got.Items {
				if u.ID == "0255" || u.Bio != "" {
					t.Fatalf("privacy: %+v", u)
				}
			}
			if total == 2000 {
				if calls != 2 {
					t.Fatalf("expected early stop, calls=%d", calls)
				}
				last := got.Items[49]
				next := fetch("&beforeUsername=" + last.Username + "&beforeId=" + last.ID)
				if len(next.Items) != 50 || next.Items[0].Username <= last.Username {
					t.Fatalf("bad next page: %+v", next)
				}
			}
		})
	}
}
