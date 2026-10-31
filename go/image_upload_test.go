package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

type uploadTransport func(*http.Request) (*http.Response, error)

func (f uploadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func uploadJPEG(t *testing.T, size int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, size, size)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func uploadRequest(t *testing.T, path string, data []byte, fields map[string]string) *http.Request {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	p, err := w.CreateFormFile("image", "test.jpg")
	if err != nil {
		t.Fatal(err)
	}
	p.Write(data)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	w.Close()
	r := httptest.NewRequest("POST", path, &b)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

// Tests the real signed RPC, real transactions and all three unchanged external
// handlers with replication PUT disabled. Opt in using a disposable local DB.
func TestRegionalImageUploadsPostgres(t *testing.T) {
	for _, region := range []string{"europe", "america"} {
		t.Run(region, func(t *testing.T) {
			db := registrationPostgresDB(t)
			_, err := db.Exec(`ALTER TABLE users ADD COLUMN updated_at timestamptz;
 CREATE TABLE login_sessions(user_id text,token_hash text,created_at timestamptz DEFAULT now(),active boolean DEFAULT true);
 CREATE TABLE ip_bans(ip text,banned_until timestamptz);
 CREATE TABLE posting_windows(id text PRIMARY KEY,region text,local_date date,starts_at timestamptz,on_time_deadline timestamptz,ended_at timestamptz);
 CREATE TABLE image_objects(id text PRIMARY KEY,owner_user_id text,data bytea,media_type text,created_at timestamptz);
 CREATE TABLE posts(id text PRIMARY KEY,author_id text,posting_window_id text,image_id text,caption text,visibility text,location_kind text,latitude double precision,longitude double precision,locality text,posted_at timestamptz,deleted_at timestamptz);
 CREATE TABLE realmojis(id text PRIMARY KEY,post_id text,reactor_id text,image_id text,reaction_type text,created_at timestamptz,replaced_at timestamptz,deleted_at timestamptz);
 CREATE TABLE block_relations(blocker_id text,blocked_id text);
 CREATE TABLE friendships(user_low_id text,user_high_id text);`)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte("session-token"))
			if _, err = db.Exec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES('actor','actor',$1,'Actor',$2),('author','author',$1,'Author',$2)`, strings.Repeat("00", 32), region); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO posting_windows VALUES('window',$1,CURRENT_DATE,now()-interval '1 hour',now()+interval '1 hour',NULL)`, region); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO posts VALUES('target','author','window','original','','public','none',NULL,NULL,NULL,now(),NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`INSERT INTO login_sessions(user_id,token_hash) VALUES('actor',$1)`, hex.EncodeToString(hash[:])); err != nil {
				t.Fatal(err)
			}

			token := bytes.Repeat([]byte("k"), 32)
			asiaFiles := &ImageFileStore{Root: t.TempDir(), token: token, client: &http.Client{}}
			localFiles := &ImageFileStore{Root: t.TempDir(), token: token, client: &http.Client{}}
			thirdFiles := &ImageFileStore{Root: t.TempDir(), token: token, client: &http.Client{}}
			defer asiaFiles.Close()
			defer localFiles.Close()
			defer thirdFiles.Close()
			asiaH := &HandlerContext{DB: db, Config: &Config{Region: "asia"}, ImageFiles: asiaFiles, IPBans: &IPBanList{}}
			asia := echo.New()
			asia.HTTPErrorHandler = APIErrorHandler
			asia.IPExtractor = extractClientIP
			asia.Use(AsiaRoutingMiddleware(asiaH))
			asia.POST(primaryRPCPath, primaryRPCHandler(asiaH))
			asia.GET("/internal/image-files/:imageId", asiaFiles.peerHandler())
			asia.PUT("/internal/image-files/:imageId", func(c *echo.Context) error { return c.NoContent(503) })
			back := httptest.NewServer(asia)
			defer back.Close()
			client, err := newPrimaryAPIClient(back.URL, region, token)
			if err != nil {
				t.Fatal(err)
			}
			defer client.client.CloseIdleConnections()
			foreignH := &HandlerContext{Config: &Config{Region: region}, PrimaryAPI: client, ImageFiles: localFiles, IPBans: &IPBanList{}}
			foreign := echo.New()
			foreign.HTTPErrorHandler = APIErrorHandler
			foreign.IPExtractor = extractClientIP
			foreign.Use(AsiaRoutingMiddleware(foreignH))
			foreign.Use(IPBanMiddleware(foreignH))
			foreign.POST("/api/posts", createPostHandler(foreignH), SessionMiddleware(foreignH, true), RegionMiddleware(foreignH))
			foreign.PUT("/api/me/profile-image", putProfileImageHandler(foreignH), SessionMiddleware(foreignH, true))
			foreign.POST("/api/posts/:postId/realmojis", replaceRealMojiHandler(foreignH), SessionMiddleware(foreignH, true), RegionMiddleware(foreignH))
			foreign.GET("/internal/image-files/:imageId", localFiles.peerHandler())
			foreign.PUT("/internal/image-files/:imageId", func(c *echo.Context) error { return c.NoContent(503) })
			front := httptest.NewServer(foreign)
			defer front.Close()
			peer := echo.New()
			peer.GET("/internal/image-files/:imageId", thirdFiles.peerHandler())
			peer.PUT("/internal/image-files/:imageId", func(c *echo.Context) error { return c.NoContent(503) })
			third := httptest.NewServer(peer)
			defer third.Close()
			localFiles.Peers = []string{back.URL, third.URL}
			asiaFiles.Peers = []string{third.URL, front.URL}
			thirdFiles.Peers = []string{back.URL, front.URL}
			var requests, updates atomic.Int64
			transport := client.client.Transport
			var idsMu sync.Mutex
			var ids []string
			client.client.Transport = uploadTransport(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				requests.Add(1)
				var input primaryRPCRequest
				if err := json.Unmarshal(body, &input); err != nil {
					t.Error(err)
				}
				if input.Op == "image_upload" {
					updates.Add(1)
					if len(body) > 2048 || bytes.Contains(body, []byte("FileHeader")) {
						t.Error("unexpected metadata", len(body))
					}
					id := input.Upload.ImageID
					if _, err := os.Stat(ImageFilePath(localFiles.Root, id)); err != nil {
						t.Error("metadata before local file", err)
					}
					if _, err := os.Stat(ImageFilePath(localFiles.jobRoot(), id)); err != nil {
						t.Error("metadata before durable job", err)
					}
					if _, err := os.Stat(ImageFilePath(asiaFiles.Root, id)); !os.IsNotExist(err) {
						t.Error("Asia saved before commit", err)
					}
					idsMu.Lock()
					ids = append(ids, id)
					idsMu.Unlock()
				}
				return transport.RoundTrip(r)
			})
			jpegData := uploadJPEG(t, 32)
			send := func(r *http.Request, want int) *httptest.ResponseRecorder {
				r.Header.Set("Authorization", "Bearer session-token")
				r.Header.Set("X-Client-IP", "203.0.113.9")
				res := httptest.NewRecorder()
				foreign.ServeHTTP(res, r)
				if res.Code != want {
					t.Fatalf("status=%d want=%d %s", res.Code, want, res.Body)
				}
				return res
			}
			// A rejected client must not persist any bytes or issue a write RPC.
			r := httptest.NewRequest("PUT", "/api/me/profile-image", bytes.NewReader(jpegData))
			r.Header.Set("Content-Type", "image/jpeg")
			r.Header.Set("Authorization", "Bearer invalid")
			res := httptest.NewRecorder()
			foreign.ServeHTTP(res, r)
			if res.Code != 401 || updates.Load() != 0 {
				t.Fatal("unauthenticated upload accepted", res.Code)
			}
			if files, _ := filepath.Glob(filepath.Join(localFiles.Root, "*", "*")); len(files) != 0 {
				t.Fatal("unauthenticated file persisted")
			}
			before := requests.Load()
			r = uploadRequest(t, "/api/posts", jpegData, map[string]string{"caption": "hello", "visibility": "public", "location_kind": "none"})
			postRes := send(r, 201)
			var post createPostResponse
			json.Unmarshal(postRes.Body.Bytes(), &post)
			var dbID string
			if err := db.Get(&dbID, `SELECT image_id FROM posts WHERE id=$1`, post.ID); err != nil {
				t.Fatal(err)
			}
			if dbID != ids[0] {
				t.Fatal("Asia changed image ID")
			}
			r = httptest.NewRequest("PUT", "/api/me/profile-image", bytes.NewReader(jpegData))
			r.Header.Set("Content-Type", "image/jpeg")
			send(r, 204)
			for range 2 {
				send(uploadRequest(t, "/api/posts/target/realmojis", jpegData, map[string]string{"reactionType": "smile"}), 201)
			}
			if requests.Load()-before != 8 || updates.Load() != 4 {
				t.Fatal("expected exactly preauth + metadata per upload", requests.Load()-before, updates.Load())
			}
			var current int
			if err := db.Get(&current, `SELECT count(*) FROM realmojis WHERE replaced_at IS NULL`); err != nil || current != 1 {
				t.Fatal("replacement lost", current, err)
			}
			// GET reaches the origin before any PUT; many callers share one verified fill.
			var wg sync.WaitGroup
			for _, files := range []*ImageFileStore{asiaFiles, thirdFiles, localFiles} {
				for range 16 {
					wg.Add(1)
					go func(files *ImageFileStore) {
						defer wg.Done()
						data, err := files.Read(context.Background(), dbID)
						if err != nil || !bytes.Equal(data, jpegData) {
							t.Error("immediate GET failed", err)
						}
					}(files)
				}
			}
			wg.Wait()
			if asiaFiles.ensureFallback.Load() != 1 || thirdFiles.ensureFallback.Load() != 1 {
				t.Error("singleflight did not share fill")
			}
			// The new metadata path must preserve user-lock serialization for replacements.
			concurrent := make(chan *httptest.ResponseRecorder, 4)
			for i := 0; i < 4; i++ {
				var req *http.Request
				if i < 2 {
					req = uploadRequest(t, "/api/posts/target/realmojis", jpegData, map[string]string{"reactionType": "heart"})
				} else {
					req = httptest.NewRequest("PUT", "/api/me/profile-image", bytes.NewReader(jpegData))
					req.Header.Set("Content-Type", "image/jpeg")
				}
				req.Header.Set("Authorization", "Bearer session-token")
				go func(r *http.Request) { res := httptest.NewRecorder(); foreign.ServeHTTP(res, r); concurrent <- res }(req)
			}
			for range 4 {
				res := <-concurrent
				if res.Code != 201 && res.Code != 204 {
					t.Fatalf("concurrent replacement: %d %s", res.Code, res.Body)
				}
			}
			if err := db.Get(&current, `SELECT count(*) FROM realmojis WHERE replaced_at IS NULL`); err != nil || current != 1 {
				t.Fatal("concurrent replacement lost", current, err)
			}

			// A second post remains forbidden; metadata does not bypass slot checks.
			send(uploadRequest(t, "/api/posts", jpegData, map[string]string{"visibility": "public", "location_kind": "none"}), 409)
			// A forged external metadata request never reaches the DB writer.
			forged := httptest.NewRequest("POST", primaryRPCPath, strings.NewReader(`{"op":"image_upload"}`))
			bad := httptest.NewRecorder()
			asia.ServeHTTP(bad, forged)
			if bad.Code != 403 {
				t.Fatal("unsigned metadata accepted")
			}
		})
	}
}

