package isureal

import (
	"bytes"
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
	cache.entries = make(map[imageURLCacheKey]imageURLCacheEntry, imageURLCacheLimit)
	for i := 0; i < imageURLCacheLimit; i++ {
		cache.entries[imageURLCacheKey{imageID: strconv.Itoa(i)}] = imageURLCacheEntry{}
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
	if len(cache.entries) != 1 {
		t.Fatalf("unexpected cache contents: %d", len(cache.entries))
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
