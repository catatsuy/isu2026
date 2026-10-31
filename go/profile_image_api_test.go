package isureal

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type profileFixture struct {
	begins, commits, rollbacks int
	failInsert                 bool
}
type profileConnector struct{ f *profileFixture }

func (c profileConnector) Driver() driver.Driver                        { return imageDriver{} }
func (c profileConnector) Connect(context.Context) (driver.Conn, error) { return profileConn{c.f}, nil }

type profileConn struct{ f *profileFixture }

func (c profileConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c profileConn) Close() error              { return nil }
func (c profileConn) Begin() (driver.Tx, error) { c.f.begins++; return profileTx{c.f}, nil }
func (c profileConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if !strings.Contains(query, "FOR NO KEY UPDATE") {
		return nil, errors.New("missing user lock")
	}
	return &imageRows{columns: []string{"profile_image_id"}, values: []driver.Value{"old-image"}}, nil
}
func (c profileConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if c.f.failInsert && strings.Contains(query, "INSERT INTO image_objects") {
		return nil, errors.New("injected insert failure")
	}
	return driver.RowsAffected(1), nil
}

type profileTx struct{ f *profileFixture }

func (tx profileTx) Commit() error   { tx.f.commits++; return nil }
func (tx profileTx) Rollback() error { tx.f.rollbacks++; return nil }

func TestProfileImagePersistsBeforeTransaction(t *testing.T) {
	for _, scenario := range []string{"success", "replication failure", "insert failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := &profileFixture{failInsert: scenario == "insert failure"}
			db := sql.OpenDB(profileConnector{f})
			defer db.Close()
			files := &ImageFileStore{Root: t.TempDir(), token: bytes.Repeat([]byte("k"), 32), client: &http.Client{}}
			// Remote failures must not determine the API response.
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "replication failure" {
					w.WriteHeader(http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer peer.Close()
			files.Peers = []string{peer.URL}
			defer files.Close()
			h := &HandlerContext{DB: sqlx.NewDb(db, "fixture"), ImageFiles: files}
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.PUT("/api/me/profile-image", func(c *echo.Context) error {
				c.Set("session", &Session{UserID: "user"})
				return putProfileImageHandler(h)(c)
			})
			var jpegData bytes.Buffer
			if err := jpeg.Encode(&jpegData, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/api/me/profile-image", &jpegData)
			req.Header.Set("Content-Type", "image/jpeg")
			response := httptest.NewRecorder()
			e.ServeHTTP(response, req)
			switch scenario {
			case "success", "replication failure":
				if response.Code != 204 || f.begins != 1 || f.commits != 1 {
					t.Fatalf("status=%d fixture=%+v", response.Code, f)
				}
			case "insert failure":
				if response.Code != 500 || f.commits != 0 || f.rollbacks != 1 {
					t.Fatalf("failed insert committed: status=%d fixture=%+v", response.Code, f)
				}
			}
		})
	}
}