func TestUploadTransportFailureNeverRetries(t *testing.T) {
	client, err := newPrimaryAPIClient("http://127.0.0.1", "europe", bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	client.client.Transport = uploadTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.GetBody != nil {
			t.Error("write is replayable")
		}
		return nil, errors.New("unknown commit")
	})
	err = client.call(context.Background(), "Bearer token", "203.0.113.9", primaryRPCRequest{Op: "image_upload", Upload: &primaryImageUpload{Kind: "profile", ImageID: "109d53fc-c136-4f3d-a150-f28046faebad"}}, nil)
	if err == nil || calls != 1 {
		t.Fatal("write retried", calls, err)
	}
}

func TestEnsureCancelledLeaderDoesNotCancelOtherWaiters(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	origin := &ImageFileStore{Root: t.TempDir(), token: token}
	id := "109d53fc-c136-4f3d-a150-f28046faebad"
	data := uploadJPEG(t, 1)
	if err := WriteImageFile(origin.Root, id, data, true); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	peer := echo.New()
	peer.GET("/internal/image-files/:imageId", origin.peerHandler())
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			close(entered)
		}
		<-release
		peer.ServeHTTP(w, r)
	}))
	defer server.Close()
	dest := &ImageFileStore{Root: t.TempDir(), token: token, Peers: []string{server.URL}, client: server.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := dest.Ensure(ctx, id); first <- err }()
	<-entered
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() { _, err := dest.Ensure(context.Background(), id); second <- err }()
	close(release)
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second * 3):
		t.Fatal("fill stuck")
	}
	if count.Load() != 1 {
		t.Fatal("duplicate fill")
	}
}

