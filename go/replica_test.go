package isureal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestReplicaReadAndWriteSelection(t *testing.T) {
	primary := &sqlx.DB{}
	replica := &sqlx.DB{}
	router := &replicaRouter{primary: primary, replica: replica}
	h := &HandlerContext{DB: primary, PrimaryDB: primary, ReplicaDB: replica, Replication: router}
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api/me", nil), httptest.NewRecorder())
	if profilePool(h, c) != primary {
		t.Fatal("startup must fail closed to Primary")
	}
	router.healthyAt.Store(time.Now().UnixNano())
	if profilePool(h, c) != replica {
		t.Fatal("healthy GET must use local replica")
	}
	c.SetRequest(httptest.NewRequest(http.MethodPost, "/api/posts", nil))
	if profilePool(h, c) != primary || h.WriteDB() != primary {
		t.Fatal("mutation reads and writes must use Primary")
	}
	router.healthyAt.Store(time.Now().Add(-3 * time.Second).UnixNano())
	c.SetRequest(httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if profilePool(h, c) != primary {
		t.Fatal("expired health must fail closed")
	}
	router.healthyAt.Store(0)
	if profilePool(h, c) != primary {
		t.Fatal("failed health must fail closed")
	}
}

func TestRequestReadPoolDoesNotAffectWrites(t *testing.T) {
	primary := &sqlx.DB{}
	snapshot := &sqlx.DB{}
	h := &HandlerContext{PrimaryDB: primary}
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/api/me", nil), httptest.NewRecorder())
	c.Set("readDB", snapshot)
	if h.ReadDB(c) != snapshot {
		t.Fatal("read helper must use the selected request pool")
	}
	if h.WriteDB() != primary {
		t.Fatal("read pool must not affect write pool")
	}
}

func profilePool(h *HandlerContext, c *echo.Context) *sqlx.DB {
	pool, _ := h.dataPool(c)
	return pool
}
func TestSensitivePermissionsAlwaysUsePrimary(t *testing.T) {
	primary, replica := &sqlx.DB{}, &sqlx.DB{}
	router := &replicaRouter{replica: replica}
	router.healthyAt.Store(time.Now().UnixNano())
	h := &HandlerContext{PrimaryDB: primary, Replication: router}
	for _, path := range []string{"/api/posts/post", "/api/images/image", "/api/users/user", "/api/relationships/friends", "/api/notifications"} {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, path, nil), httptest.NewRecorder())
		pool, local := h.dataPool(c)
		if local || pool != primary || h.WriteDB() != primary {
			t.Fatalf("sensitive route must use Primary: %s", path)
		}
	}
}

func TestRoutingAddsNoDatabaseRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		path   string
		region string
		local  bool
	}{
		{"/api/me", "asia", false}, {"/api/users", "asia", false},
		{"/api/feeds/:feed", "europe", true}, {"/api/me", "europe", true},
	} {
		t.Run(tc.region+tc.path, func(t *testing.T) {
			primary, replica := &sqlx.DB{}, &sqlx.DB{}
			h := &HandlerContext{PrimaryDB: primary}
			if tc.region != "asia" {
				h.Replication = &replicaRouter{replica: replica}
				h.Replication.healthyAt.Store(time.Now().UnixNano())
			}
			e := echo.New()
			e.Use(ReadRoutingMiddleware(h))
			e.GET(tc.path, func(c *echo.Context) error {
				want := primary
				if tc.local {
					want = replica
				}
				if h.ReadDB(c) != want {
					t.Fatal("wrong read pool")
				}
				return c.NoContent(http.StatusNoContent)
			})
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, strings.ReplaceAll(tc.path, ":feed", "friends"), nil))
			// The pools have no underlying connection. Any BEGIN/Ping/query in the
			// routing middleware would fail this test before reaching the handler.
			if rec.Code != http.StatusNoContent {
				t.Fatal(rec.Code)
			}
		})
	}
}
