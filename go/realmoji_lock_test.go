package isureal

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestConcurrentRealMojiReplacementPostgres(t *testing.T) {
	db := registrationPostgresDB(t)
	// No UNIQUE constraint on current reactions: serialization must come from
	// beginRelationshipTx, and both requests should create a historical record.
	_, err := db.Exec(`
 CREATE TABLE posting_windows(id text PRIMARY KEY, region text, local_date date, on_time_deadline timestamptz, ended_at timestamptz);
 CREATE TABLE posts(id text PRIMARY KEY,author_id text,posting_window_id text,image_id text,caption text,visibility text,location_kind text,latitude double precision,longitude double precision,locality text,posted_at timestamptz,deleted_at timestamptz);
 CREATE TABLE image_objects(id text PRIMARY KEY,owner_user_id text,data bytea,media_type text);
 CREATE TABLE realmojis(id text PRIMARY KEY,post_id text,reactor_id text,image_id text,reaction_type text,created_at timestamptz,replaced_at timestamptz,deleted_at timestamptz);
 CREATE TABLE block_relations(blocker_id text,blocked_id text);
 CREATE TABLE friendships(user_low_id text,user_high_id text);`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES('actor','actor',$1,'Actor','asia'),('author','author',$1,'Author','asia');
 `, strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO posting_windows VALUES('window','asia',CURRENT_DATE,now()+interval '1 hour',NULL);
 INSERT INTO posts(id,author_id,posting_window_id,image_id,caption,visibility,location_kind,posted_at) VALUES('post','author','window','original','','public','none',now()),('viewer-post','actor','window','viewer-image','','public','none',now())`)
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer peer.Close()
	h := &HandlerContext{DB: db, ImageFiles: &ImageFileStore{Root: t.TempDir(), Peers: []string{peer.URL}, client: peer.Client()}}
	defer h.ImageFiles.Close()
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.POST("/posts/:postId/realmojis", func(c *echo.Context) error {
		c.Set("session", &Session{UserID: "actor"})
		return replaceRealMojiHandler(h)(c)
	})
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
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 2 {
		go func() {
			<-start
			req := httptest.NewRequest("POST", "/posts/post/realmojis", bytes.NewReader(body.Bytes())).WithContext(ctx)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			responses <- rec
		}()
	}
	close(start)
	for range 2 {
		select {
		case rec := <-responses:
			if rec.Code != 201 {
				t.Fatalf("status=%d %s", rec.Code, rec.Body)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var counts struct {
		Total   int
		Current int
	}
	if err := db.Get(&counts, `SELECT count(*) AS total,count(*) FILTER(WHERE replaced_at IS NULL AND deleted_at IS NULL) AS current FROM realmojis WHERE post_id='post' AND reactor_id='actor'`); err != nil {
		t.Fatal(err)
	}
	if counts.Total != 2 || counts.Current != 1 {
		t.Fatalf("lost serialization: %+v", counts)
	}
}