func TestLocalImageUploadValidationAndSaveFailure(t *testing.T) {
	for _, region := range []string{"asia", "europe", "america"} {
		for _, kind := range []string{"post", "profile", "realmoji"} {
			for _, scenario := range []string{"bad jpeg", "truncated jpeg", "invalid metadata", "save failure"} {
				t.Run(region+"/"+kind+"/"+scenario, func(t *testing.T) {
					root := t.TempDir()
					if scenario == "save failure" {
						root = filepath.Join(root, "blocked")
						os.WriteFile(root, []byte("blocked"), 0600)
					}
					files := &ImageFileStore{Root: root, Peers: []string{"http://127.0.0.1:1"}, client: &http.Client{Timeout: time.Millisecond * 50}}
					defer files.Close()
					h := &HandlerContext{Config: &Config{Region: region}, ImageFiles: files}
					// If a failed validation/save tries to issue a metadata call, fail the test.
					var writes atomic.Int64
					if region != "asia" {
						client, _ := newPrimaryAPIClient("http://127.0.0.1", region, bytes.Repeat([]byte("k"), 32))
						client.client.Transport = uploadTransport(func(*http.Request) (*http.Response, error) {
							writes.Add(1)
							return nil, errors.New("unexpected metadata")
						})
						h.PrimaryAPI = client
					}
					e := echo.New()
					e.HTTPErrorHandler = APIErrorHandler
					inject := func(next echo.HandlerFunc) echo.HandlerFunc {
						return func(c *echo.Context) error { c.Set("session", &Session{UserID: "actor"}); return next(c) }
					}
					e.POST("/api/posts", inject(createPostHandler(h)))
					e.PUT("/api/me/profile-image", inject(putProfileImageHandler(h)))
					e.POST("/api/posts/:postId/realmojis", inject(replaceRealMojiHandler(h)))
					data := uploadJPEG(t, 1)
					if scenario == "bad jpeg" {
						data = []byte("not a JPEG")
					}
					if scenario == "truncated jpeg" {
						data = data[:len(data)-30]
					}
					fields := map[string]string{"caption": "hello", "visibility": "public", "location_kind": "none", "reactionType": "smile"}
					if scenario == "invalid metadata" {
						fields["visibility"] = "invalid"
						fields["reactionType"] = "invalid"
					}
					var r *http.Request
					switch kind {
					case "post":
						r = uploadRequest(t, "/api/posts", data, fields)
					case "realmoji":
						r = uploadRequest(t, "/api/posts/target/realmojis", data, fields)
					case "profile":
						r = httptest.NewRequest("PUT", "/api/me/profile-image", bytes.NewReader(data))
						r.Header.Set("Content-Type", "image/jpeg")
						if scenario == "invalid metadata" {
							r.Header.Set("Content-Type", "application/json")
						}
					}
					res := httptest.NewRecorder()
					e.ServeHTTP(res, r)
					want := 400
					if scenario == "save failure" {
						want = 500
					}
					if kind == "profile" && scenario == "invalid metadata" {
						want = 415
					}
					if res.Code != want || writes.Load() != 0 {
						t.Fatalf("status=%d want=%d metadata calls=%d body=%s", res.Code, want, writes.Load(), res.Body)
					}
					if scenario != "save failure" {
						entries, _ := os.ReadDir(root)
						if len(entries) != 0 {
							t.Fatal("invalid upload persisted")
						}
					}
				})
			}
		}
	}
}

