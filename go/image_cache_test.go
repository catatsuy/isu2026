package isureal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageCacheConcurrentFillAndGeneration(t *testing.T) {
	var cache imageFileCache
	root := t.TempDir()
	created := time.Now()
	key := imageCacheKey("1", "../../unsafe\r\n", created)
	var calls atomic.Int32
	load := func() ([]byte, error) {
		calls.Add(1)
		return []byte("image bytes"), nil
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			uri, err := cache.ensure(context.Background(), root, key, load)
			if err != nil {
				t.Error(err)
				return
			}
			data, err := os.ReadFile(filepath.Join(root, strings.TrimPrefix(uri, "/_internal/images/")))
			if err != nil || string(data) != "image bytes" {
				t.Errorf("incomplete image: %q, %v", data, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("loaded %d times", calls.Load())
	}
	if _, err := cache.ensure(context.Background(), root, imageCacheKey("2", "../../unsafe\r\n", created), load); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("new generation reused old cache")
	}
}

func TestImageCacheLoadFailureIsRetried(t *testing.T) {
	var cache imageFileCache
	root := t.TempDir()
	key := imageCacheKey("1", "id", time.Now())
	want := errors.New("database unavailable")
	if _, err := cache.ensure(context.Background(), root, key, func() ([]byte, error) { return nil, want }); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, key[:2], key)); !os.IsNotExist(err) {
		t.Fatalf("failed load created cache: %v", err)
	}
	if _, err := cache.ensure(context.Background(), root, key, func() ([]byte, error) { return []byte("retry"), nil }); err != nil {
		t.Fatal(err)
	}
}
