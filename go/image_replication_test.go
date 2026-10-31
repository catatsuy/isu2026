package isureal

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func waitImageTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for image replication")
}

func TestImageReplicationRetryAndRestart(t *testing.T) {
	var attempts atomic.Int32
	var healthy atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if !healthy.Load() {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer peer.Close()
	root := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, bytes.Repeat([]byte("k"), 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".export-complete"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	newStore := func() *ImageFileStore {
		store, err := NewImageFileStore(&Config{ImageFileDir: root, ImagePeers: []string{peer.URL}, ImageTokenFile: tokenFile})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	first := newStore()
	ctx, cancel := context.WithCancel(context.Background())
	if err := first.Save(ctx, "image", []byte("data")); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitImageTest(t, func() bool { return attempts.Load() >= 2 })
	first.Close()
	marker := ImageFilePath(first.jobRoot(), "image")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	healthy.Store(true)
	second := newStore()
	defer second.Close()
	// Start without another upload, as the production constructor does.
	if err := second.startReplication(); err != nil {
		t.Fatal(err)
	}
	waitImageTest(t, func() bool { _, err := os.Stat(marker); return os.IsNotExist(err) })
	if attempts.Load() < 3 {
		t.Fatal("restart did not resume")
	}
}

func TestImageEnsureBeforeReplication(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	putStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case putStarted <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(204)
	}))
	defer slow.Close()
	defer close(release)
	source := &ImageFileStore{Root: t.TempDir(), Peers: []string{slow.URL}, token: token, client: slow.Client()}
	defer source.Close()
	id := "109d53fc-c136-4f3d-a150-f28046faebad"
	if err := source.Save(context.Background(), id, []byte("data")); err != nil {
		t.Fatal(err)
	}
	<-putStarted
	e := echo.New()
	e.GET("/internal/image-files/:imageId", source.peerHandler())
	server := httptest.NewServer(e)
	defer server.Close()
	reader := &ImageFileStore{Root: t.TempDir(), Peers: []string{server.URL}, token: token, client: server.Client()}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := reader.Read(context.Background(), id)
			if err != nil || string(data) != "data" {
				t.Errorf("read: %q %v", data, err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(ImageFilePath(reader.Root, id))
	if err != nil || string(data) != "data" {
		t.Fatalf("recovery not persisted: %q %v", data, err)
	}
}

func TestImageReplicationBoundedAndConcurrent(t *testing.T) {
	var active, peak atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		<-release
		w.WriteHeader(204)
	}))
	defer server.Close()
	defer close(release)
	store := &ImageFileStore{Root: t.TempDir(), Peers: []string{server.URL}, client: server.Client()}
	defer store.Close()
	baseline := runtime.NumGoroutine()
	for i := 0; i < 200; i++ {
		if err := store.Save(context.Background(), fmt.Sprint(i), []byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Save(context.Background(), "same", []byte("same")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	waitImageTest(t, func() bool { return peak.Load() == imageReplicationWorkers })
	if delta := runtime.NumGoroutine() - baseline; delta > 50 {
		t.Fatalf("goroutines grew with pending jobs: %d", delta)
	}
	if peak.Load() > imageReplicationWorkers {
		t.Fatalf("unbounded requests: %d", peak.Load())
	}
	if data, err := os.ReadFile(ImageFilePath(store.Root, "same")); err != nil || string(data) != "same" {
		t.Fatalf("corrupt: %q %v", data, err)
	}
}

func TestImageSaveLocalFailure(t *testing.T) {
	root := t.TempDir()
	id := "blocked"
	if err := os.WriteFile(filepath.Dir(ImageFilePath(root, id)), []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &ImageFileStore{Root: root, Peers: []string{"http://unused.invalid"}, client: &http.Client{}}
	defer store.Close()
	if err := store.Save(context.Background(), id, []byte("data")); err == nil {
		t.Fatal("accepted local failure")
	}
}

func TestImageReplicationAllPeers(t *testing.T) {
	var successes, failures atomic.Int32
	var healthy atomic.Bool
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failures.Add(1)
		if healthy.Load() {
			w.WriteHeader(204)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer failed.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { successes.Add(1); w.WriteHeader(204) }))
	defer good.Close()
	store := &ImageFileStore{Root: t.TempDir(), Peers: []string{failed.URL, good.URL}, client: good.Client()}
	defer store.Close()
	if err := store.Save(context.Background(), "image", []byte("data")); err != nil {
		t.Fatal(err)
	}
	waitImageTest(t, func() bool { return successes.Load() > 0 })
	marker := ImageFilePath(store.jobRoot(), "image")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("job removed before all peers acknowledged", err)
	}
	healthy.Store(true)
	waitImageTest(t, func() bool { _, err := os.Stat(marker); return os.IsNotExist(err) })
	if failures.Load() < 2 {
		t.Fatal("failed peer was not retried")
	}
}

func TestImageReplicaAuthentication(t *testing.T) {
	token := bytes.Repeat([]byte("k"), 32)
	replica := &ImageFileStore{Root: t.TempDir(), token: token}
	e := echo.New()
	e.PUT("/internal/image-files/:imageId", replica.peerHandler())
	server := httptest.NewServer(e)
	defer server.Close()
	bad := &ImageFileStore{Root: t.TempDir(), Peers: []string{server.URL}, token: bytes.Repeat([]byte("x"), 32), client: server.Client()}
	defer bad.Close()
	id := "109d53fc-c136-4f3d-a150-f28046faebad"
	if err := bad.Save(context.Background(), id, []byte("data")); err != nil {
		t.Fatal(err)
	}
	waitImageTest(t, func() bool {
		_, err := os.Stat(filepath.Join(filepath.Dir(ImageFilePath(bad.jobRoot(), id)), ".retry-"+imageFileKey(id)))
		return err == nil
	})
	if _, err := os.Stat(ImageFilePath(replica.Root, id)); !os.IsNotExist(err) {
		t.Fatal("invalid HMAC stored an image", err)
	}
	// A response without the HMAC must not be persisted by Ensure.
	unsigned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("data")) }))
	defer unsigned.Close()
	reader := &ImageFileStore{Root: t.TempDir(), Peers: []string{unsigned.URL}, token: token, client: unsigned.Client()}
	if _, err := reader.Ensure(context.Background(), id); err == nil {
		t.Fatal("unsigned recovery accepted")
	}
	if _, err := os.Stat(ImageFilePath(reader.Root, id)); !os.IsNotExist(err) {
		t.Fatal("unsigned recovery persisted", err)
	}
}

func TestImageSaveJobPersistenceFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".replication"), []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &ImageFileStore{Root: root, Peers: []string{"http://unused.invalid"}, client: &http.Client{}}
	defer store.Close()
	if err := store.Save(context.Background(), "image", []byte("data")); err == nil {
		t.Fatal("accepted job persistence failure")
	}
}

type imageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f imageRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestImageReplicationTimeoutAndClose(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	client := &http.Client{Transport: imageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 15*time.Second {
			t.Error("missing replication timeout")
		}
		close(entered)
		<-r.Context().Done()
		close(canceled)
		return nil, r.Context().Err()
	})}
	store := &ImageFileStore{Root: t.TempDir(), Peers: []string{"http://peer.invalid"}, client: client}
	defer store.Close()
	if err := store.Save(context.Background(), "image", []byte("data")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	done := make(chan struct{})
	go func() { store.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not join workers")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("HTTP call not canceled")
	}
	if _, err := os.Stat(ImageFilePath(store.jobRoot(), "image")); err != nil {
		t.Fatal("canceled job lost", err)
	}
}

func TestImageConcurrentSaveDuringReplicationCompletion(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer peer.Close()
	store := &ImageFileStore{Root: t.TempDir(), Peers: []string{peer.URL}, client: peer.Client()}
	defer store.Close()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if err := store.Save(context.Background(), "same", []byte("data")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	waitImageTest(t, func() bool { _, err := os.Stat(ImageFilePath(store.jobRoot(), "same")); return os.IsNotExist(err) })
}
