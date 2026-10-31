package isureal

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type postFixture struct {
	active, deleted, queries, writes int
	deadline, starts                 time.Time
	post                             Post
	committed, rolledBack            bool
	commitErr                        bool
}
type postConnector struct{ f *postFixture }

func (c postConnector) Connect(context.Context) (driver.Conn, error) { return postConn{c.f}, nil }
func (postConnector) Driver() driver.Driver                          { return imageDriver{} }

type postConn struct{ f *postFixture }

func (postConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unexpected prepare") }
func (postConn) Close() error                        { return nil }
func (c postConn) Begin() (driver.Tx, error)         { return c, nil }
func (c postConn) Commit() error {
	if c.f.commitErr {
		return errors.New("commit failed")
	}
	c.f.committed = true
	return nil
}
func (c postConn) Rollback() error { c.f.rolledBack = true; return nil }
func (c postConn) QueryContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	f := c.f
	f.queries++
	switch {
	case strings.Contains(q, "FOR NO KEY UPDATE"):
		return &imageRows{columns: []string{"id"}, values: []driver.Value{"user"}}, nil
	case strings.Contains(q, "FROM users"):
		return &imageRows{columns: userColumns, values: []driver.Value{"user", "alice", strings.Repeat("00", 32), "Alice", "bio", nil, "asia", "normal", "active"}}, nil
	case q == activePostingWindowQuery && strings.Contains(q, "FOR SHARE"):
		return &imageRows{columns: []string{"id", "local_date", "starts_at", "on_time_deadline"}, values: []driver.Value{"window", f.starts, f.starts, f.deadline}}, nil
	case strings.Contains(q, "count(*) FILTER"):
		return &imageRows{columns: []string{"active", "deleted"}, values: []driver.Value{int64(f.active), int64(f.deleted)}}, nil
	default:
		return nil, errors.New("unexpected SELECT: " + q)
	}
}
func (c postConn) ExecContext(_ context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	f := c.f
	f.writes++
	if strings.Contains(q, "INSERT INTO posts") {
		// Reproduce the old getPost result using pgx's actual timestamp codec.
		codec := pgtype.TimestamptzCodec{}
		buf, err := codec.PlanEncode(nil, 0, pgtype.BinaryFormatCode, pgtype.Timestamptz{}).Encode(pgtype.Timestamptz{Time: a[10].Value.(time.Time), Valid: true}, nil)
		if err != nil {
			return nil, err
		}
		var stamp pgtype.Timestamptz
		if err := codec.PlanScan(nil, 0, pgtype.BinaryFormatCode, &stamp).Scan(buf, &stamp); err != nil {
			return nil, err
		}
		f.post = Post{ID: a[0].Value.(string), AuthorID: a[1].Value.(string), PostingWindowID: a[2].Value.(string), ImageID: a[3].Value.(string), Caption: a[4].Value.(string), Visibility: a[5].Value.(string), PostedAt: stamp.Time, OnTimeDeadline: f.deadline}
		f.post.Kind = a[6].Value.(string)
		if a[7].Value != nil {
			v := a[7].Value.(float64)
			f.post.Latitude = &v
		}
		if a[8].Value != nil {
			v := a[8].Value.(float64)
			f.post.Longitude = &v
		}
		if a[9].Value != nil {
			v := a[9].Value.(string)
			f.post.Locality = &v
		}
	} else if !strings.Contains(q, "INSERT INTO image_objects") {
		return nil, errors.New("unexpected write")
	}
	return driver.RowsAffected(1), nil
}

