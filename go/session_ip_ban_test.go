package isureal

import (
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestCombinedPrimaryAuthPreservesBanAndInvalidSession(t *testing.T) {
	hash := sha256.Sum256([]byte("token"))
	tokenHash := hex.EncodeToString(hash[:])
	for _, tc := range []struct {
		name           string
		banned, active bool
		status         int
	}{
		{"active", false, true, 204}, {"inactive or deleted", false, false, 401},
		{"banned active", true, true, 403}, {"banned invalid token", true, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := []driver.Value{tc.banned, nil, nil, nil, nil}
			if tc.active {
				row = []driver.Value{tc.banned, "user", tokenHash, time.Now(), "europe"}
			}
			db := feedTestDB(t, []feedQuery{{contains: "LEFT JOIN LATERAL", args: []any{tokenHash, "192.0.2.1"}, columns: []string{"banned", "user_id", "token_hash", "created_at", "region"}, rows: [][]driver.Value{row}}})
			h := &HandlerContext{PrimaryDB: db, Config: &Config{Region: "europe"}, IPBans: &IPBanList{}}
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.IPExtractor = extractClientIP
			e.Use(IPBanMiddleware(h))
			e.GET("/api/me", func(c *echo.Context) error {
				if GetSession(c) == nil {
					t.Fatal("active Primary session missing")
				}
				return c.NoContent(204)
			}, SessionMiddleware(h, true))
			req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
			req.Header.Set("Authorization", "Bearer token")
			req.Header.Set("X-Client-IP", "192.0.2.1")
			res := httptest.NewRecorder()
			e.ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status=%d body=%s", res.Code, res.Body)
			}
		})
	}
}

func TestAsiaRetainsOriginalAuthQueries(t *testing.T) {
	hash := sha256.Sum256([]byte("token"))
	tokenHash := hex.EncodeToString(hash[:])
	db := feedTestDB(t, []feedQuery{
		{contains: "SELECT ip, banned_until", columns: []string{"ip", "banned_until"}, args: []any{}},
		{contains: "FROM login_sessions s JOIN users", args: []any{tokenHash}, columns: []string{"user_id", "token_hash", "created_at", "region"}, rows: [][]driver.Value{{"user", tokenHash, time.Now(), "asia"}}},
	})
	h := &HandlerContext{PrimaryDB: db, Config: &Config{Region: "asia"}, IPBans: &IPBanList{}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.Use(IPBanMiddleware(h))
	e.GET("/api/me", func(c *echo.Context) error { return c.NoContent(204) }, SessionMiddleware(h, true))
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.Header.Set("Authorization", "Bearer token")
	res := httptest.NewRecorder()
	e.ServeHTTP(res, req)
	if res.Code != 204 {
		t.Fatal(res.Code, res.Body)
	}
}
