package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestAsiaForwardPreservesBodyIPAndRegion(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	h := &HandlerContext{Config: &Config{Region: "asia"}, ImageFiles: &ImageFileStore{token: token}}
	asia := echo.New()
	asia.IPExtractor = extractClientIP
	asia.HTTPErrorHandler = APIErrorHandler
	asia.Use(AsiaRoutingMiddleware(h))
	writes := 0
	asia.POST("/api/posts", func(c *echo.Context) error {
		if c.RealIP() != "203.0.113.7" {
			t.Error("original IP lost")
		}
		body, _ := io.ReadAll(c.Request().Body)
		if string(body) != "multipart-test-data" {
			t.Error("body changed")
		}
		c.Set("session", &Session{Region: "europe", UserID: "user"})
		return RegionMiddleware(h)(func(c *echo.Context) error { writes++; return c.JSON(201, map[string]string{"id": "created"}) })(c)
	})
	backend := httptest.NewServer(asia)
	defer backend.Close()
	client, err := newPrimaryAPIClient(backend.URL, "europe", token)
	if err != nil {
		t.Fatal(err)
	}
	foreign := echo.New()
	foreign.HTTPErrorHandler = APIErrorHandler
	foreign.IPExtractor = extractClientIP
	foreign.Use(AsiaRoutingMiddleware(&HandlerContext{Config: &Config{Region: "europe"}, PrimaryAPI: client}))
	foreign.POST("/api/posts", func(c *echo.Context) error { t.Fatal("foreign write handler executed"); return nil })
	req := httptest.NewRequest("POST", "/api/posts", bytes.NewBufferString("multipart-test-data"))
	req.Header.Set("X-Client-IP", "203.0.113.7")
	res := httptest.NewRecorder()
	foreign.ServeHTTP(res, req)
	if res.Code != 201 || writes != 1 {
		t.Fatalf("status=%d writes=%d body=%s", res.Code, writes, res.Body)
	}
	if h.Config.Region != "asia" {
		t.Fatal("fixed server region changed")
	}
}

func TestPrimarySignatureRejectsTamperingAndReplay(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	h := &HandlerContext{Config: &Config{Region: "asia"}, ImageFiles: &ImageFileStore{token: token}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.Use(AsiaRoutingMiddleware(h))
	calls := 0
	e.POST("/api/posts", func(c *echo.Context) error { calls++; return c.NoContent(204) })
	makeRequest := func(body string) *http.Request {
		req := httptest.NewRequest("POST", "/api/posts", bytes.NewBufferString(body))
		req.Header.Set(primaryRegionHeader, "europe")
		req.Header.Set(primaryTimeHeader, strconv.FormatInt(time.Now().Unix(), 10))
		req.Header.Set(primaryNonceHeader, "01234567890123456789012345678901")
		req.Header.Set(primarySignatureHeader, primaryRequestMAC(primaryAPIKey(token), req, []byte("original")))
		return req
	}
	res := httptest.NewRecorder()
	e.ServeHTTP(res, makeRequest("changed"))
	if res.Code != 403 || calls != 0 {
		t.Fatal("tampered body accepted")
	}
	res = httptest.NewRecorder()
	e.ServeHTTP(res, makeRequest("original"))
	if res.Code != 204 || calls != 1 {
		t.Fatal("valid signature rejected", res.Code)
	}
	res = httptest.NewRecorder()
	e.ServeHTTP(res, makeRequest("original"))
	if res.Code != 403 || calls != 1 {
		t.Fatal("replayed mutation executed")
	}
}

func TestPrimaryAPIAuthenticationPreservesSessionRegion(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	hash := sha256.Sum256([]byte("user-token"))
	hashText := hex.EncodeToString(hash[:])
	db := feedTestDB(t, []feedQuery{{contains: "LEFT JOIN LATERAL", args: []any{hashText, "203.0.113.7"}, columns: []string{"banned", "user_id", "token_hash", "created_at", "region"}, rows: [][]driver.Value{{false, "user", hashText, time.Now(), "europe"}}}})
	h := &HandlerContext{Config: &Config{Region: "asia"}, PrimaryDB: db, IPBans: &IPBanList{}, ImageFiles: &ImageFileStore{token: token}}
	e := echo.New()
	e.IPExtractor = extractClientIP
	e.HTTPErrorHandler = APIErrorHandler
	e.Use(AsiaRoutingMiddleware(h))
	e.POST(primaryRPCPath, primaryRPCHandler(h))
	backend := httptest.NewServer(e)
	defer backend.Close()
	client, err := newPrimaryAPIClient(backend.URL, "europe", token)
	if err != nil {
		t.Fatal(err)
	}
	f := &HandlerContext{Config: &Config{Region: "europe"}, PrimaryAPI: client}
	frontend := echo.New()
	frontend.IPExtractor = extractClientIP
	frontend.HTTPErrorHandler = APIErrorHandler
	frontend.GET("/api/me", func(c *echo.Context) error {
		if err := f.authenticateViaAPI(c); err != nil {
			return err
		}
		if GetSession(c) == nil || GetSession(c).Region != "europe" {
			t.Fatal("Primary session region lost over JSON")
		}
		return c.NoContent(204)
	})
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer user-token")
	req.Header.Set("X-Client-IP", "203.0.113.7")
	response := httptest.NewRecorder()
	frontend.ServeHTTP(response, req)
	if response.Code != 204 {
		t.Fatal(response.Code, response.Body)
	}
}

func TestPrimaryRPCRejectsUnsignedResponses(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(primaryResponseHeader, "wrong")
		io.WriteString(w, `{"data":true}`)
	}))
	defer backend.Close()
	client, err := newPrimaryAPIClient(backend.URL, "europe", bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	if err = client.call(context.Background(), "", "", primaryRPCRequest{Op: "ready"}, nil); err == nil {
		t.Fatal("unauthenticated response accepted")
	}
}
