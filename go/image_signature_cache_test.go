package isureal

import (
	"bytes"
	"container/list"
	"encoding/base64"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestImageURLCache(t *testing.T) {
	var cache imageURLCache
	key := bytes.Repeat([]byte{1}, 32)
	now := time.Unix(1800000000, 0)
	first := cache.get(key, "image/id", now)
	if got := cache.get(key, "image/id", now.Add(59*time.Second)); got != first {
		t.Fatal("URL not reused")
	}
	parsed, err := url.Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := strconv.ParseInt(parsed.Query().Get("exp"), 10, 64)
	if err != nil || exp != now.Add(imageURLLifetime).Unix() {
		t.Fatal("incorrect expiration")
	}
	if parsed.Query().Get("sig") != base64.RawURLEncoding.EncodeToString(legacySignature(key, "image/id", exp)) {
		t.Fatal("signature incompatible")
	}
	if cache.get(key, "image/id", now.Add(time.Minute)) == first {
		t.Fatal("URL not refreshed")
	}
	if cache.get(bytes.Repeat([]byte{2}, 32), "image/id", now) == first {
		t.Fatal("key rotation reused signature")
	}
	if cache.get(key, "other", now) == first {
		t.Fatal("different image reused URL")
	}
	if cache.get(key, "image/id", now.Add(-time.Minute)) == first {
		t.Fatal("clock rollback reused future URL")
	}
}

func TestImageURLCacheBoundAndConcurrent(t *testing.T) {
	var cache imageURLCache
	key := bytes.Repeat([]byte{1}, 32)
	now := time.Now()
	cache.entries = make(map[imageURLCacheKey]*list.Element, imageURLCacheLimit)
	for i := 0; i < imageURLCacheLimit; i++ {
		k := imageURLCacheKey{imageID: strconv.Itoa(i)}
		cache.entries[k] = cache.recent.PushFront(imageURLCacheEntry{key: k, created: now})
	}
	hot := imageURLCacheKey{key: string(key), imageID: "hot"}
	oldest := cache.recent.Back()
	delete(cache.entries, oldest.Value.(imageURLCacheEntry).key)
	oldest.Value = imageURLCacheEntry{key: hot, url: "retained", created: now}
	cache.entries[hot] = oldest
	if cache.get(key, "hot", now) != "retained" {
		t.Fatal("hot entry not found")
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for j := 0; j < 20; j++ {
				cache.get(key, "image", now)
			}
		})
	}
	wg.Wait()
	if len(cache.entries) > imageURLCacheLimit {
		t.Fatal("cache exceeded limit")
	}
	if len(cache.entries) != imageURLCacheLimit || cache.evictions != 1 || cache.misses != 1 {
		t.Fatalf("unexpected cache contents: %d", len(cache.entries))
	}
	if _, ok := cache.entries[hot]; !ok {
		t.Fatal("recently used entry evicted")
	}
	if _, ok := cache.entries[imageURLCacheKey{imageID: "1"}]; ok {
		t.Fatal("least recently used entry retained")
	}

}

func BenchmarkSignedImageURLCache(b *testing.B) {
	key := bytes.Repeat([]byte{1}, 32)
	user := User{ImageKey: key}
	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var cache imageURLCache
			cache.get(key, "image", time.Now())
		}
	})
	b.Run("cached", func(b *testing.B) {
		signedImageURL(user, "image")
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				signedImageURL(user, "image")
			}
		})
	})
}