func TestUploadFinalSessionCheckPostgres(t *testing.T) {
	for _, change := range []string{"logout", "deleted", "region", "ip ban"} {
		t.Run(change, func(t *testing.T) {
			db := registrationPostgresDB(t)
			_, err := db.Exec(`CREATE TABLE login_sessions(user_id text,token_hash text,active boolean); CREATE TABLE ip_bans(ip text,banned_until timestamptz)`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES('actor','actor','hash','Actor','europe'); INSERT INTO login_sessions VALUES('actor','hash',true)`)
			if err != nil {
				t.Fatal(err)
			}
			held, err := db.Beginx()
			if err != nil {
				t.Fatal(err)
			}
			defer held.Rollback()
			var id string
			if err = held.Get(&id, `SELECT id FROM users WHERE id='actor' FOR NO KEY UPDATE`); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "logout":
				_, err = held.Exec(`UPDATE login_sessions SET active=false`)
			case "deleted":
				_, err = held.Exec(`UPDATE users SET status='deleted'`)
			case "region":
				_, err = held.Exec(`UPDATE users SET region='america'`)
			case "ip ban":
				_, err = held.Exec(`INSERT INTO ip_bans VALUES('203.0.113.9',now()+interval '1 hour')`)
			}
			if err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			r := httptest.NewRequest("PUT", "/api/me/profile-image", nil)
			r.Header.Set("X-Client-IP", "203.0.113.9")
			e.IPExtractor = extractClientIP
			c := e.NewContext(r, httptest.NewRecorder())
			c.Set("session", &Session{UserID: "actor", TokenHash: "hash", Region: "europe"})
			done := make(chan error, 1)
			go func() {
				tx, err := db.Beginx()
				if err != nil {
					done <- err
					return
				}
				defer tx.Rollback()
				var id string
				if err := tx.Get(&id, `SELECT id FROM users WHERE id='actor' FOR NO KEY UPDATE`); err != nil {
					done <- err
					return
				}
				done <- recheckUploadSession(c, tx)
			}()
			select {
			case err := <-done:
				t.Fatal("did not wait for user lock", err)
			case <-time.After(30 * time.Millisecond):
			}
			if err = held.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				var api *APIError
				want := 401
				if change == "ip ban" {
					want = 403
				}
				if !errors.As(err, &api) || api.Status != want {
					t.Fatalf("stale authorization accepted: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("lock stuck")
			}
		})
	}
}

func TestUploadJPEGDimensionBoundaries(t *testing.T) {
	for _, limit := range []int{512, 4096} {
		for _, bounds := range [][2]int{{limit, 1}, {1, limit}, {limit + 1, 1}, {1, limit + 1}} {
			var data bytes.Buffer
			if err := jpeg.Encode(&data, image.NewGray(image.Rect(0, 0, bounds[0], bounds[1])), nil); err != nil {
				t.Fatal(err)
			}
			e := echo.New()
			c := e.NewContext(httptest.NewRequest("POST", "/api/posts", nil), httptest.NewRecorder())
			err := decodeUploadJPEG(c, data.Bytes(), limit, "invalid_post")
			oversized := bounds[0] > limit || bounds[1] > limit
			if (err != nil) != oversized {
				t.Fatalf("%dx%d limit=%d error=%v", bounds[0], bounds[1], limit, err)
			}
		}
	}
}