func TestCreatePost(t *testing.T) {
	for _, tc := range []struct {
		name, kind              string
		active, deleted         int
		late, future, commitErr bool
	}{
		{name: "none on time", kind: "none"},
		{name: "exact late", kind: "exact", late: true},
		{name: "approximate on time", kind: "approximate", deleted: 1},
		{name: "active post", kind: "none", active: 1},
		{name: "two deletions", kind: "none", deleted: 2},
		{name: "future window", kind: "none", future: true},
		{name: "commit failure", kind: "none", commitErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			f := &postFixture{active: tc.active, deleted: tc.deleted, starts: now.Add(-time.Hour), deadline: now.Add(time.Hour), commitErr: tc.commitErr}
			if tc.late {
				f.deadline = now.Add(-2500 * time.Millisecond)
			}
			if tc.future {
				f.starts = now.Add(time.Hour)
			}
			db := sql.OpenDB(postConnector{f})
			defer db.Close()
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			defer peer.Close()
			h := &HandlerContext{DB: sqlx.NewDb(db, "fixture"), ImageFiles: &ImageFileStore{Root: t.TempDir(), Peers: []string{peer.URL}, client: peer.Client()}}
			defer h.ImageFiles.Close()
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			part, err := mw.CreateFormFile("image", "post.jpg")
			if err != nil {
				t.Fatal(err)
			}
			if err := jpeg.Encode(part, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
				t.Fatal(err)
			}
			for k, v := range map[string]string{"caption": "hello 日本語", "visibility": "public", "location_kind": tc.kind} {
				if err := mw.WriteField(k, v); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kind == "exact" {
				mw.WriteField("latitude", "35.123456789")
				mw.WriteField("longitude", "139.987654321")
			}
			if tc.kind == "approximate" {
				mw.WriteField("locality", "東京都")
			}
			mw.Close()
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.POST("/api/posts", func(c *echo.Context) error {
				c.Set("session", &Session{UserID: "user"})
				return createPostHandler(h)(c)
			})
			req := httptest.NewRequest(http.MethodPost, "/api/posts", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if tc.active != 0 || tc.deleted >= 2 || tc.future {
				if rec.Code != 409 || f.writes != 0 || !f.rolledBack {
					t.Fatalf("limit: status=%d fixture=%+v", rec.Code, f)
				}
				return
			}
			if tc.commitErr {
				if rec.Code != 500 || f.committed {
					t.Fatalf("commit failure: %d", rec.Code)
				}
				return
			}
			if rec.Code != 201 || !f.committed || f.queries != 4 || f.writes != 2 {
				t.Fatalf("status=%d body=%s fixture=%+v", rec.Code, rec.Body, f)
			}
			user := User{ID: "user", Username: "alice", DisplayName: "Alice", Bio: "bio", AccountType: "normal", ImageKey: make([]byte, 32)}
			want, err := json.Marshal(newPostResponse(f.post, user))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(rec.Body.String()) != string(want) {
				t.Fatalf("response differs from DB round trip:\ngot %s\nwant %s", rec.Body, want)
			}
			if f.post.PostedAt.Nanosecond()%1000 != 0 {
				t.Fatal("timestamp loses precision in database")
			}
		})
	}
}

func TestPostResponseDeadline(t *testing.T) {
	deadline := time.Date(2026, 10, 31, 12, 0, 0, 123456000, time.FixedZone("JST", 9*3600))
	for _, tc := range []struct {
		delta  time.Duration
		onTime bool
		late   int64
	}{
		{-time.Microsecond, true, 0}, {0, true, 0}, {time.Microsecond, false, 0}, {time.Second - time.Microsecond, false, 0}, {time.Second, false, 1}, {2500 * time.Millisecond, false, 2},
	} {
		post := Post{PostedAt: deadline.Add(tc.delta).UTC(), OnTimeDeadline: deadline}
		got := newPostResponse(post, User{ImageKey: make([]byte, 32)})
		if got.OnTime != tc.onTime || got.LateSeconds != tc.late {
			t.Fatalf("delta %s: %+v", tc.delta, got)
		}
	}
}

func TestPostTimestampMatchesPGX(t *testing.T) {
	for _, zone := range []*time.Location{time.UTC, time.FixedZone("JST", 9*3600), time.FixedZone("west", -7*3600)} {
		for _, nanos := range []int{123456001, 123456999, 999999999} {
			original := time.Date(2026, 10, 31, 12, 0, 0, nanos, zone)
			codec := pgtype.TimestamptzCodec{}
			value := pgtype.Timestamptz{Time: original, Valid: true}
			buf, err := codec.PlanEncode(nil, 0, pgtype.BinaryFormatCode, value).Encode(value, nil)
			if err != nil {
				t.Fatal(err)
			}
			var stored pgtype.Timestamptz
			if err := codec.PlanScan(nil, 0, pgtype.BinaryFormatCode, &stored).Scan(buf, &stored); err != nil {
				t.Fatal(err)
			}
			got, _ := original.Truncate(time.Microsecond).In(time.Local).MarshalJSON()
			want, _ := stored.Time.MarshalJSON()
			if !bytes.Equal(got, want) {
				t.Fatalf("zone=%s nanos=%d got=%s want=%s", zone, nanos, got, want)
			}
		}
	}
}
