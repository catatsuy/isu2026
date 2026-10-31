package isureal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestImageFileReplicas(t *testing.T) {
	ctx := context.Background()
	token := bytes.Repeat([]byte("k"), 32)
	replica := &ImageFileStore{Root: t.TempDir(), token: token}
	e := echo.New()
	e.PUT("/internal/image-files/:imageId", replica.peerHandler())
	e.GET("/internal/image-files/:imageId", replica.peerHandler())
	server := httptest.NewServer(e)
	defer server.Close()
	primary := &ImageFileStore{Root: t.TempDir(), Peers: []string{server.URL}, token: token, client: &http.Client{Timeout: time.Second}}
	id := "109d53fc-c136-4f3d-a150-f28046faebad"
	data := []byte("immutable jpeg")
	if err := primary.Save(ctx, id, data); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(ImageFilePath(replica.Root, id))
	if err != nil || !bytes.Equal(copied, data) {
		t.Fatalf("replica differs: %v", err)
	}
	if err := os.Remove(ImageFilePath(primary.Root, id)); err != nil {
		t.Fatal(err)
	}
	recovered, err := primary.Read(ctx, id)
	if err != nil || !bytes.Equal(recovered, data) {
		t.Fatalf("file recovery failed: %v", err)
	}
	if err := primary.Save(ctx, id, []byte("different")); err == nil {
		t.Fatal("immutable image overwritten")
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/image-files/"+id, nil)
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	if response.Code != 403 {
		t.Fatal("unauthenticated internal download allowed")
	}
	primary.token = bytes.Repeat([]byte("x"), 32)
	if err := primary.Save(ctx, "209d53fc-c136-4f3d-a150-f28046faebad", data); err == nil {
		t.Fatal("unacknowledged replication accepted")
	}
}

func TestNewImageRequiresReplica(t *testing.T) {
	store := &ImageFileStore{Root: t.TempDir()}
	if err := store.Save(context.Background(), "id", []byte("image")); err == nil {
		t.Fatal("single local copy accepted")
	}
}

// Find distinct IDs routed to the same shard.
func sameImageDirectoryIDs(n int) []string {
	ids := []string{"image-0"}
	shard := imageFileKey(ids[0])[:2]
	for i := 1; len(ids) < n; i++ {
		id := fmt.Sprintf("image-%d", i)
		if imageFileKey(id)[:2] == shard {
			ids = append(ids, id)
		}
	}
	return ids
}

func TestWriteImageFileSyncs(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		t.Run(fmt.Sprint(preexisting), func(t *testing.T) {
			root := t.TempDir()
			ids := sameImageDirectoryIDs(2)
			dir := filepath.Dir(ImageFilePath(root, ids[0]))
			if preexisting {
				if err := os.Mkdir(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			syncFile := func(f *os.File) error {
				calls = append(calls, f.Name())
				return f.Sync()
			}
			for i, id := range ids {
				calls = nil
				if err := writeImageFile(root, id, []byte("image"), true, syncFile, (*os.File).Write); err != nil {
					t.Fatal(err)
				}
				want := 2
				if i == 0 {
					want = 3
				}
				if len(calls) != want {
					t.Fatalf("syncs = %v, want %d", calls, want)
				}
				if i == 0 && calls[0] != root {
					t.Fatalf("parent not synced first: %v", calls)
				}
				if calls[len(calls)-1] != dir {
					t.Fatalf("destination not synced: %v", calls)
				}
			}
			calls = nil
			if err := writeImageFile(root, ids[0], []byte("image"), true, syncFile, (*os.File).Write); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 || calls[0] != ImageFilePath(root, ids[0]) {
				t.Fatalf("retry syncs: %v", calls)
			}
			if err := WriteImageFile(root, ids[0], []byte("different"), true); err == nil {
				t.Fatal("overwrite accepted")
			}
		})
	}
}

func TestWriteImageFileFailures(t *testing.T) {
	for _, stage := range []string{"parent", "write", "short write", "file", "directory"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			id := "image"
			dir := filepath.Dir(ImageFilePath(root, id))
			injected := errors.New("injected failure")
			syncFile := func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if (stage == "parent" && f.Name() == root) || (stage == "directory" && f.Name() == dir) || (stage == "file" && !info.IsDir()) {
					return injected
				}
				return f.Sync()
			}
			write := func(f *os.File, b []byte) (int, error) {
				if stage == "write" {
					return 0, injected
				}
				if stage == "short write" {
					return 0, nil
				}
				return f.Write(b)
			}
			err := writeImageFile(root, id, []byte("image"), true, syncFile, write)
			want := injected
			if stage == "short write" {
				want = io.ErrShortWrite
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".image-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("temporary files: %v, %v", temps, err)
			}
			// In particular a published file after a failed directory sync must be
			// synced again, and a failed parent sync must not populate the cache.
			var calls []string
			if err := writeImageFile(root, id, []byte("image"), true, func(f *os.File) error {
				calls = append(calls, f.Name())
				return f.Sync()
			}, (*os.File).Write); err != nil {
				t.Fatal(err)
			}
			if stage == "parent" && calls[0] != root {
				t.Fatalf("failed parent sync cached: %v", calls)
			}
			if calls[len(calls)-1] != dir {
				t.Fatalf("retry omitted directory sync: %v", calls)
			}
		})
	}
}

