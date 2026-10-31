package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// A tiny database fixture verifies authorization even when the file is cached.
type imageDBFixture struct {
	active     bool
	generation string
	dataReads  int
	queries    int
}
type imageConnector struct{ fixture *imageDBFixture }

func (c imageConnector) Connect(context.Context) (driver.Conn, error) {
	return imageConn{c.fixture}, nil
}
func (c imageConnector) Driver() driver.Driver { return imageDriver{} }

type imageDriver struct{}

func (imageDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type imageConn struct{ fixture *imageDBFixture }

func (imageConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }
func (imageConn) Close() error                        { return nil }
func (imageConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected transaction") }
func (c imageConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	f := c.fixture
	f.queries++
	switch {
	case strings.Contains(query, "SELECT u.secret_key_hash"):
		return &imageRows{columns: []string{"secret_key_hash"}, values: []driver.Value{strings.Repeat("00", 32)}}, nil
	case strings.Contains(query, "SELECT i.media_type"):
		rows := &imageRows{columns: []string{"media_type", "created_at", "generation"}}
		if f.active {
			rows.values = []driver.Value{"image/jpeg", time.Unix(100, 0), f.generation}
		}
		return rows, nil
	case strings.Contains(query, "SELECT data"):
		f.dataReads++
		return &imageRows{columns: []string{"data"}, values: []driver.Value{[]byte("jpeg bytes")}}, nil
	default:
		return nil, errors.New("unexpected query")
	}
}

type imageRows struct {
	columns []string
	values  []driver.Value
}

func (r *imageRows) Columns() []string { return r.columns }
func (r *imageRows) Close() error      { return nil }
func (r *imageRows) Next(dest []driver.Value) error {
	if r.values == nil {
		return io.EOF
	}
	copy(dest, r.values)
	r.values = nil
	return nil
}

func TestImageHandlerAuthorizesCachedImages(t *testing.T) {
	f := &imageDBFixture{active: true, generation: "1"}
	db := sql.OpenDB(imageConnector{f})
	t.Cleanup(func() { db.Close() })
	h := &HandlerContext{DB: sqlx.NewDb(db, "fixture"), Config: &Config{ImageCacheDir: t.TempDir()}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/images/:imageId", getImageHandler(h))
	key, _ := hex.DecodeString(strings.Repeat("00", 32))
	exp := time.Now().Add(time.Hour).Unix()
	url := func(exp int64, sig string) string {
		return "/api/images/image-id?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + sig
	}
	sig := base64.RawURLEncoding.EncodeToString(imageSignature(key, "image-id", exp))
	request := func(target string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		e.ServeHTTP(r, httptest.NewRequest(http.MethodGet, target, nil))
		return r
	}
	first := request(url(exp, sig))
	if first.Code != 200 || first.Header().Get("X-Accel-Redirect") == "" || first.Body.Len() != 0 {
		t.Fatalf("bad redirect: %d %v %q", first.Code, first.Header(), first.Body.String())
	}
	second := request(url(exp, sig))
	if second.Code != 200 || f.dataReads != 1 || f.queries != 5 {
		t.Fatalf("cache hit did not reauthorize or reread bytes: status=%d, reads=%d, queries=%d", second.Code, f.dataReads, f.queries)
	}
	f.active = false
	deleted := request(url(exp, sig))
	if deleted.Code != 403 || deleted.Header().Get("X-Accel-Redirect") != "" {
		t.Fatal("deleted cached image was allowed")
	}
	f.active = true
	for _, target := range []string{url(exp-7200, sig), url(exp, base64.RawURLEncoding.EncodeToString(make([]byte, 32)))} {
		if got := request(target); got.Code != 403 || got.Header().Get("X-Accel-Redirect") != "" {
			t.Fatal("invalid signature or expired URL allowed")
		}
	}
	f.generation = "2"
	next := request(url(exp, sig))
	if next.Code != 200 || next.Header().Get("X-Accel-Redirect") == first.Header().Get("X-Accel-Redirect") || f.dataReads != 2 {
		t.Fatal("generation change reused old image")
	}
}
