package isureal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// This fixture checks the SQL contract and transaction boundaries. PostgreSQL
// row locking and concurrent transactions require separate integration testing.
type loginFixture struct {
	username, secret      string
	t                     *testing.T
	missing               bool
	failInsert            bool
	queries               int
	committed, rolledBack bool
	tokenHash             string
	createdAt             time.Time
}
type loginConnector struct{ f *loginFixture }

func (c loginConnector) Connect(context.Context) (driver.Conn, error) { return loginConn{c.f}, nil }
func (c loginConnector) Driver() driver.Driver                        { return loginDriver{} }

type loginDriver struct{}

func (loginDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type loginConn struct{ f *loginFixture }

func (c loginConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }
func (c loginConn) Close() error                        { return nil }
func (c loginConn) Begin() (driver.Tx, error)           { return c, nil }
func (c loginConn) Commit() error                       { c.f.committed = true; return nil }
func (c loginConn) Rollback() error                     { c.f.rolledBack = true; return nil }
func (c loginConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.f
	f.queries++
	switch f.queries {
	case 1:
		if !strings.Contains(query, "username = $1 AND secret_key_hash = $2 AND status = 'active' FOR NO KEY UPDATE") {
			f.t.Fatalf("authentication must filter credentials and status before locking: %s", query)
		}
		hash := sha256.Sum256([]byte(f.secret))
		if len(args) != 2 || args[0].Value != f.username || args[1].Value != hex.EncodeToString(hash[:]) {
			f.t.Fatalf("unexpected auth args: %v", args)
		}
		r := &loginRows{columns: []string{"id", "username", "display_name", "bio", "region", "account_type", "status"}}
		if !f.missing {
			r.values = []driver.Value{"user-id", "alice", "Alice", "bio", "asia", "regular", "active"}
		}
		return r, nil
	case 3:
		if !strings.Contains(query, "INSERT INTO login_sessions") || !strings.Contains(query, "RETURNING user_id, token_hash, created_at") {
			f.t.Fatalf("unexpected insert: %s", query)
		}
		if len(args) != 3 || args[1].Value != "user-id" {
			f.t.Fatalf("unexpected insert args: %v", args)
		}
		if _, err := uuid.Parse(args[0].Value.(string)); err != nil {
			f.t.Fatal(err)
		}
		if f.failInsert {
			return nil, errors.New("insert failed")
		}
		f.tokenHash = args[2].Value.(string)
		return &loginRows{columns: []string{"user_id", "token_hash", "created_at"}, values: []driver.Value{"user-id", f.tokenHash, f.createdAt}}, nil
	default:
		f.t.Fatalf("unexpected query %d: %s", f.queries, query)
		return nil, errors.New("unexpected query")
	}
}
func (c loginConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	f := c.f
	f.queries++
	if f.queries != 2 || !strings.Contains(query, "UPDATE login_sessions") || !strings.Contains(query, "reason = 'replaced'") || !strings.Contains(query, "WHERE user_id = $1 AND active = true") || len(args) != 1 || args[0].Value != "user-id" {
		f.t.Fatalf("unexpected invalidation: %s, %v", query, args)
	}
	return driver.RowsAffected(1), nil
}

type loginRows struct {
	columns []string
	values  []driver.Value
}

func (r *loginRows) Columns() []string { return r.columns }
func (r *loginRows) Close() error      { return nil }
func (r *loginRows) Next(dest []driver.Value) error {
	if r.values == nil {
		return io.EOF
	}
	copy(dest, r.values)
	r.values = nil
	return nil
}

func TestLoginTransaction(t *testing.T) {
	for _, name := range []string{"success", "invalid secret", "unknown user", "deleted user", "insert failure"} {
		t.Run(name, func(t *testing.T) {
			// Each rejected credential/state combination is represented by the empty
			// result PostgreSQL returns for the verified authentication WHERE clause.
			f := &loginFixture{t: t, missing: name != "success" && name != "insert failure", failInsert: name == "insert failure", createdAt: time.Unix(123, 456).UTC()}
			f.username, f.secret = "alice", "secret"
			if name == "invalid secret" {
				f.secret = "wrong-secret"
			}
			if name == "unknown user" {
				f.username = "unknown"
			}
			db := sql.OpenDB(loginConnector{f})
			defer db.Close()
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.POST("/api/auth/login", loginHandler(&HandlerContext{DB: sqlx.NewDb(db, "fixture")}))
			body, err := json.Marshal(loginRequest{Username: f.username, SecretKey: f.secret})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(string(body)))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			res := httptest.NewRecorder()
			e.ServeHTTP(res, req)
			if f.missing {
				if res.Code != http.StatusUnauthorized || strings.TrimSpace(res.Body.String()) != `{"error":"invalid_credentials"}` || f.queries != 1 || f.committed || !f.rolledBack {
					t.Fatalf("unexpected rejection: %d %s %+v", res.Code, res.Body, f)
				}
				return
			}
			if f.failInsert {
				if res.Code != http.StatusInternalServerError || f.committed || !f.rolledBack {
					t.Fatalf("insert failure must roll back: %d %+v", res.Code, f)
				}
				return
			}
			if res.Code != http.StatusOK || f.queries != 3 || !f.committed || f.rolledBack {
				t.Fatalf("unexpected success: %d %s %+v", res.Code, res.Body, f)
			}
			var response loginResponse
			if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if _, err := uuid.Parse(response.Token); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(response.Token))
			if f.tokenHash != hex.EncodeToString(hash[:]) || response.User.ID != "user-id" || response.User.Region != "asia" {
				t.Fatalf("unexpected response: %+v", response)
			}
		})
	}
}

func TestCreateLoginSessionReturning(t *testing.T) {
	f := &loginFixture{t: t, queries: 1, createdAt: time.Unix(123, 456).UTC()}
	db := sql.OpenDB(loginConnector{f})
	defer db.Close()
	tx, err := sqlx.NewDb(db, "fixture").Beginx()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	session, err := createLoginSessionTx(context.Background(), tx, "user-id")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(session.Token))
	if session.UserID != "user-id" || session.TokenHash != hex.EncodeToString(hash[:]) || !session.CreatedAt.Equal(f.createdAt) || session.Region != "" {
		t.Fatalf("unexpected session: %+v", session)
	}
}
