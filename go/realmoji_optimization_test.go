package isureal

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"github.com/labstack/echo/v5"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealMojiSaveBeforeLocks(t *testing.T) {
	for _, scenario := range []string{"success", "commit failure", "save failure", "insert failure", "account deleted during save", "post deleted during save", "viewer post deleted during save", "blocked during save"} {
		t.Run(scenario, func(t *testing.T) {
			f := &optimizationFixture{}
			if scenario == "commit failure" {
				f.commitErr = errors.New("uncertain commit")
			}
			writes := 0
			saved := false
			imageID := ""
			f.query = func(q string, a []driver.NamedValue) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "FOR NO KEY UPDATE"):
					if !saved {
						t.Fatal("user locked before file save")
					}
					if scenario == "account deleted during save" {
						return &imageRows{columns: []string{"id"}}, nil
					}
					return &imageRows{columns: []string{"id"}, values: []driver.Value{a[0].Value}}, nil
				case strings.Contains(q, "JOIN users u"):
					if f.begins != 1 {
						t.Fatal("visibility not checked in transaction")
					}
					r := &imageRows{columns: []string{"id", "author_id", "visibility", "author.secret_key_hash"}}
					if scenario != "post deleted during save" {
						r.values = []driver.Value{"post", "author", "public", strings.Repeat("00", 32)}
					}
					return r, nil
				case strings.Contains(q, "FROM users"):
					return &imageRows{columns: userColumns, values: []driver.Value{"actor", "alice", strings.Repeat("00", 32), "Alice", "bio", nil, "asia", "personal", "active"}}, nil
				case strings.Contains(q, "SELECT EXISTS"):
					return &imageRows{columns: []string{"exists"}, values: []driver.Value{scenario != "viewer post deleted during save"}}, nil
				case strings.Contains(q, "block_relations"):
					r := &optimizationRows{columns: []string{"id"}}
					if scenario == "blocked during save" {
						r.rows = [][]driver.Value{{"author"}}
					}
					return r, nil
				case strings.Contains(q, "friendships"):
					return &optimizationRows{columns: []string{"user_low_id", "user_high_id"}}, nil
				case strings.Contains(q, "FROM realmojis"):
					return &imageRows{columns: []string{"id", "post_id", "reactor_id", "image_id", "reaction_type", "created_at"}, values: []driver.Value{"reaction", "post", "actor", imageID, "like", time.Unix(123, 0)}}, nil
				case strings.Contains(q, "FROM posts p"):
					return &imageRows{columns: []string{"id", "author_id"}, values: []driver.Value{"post", "author"}}, nil
				default:
					t.Fatalf("unexpected query: %s", q)
					return nil, errors.New("unexpected query")
				}
			}
			f.exec = func(q string, a []driver.NamedValue) (driver.Result, error) {
				writes++
				if f.begins != 1 || !saved {
					t.Fatal("write before save/transaction")
				}
				switch writes {
				case 1:
					if !strings.HasPrefix(q, "UPDATE realmojis SET replaced_at") {
						t.Fatal(q)
					}
				case 2:
					if !strings.HasPrefix(q, "INSERT INTO image_objects") {
						t.Fatal(q)
					}
					imageID = a[0].Value.(string)
					if scenario == "insert failure" {
						return nil, errors.New("injected failure")
					}
				case 3:
					if !strings.HasPrefix(q, "INSERT INTO realmojis") {
						t.Fatal(q)
					}
				default:
					t.Fatal(q)
				}
				return driver.RowsAffected(1), nil
			}
			db := optimizationDB(t, f)
			inUse := make(chan int, 1)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inUse <- db.Stats().InUse
				if scenario == "save failure" {
					w.WriteHeader(500)
				} else {
					w.WriteHeader(204)
				}
			}))
			defer peer.Close()
			// Save completion is observed synchronously by the first lock query. The
			// peer goroutine communicates through the channel, avoiding shared state.
			originalQuery := f.query
			f.query = func(q string, a []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(q, "FOR NO KEY UPDATE") && !saved {
					if n := <-inUse; n != 0 {
						t.Fatalf("save held %d connections", n)
					}
					saved = true
				}
				return originalQuery(q, a)
			}
			root := t.TempDir()
			h := &HandlerContext{DB: db, ImageFiles: &ImageFileStore{Root: root, Peers: []string{peer.URL}, client: peer.Client()}}
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			part, err := mw.CreateFormFile("image", "realmoji.jpg")
			if err != nil {
				t.Fatal(err)
			}
			if err := jpeg.Encode(part, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
				t.Fatal(err)
			}
			mw.WriteField("reactionType", "like")
			mw.Close()
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.POST("/posts/:postId/realmojis", func(c *echo.Context) error {
				c.Set("session", &Session{UserID: "actor"})
				return replaceRealMojiHandler(h)(c)
			})
			req := httptest.NewRequest("POST", "/posts/post/realmojis", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			want := 403
			switch scenario {
			case "success":
				want = 201
			case "save failure", "insert failure", "commit failure":
				want = 500
			case "account deleted during save":
				want = 404
			}
			if rec.Code != want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if scenario == "save failure" {
				if n := <-inUse; n != 0 {
					t.Fatal(n)
				}
				if f.begins != 0 || writes != 0 {
					t.Fatal("save failure touched transaction")
				}
			} else if scenario == "success" {
				if writes != 3 || f.commits != 1 {
					t.Fatalf("fixture=%+v writes=%d", f, writes)
				}
				if !strings.Contains(rec.Body.String(), `"reactionType":"like"`) {
					t.Fatal(rec.Body.String())
				}
			} else if scenario == "commit failure" {
				if f.commits != 1 || writes != 3 {
					t.Fatalf("fixture=%+v writes=%d", f, writes)
				}
			} else {
				if f.commits != 0 || f.rollbacks != 1 {
					t.Fatalf("fixture=%+v", f)
				}
				if scenario != "insert failure" && writes != 0 {
					t.Fatal("state change allowed write")
				}
			}
			if scenario != "save failure" {
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) == 0 {
					t.Fatalf("saved file removed: %v", err)
				}
			}
		})
	}
}