func TestWriteImageFileConcurrent(t *testing.T) {
	for _, sameID := range []bool{false, true} {
		t.Run(fmt.Sprint(sameID), func(t *testing.T) {
			root := t.TempDir()
			ids := sameImageDirectoryIDs(16)
			entered, release := make(chan struct{}), make(chan struct{})
			var mu sync.Mutex
			parentSyncs := 0
			syncFile := func(f *os.File) error {
				if f.Name() == root {
					mu.Lock()
					parentSyncs++
					mu.Unlock()
					close(entered)
					<-release
				}
				return f.Sync()
			}
			results := make(chan error, len(ids))
			go func() { results <- writeImageFile(root, ids[0], []byte("image"), true, syncFile, (*os.File).Write) }()
			<-entered
			for _, id := range ids[1:] {
				if sameID {
					id = ids[0]
				}
				go func() { results <- writeImageFile(root, id, []byte("image"), true, syncFile, (*os.File).Write) }()
			}
			select {
			case err := <-results:
				t.Fatalf("returned before parent sync: %v", err)
			default:
			}
			close(release)
			for range ids {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			if parentSyncs != 1 {
				t.Fatalf("parent syncs = %d", parentSyncs)
			}
			for _, id := range ids {
				if sameID {
					id = ids[0]
				}
				data, err := os.ReadFile(ImageFilePath(root, id))
				if err != nil || string(data) != "image" {
					t.Fatalf("file = %q, %v", data, err)
				}
			}
		})
	}
}

func TestImageReplicaPersistenceFailure(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	replica := &ImageFileStore{Root: t.TempDir(), token: token}
	id := "109d53fc-c136-4f3d-a150-f28046faebad"
	// Prevent the destination directory from being created.
	if err := os.WriteFile(filepath.Dir(ImageFilePath(replica.Root, id)), []byte("blocked"), 0644); err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.PUT("/internal/image-files/:imageId", replica.peerHandler())
	server := httptest.NewServer(e)
	defer server.Close()
	primary := &ImageFileStore{Root: t.TempDir(), Peers: []string{server.URL}, token: token, client: server.Client()}
	if err := primary.Save(context.Background(), id, []byte("image")); err == nil {
		t.Fatal("accepted failed replica persistence")
	}
}

func TestWriteImageFileConcurrentMismatch(t *testing.T) {
	root := t.TempDir()
	start := make(chan struct{})
	type result struct {
		data string
		err  error
	}
	results := make(chan result, 2)
	for _, data := range []string{"first", "second"} {
		go func() {
			<-start
			results <- result{data, WriteImageFile(root, "id", []byte(data), true)}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if (a.err == nil) == (b.err == nil) {
		t.Fatalf("expected one winner: %v, %v", a.err, b.err)
	}
	winner := a.data
	if a.err != nil {
		winner = b.data
	}
	got, err := os.ReadFile(ImageFilePath(root, "id"))
	if err != nil || string(got) != winner {
		t.Fatalf("winner changed: %q, %v", got, err)
	}
}

func TestImageSaveWaitsForReplica(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	replica := &ImageFileStore{Root: t.TempDir(), token: token}
	e := echo.New()
	e.PUT("/internal/image-files/:imageId", replica.peerHandler())
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		e.ServeHTTP(w, r)
	}))
	defer server.Close()
	primary := &ImageFileStore{Root: t.TempDir(), Peers: []string{server.URL}, token: token, client: server.Client()}
	done := make(chan error, 1)
	go func() {
		done <- primary.Save(context.Background(), "109d53fc-c136-4f3d-a150-f28046faebad", []byte("image"))
	}()
	<-entered
	select {
	case err := <-done:
		close(release)
		t.Fatalf("returned before replica persistence: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
