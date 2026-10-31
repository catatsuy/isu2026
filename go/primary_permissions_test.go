package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestReplicaPayloadUsesCurrentPrimaryVisibility(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		visibility                     string
		exists, ended, friend, blocked bool
		want                           int
	}{
		{name: "public became private", visibility: "friends", exists: true, want: 0},
		{name: "current friend allowed", visibility: "friends", exists: true, friend: true, want: 1},
		{name: "post deleted", exists: false, want: 0},
		{name: "window ended", visibility: "public", exists: true, ended: true, want: 0},
		{name: "blocked", visibility: "public", exists: true, blocked: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rows [][]driver.Value
			if tc.exists {
				var ended driver.Value
				if tc.ended {
					ended = time.Now()
				}
				rows = [][]driver.Value{{"post", "author", tc.visibility, ended, "regular", false}}
			}
			db := feedTestDB(t, []feedQuery{{contains: "p.id=ANY", args: []any{[]string{"post"}, "viewer"}, columns: []string{"id", "author_id", "visibility", "window_ended_at", "account_type", "following"}, rows: rows}})
			viewer := viewerContext{User: User{ID: "viewer"}, HasPostedToday: true, Friends: map[string]bool{"author": tc.friend}, Blocked: map[string]bool{"author": tc.blocked}}
			stale := []PostWithAuthor{{Post: Post{ID: "post", AuthorID: "author", Visibility: "public"}}}
			h := &HandlerContext{PrimaryDB: db, ReplicaDB: &sqlx.DB{}}
			c := echo.New().NewContext(httptest.NewRequest("GET", "/api/posts/post", nil), httptest.NewRecorder())
			c.Set("usingReplica", true)
			filtered, err := h.authorizePosts(c, viewer, stale, "")
			if err != nil || len(filtered) != tc.want {
				t.Fatalf("len=%d err=%v", len(filtered), err)
			}
			if len(filtered) > 0 && filtered[0].Visibility != tc.visibility {
				t.Fatal("stale visibility returned")
			}
		})
	}
}

// Explicit opt-in; runs only read-only SELECTs against the application schema.
func TestPrimaryViewerBatchEquivalent(t *testing.T) {
	if os.Getenv("ISUREAL_VALIDATE_PRIMARY") != "true" {
		t.Skip("opt-in read-only Primary validation")
	}
	db, err := sqlx.Connect("pgx", "postgres:///isureal?host=/var/run/postgresql&user=postgres&default_transaction_read_only=on&default_query_exec_mode=exec")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var ids []string
	err = tx.SelectContext(ctx, &ids, `SELECT u.id FROM users u WHERE u.status='active' AND EXISTS(SELECT FROM posts p JOIN posting_windows w ON w.id=p.posting_window_id JOIN posting_windows current ON current.region=u.region AND current.ended_at IS NULL AND current.local_date=w.local_date WHERE p.author_id=u.id AND p.deleted_at IS NULL) LIMIT 20`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) == 0 {
		t.Fatal("no posted viewers available for validation")
	}
	for _, id := range ids {
		old, err := loadViewerContext(ctx, tx, id)
		if err != nil {
			t.Fatal(err)
		}
		batched, err := loadPrimaryViewer(ctx, tx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(old, batched) {
			t.Fatalf("Primary permission batch differs for viewer %s", id)
		}
	}
	t.Logf("validated %d viewers in one read-only snapshot", len(ids))
}

func TestPrimaryMemoriesReadOnly(t *testing.T) {
	if os.Getenv("ISUREAL_VALIDATE_PRIMARY") != "true" {
		t.Skip("opt-in read-only Primary validation")
	}
	db, err := sqlx.Connect("pgx", "postgres:///isureal?host=/var/run/postgresql&user=postgres&default_transaction_read_only=on&default_query_exec_mode=exec")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ids []string
	if err = db.SelectContext(ctx, &ids, `SELECT id FROM users WHERE username IN('soba_smooth_rust_35','koivyben_5224','suredan.5029','bensafu5219','elrashi.5164')`); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range ids {
		started := time.Now()
		data, err := listMemories(ctx, db, id, start, start.AddDate(0, 1, 0), nil, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("memories rows=%d elapsed=%s", len(data.Items), time.Since(started))
	}
}
