package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type feedQuery struct {
	contains string
	args     []any
	columns  []string
	rows     [][]driver.Value
}
type feedFixture struct {
	t       *testing.T
	queries []feedQuery
	calls   int
}
type feedConnector struct{ f *feedFixture }

func (c feedConnector) Connect(context.Context) (driver.Conn, error) { return feedConn{c.f}, nil }
func (c feedConnector) Driver() driver.Driver                        { return loginDriver{} }

type feedConn struct{ f *feedFixture }

func (c feedConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }
func (c feedConn) Close() error                        { return nil }
func (c feedConn) Begin() (driver.Tx, error)           { return nil, errors.New("unexpected transaction") }
func (c feedConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.f
	if f.calls >= len(f.queries) {
		f.t.Fatalf("unexpected SQL: %s", query)
	}
	want := f.queries[f.calls]
	f.calls++
	actual := make([]any, len(args))
	for i, arg := range args {
		actual[i] = arg.Value
	}
	if !strings.Contains(query, want.contains) || !reflect.DeepEqual(actual, want.args) {
		f.t.Fatalf("unexpected SQL/args: %s %v; want %s %v", query, actual, want.contains, want.args)
	}
	return &feedRows{columns: want.columns, rows: want.rows}, nil
}

type feedRows struct {
	columns []string
	rows    [][]driver.Value
}

func (r *feedRows) Columns() []string { return r.columns }
func (r *feedRows) Close() error      { return nil }
func (r *feedRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}
func feedTestDB(t *testing.T, queries []feedQuery) *sqlx.DB {
	t.Helper()
	f := &feedFixture{t: t, queries: queries}
	db := sqlx.NewDb(sql.OpenDB(feedConnector{f}), "fixture")
	t.Cleanup(func() {
		db.Close()
		if f.calls != len(queries) {
			t.Errorf("executed %d queries, want %d", f.calls, len(queries))
		}
	})
	return db
}
func viewerQueries(posted bool) []feedQuery {
	return []feedQuery{
		{contains: "FROM users WHERE id = $1", args: []any{"viewer"}, columns: []string{"id", "region", "secret_key_hash"}, rows: [][]driver.Value{{"viewer", "asia", strings.Repeat("ab", 32)}}},
		{contains: "SELECT EXISTS(", args: []any{"viewer", "asia"}, columns: []string{"exists"}, rows: [][]driver.Value{{posted}}},
	}
}
func TestFeedWithoutPostSkipsRelationshipsAndPosts(t *testing.T) {
	for _, feed := range []string{"friends", "friends-of-friends", "public", "following"} {
		t.Run(feed, func(t *testing.T) {
			db := feedTestDB(t, viewerQueries(false))
			e := echo.New()
			e.GET("/api/feed/:feed", func(c *echo.Context) error {
				c.Set("session", &Session{UserID: "viewer"})
				return getFeedHandler(&HandlerContext{DB: db})(c)
			})
			res := httptest.NewRecorder()
			e.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/feed/"+feed, nil))
			if res.Code != http.StatusOK || strings.TrimSpace(res.Body.String()) != `{"items":[],"hasMore":false}` {
				t.Fatalf("unexpected response: %d %s", res.Code, res.Body)
			}
		})
	}
}
func TestLoadViewerContextStillLoadsRelationshipsWithoutPost(t *testing.T) {
	queries := append(viewerQueries(false),
		feedQuery{contains: "FROM block_relations", args: []any{"viewer"}, columns: []string{"id"}, rows: [][]driver.Value{{"blocked"}}},
		feedQuery{contains: "WHERE user_low_id = $1 OR user_high_id = $1", args: []any{"viewer"}, columns: []string{"user_low_id", "user_high_id"}, rows: [][]driver.Value{{"viewer", "friend"}}},
		feedQuery{contains: "WHERE user_low_id IN ($1) OR user_high_id IN ($2)", args: []any{"friend", "friend"}, columns: []string{"user_low_id", "user_high_id"}, rows: [][]driver.Value{{"friend", "other"}}},
	)
	viewer, err := loadViewerContext(context.Background(), feedTestDB(t, queries), "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if viewer.HasPostedToday || !viewer.Blocked["blocked"] || !viewer.Friends["friend"] || !viewer.FriendsOfFriends["other"] || viewer.MutualFriends["other"] != 1 {
		t.Fatalf("missing viewer data: %+v", viewer)
	}
	if !viewer.canView(Post{AuthorID: "viewer"}) || viewer.canView(Post{AuthorID: "other", Visibility: "public"}) {
		t.Fatal("view permissions changed")
	}
}
func TestRecommendationScoresDeduplicatesAuthors(t *testing.T) {
	db := feedTestDB(t, []feedQuery{
		{contains: "WHERE user_low_id IN ($1, $2) OR user_high_id IN ($3, $4)", args: []any{"author-a", "author-b", "author-a", "author-b"}, columns: []string{"user_low_id", "user_high_id"}, rows: [][]driver.Value{{"author-a", "mutual"}, {"author-b", "mutual"}}},
		{contains: "WHERE r.post_id IN ($1, $2, $3)", args: []any{"post-1", "post-2", "post-3", "post-1", "post-2", "post-3"}, columns: []string{"post_id", "user_id"}, rows: [][]driver.Value{{"post-1", "friend"}, {"post-2", "stranger"}, {"post-3", "friend"}}},
	})
	posts := []PostWithAuthor{{Post: Post{ID: "post-1", AuthorID: "author-a"}}, {Post: Post{ID: "post-2", AuthorID: "author-a"}}, {Post: Post{ID: "post-3", AuthorID: "author-b"}}}
	viewer := viewerContext{Friends: map[string]bool{"friend": true}, MutualFriends: map[string]int{"author-a": 2, "author-b": 1, "mutual": 4}}
	scores, err := recommendationScores(context.Background(), db, viewer, posts)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{"post-1": 20, "post-2": 10, "post-3": 17}; !reflect.DeepEqual(scores, want) {
		t.Fatalf("scores = %v, want %v", scores, want)
	}
	if posts[0].ID != "post-1" || posts[1].ID != "post-2" || posts[2].ID != "post-3" {
		t.Fatal("post order changed")
	}
}

// Match pgx's support for PostgreSQL array arguments while retaining the
// standard conversion of ordinary scalar arguments in this fixture.
func (c feedConn) CheckNamedValue(value *driver.NamedValue) error {
	if _, ok := value.Value.([]string); ok {
		return nil
	}
	return driver.ErrSkip
}
