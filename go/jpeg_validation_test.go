package isureal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestJPEGValidationCache(t *testing.T) {
	var cache jpegValidationCache
	data := jpegFixture(t)
	for range 2 {
		size, err := cache.dimensions(data)
		if err != nil || size != image.Pt(512, 512) {
			t.Fatalf("size=%v err=%v", size, err)
		}
	}
	if cache.decodes != 1 || cache.hits != 1 {
		t.Fatal("valid image was decoded twice")
	}
	truncated := data[:len(data)-10]
	for range 2 {
		if _, err := cache.dimensions(truncated); err == nil {
			t.Fatal("truncated image accepted after valid image cached")
		}
	}
	if cache.invalid != 2 || len(cache.entries) != 1 {
		t.Fatal("invalid JPEG was cached")
	}
	changed := append(bytes.Clone(data), 0)
	if _, err := cache.dimensions(changed); err != nil {
		t.Fatal(err)
	}
	if cache.decodes != 4 {
		t.Fatal("different input reused cached decode")
	}
}

func TestJPEGValidationCacheBoundAndConcurrent(t *testing.T) {
	var cache jpegValidationCache
	cache.entries = make(map[[sha256.Size]byte]image.Point)
	for i := range jpegValidationCacheLimit {
		var key [sha256.Size]byte
		binary.LittleEndian.PutUint64(key[:], uint64(i))
		cache.order[i] = key
		cache.entries[key] = image.Pt(1, 1)
	}
	data := jpegFixture(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 5 {
				if _, err := cache.dimensions(data); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if cache.decodes != 1 || cache.evictions != 1 || len(cache.entries) != jpegValidationCacheLimit {
		t.Fatalf("decodes=%d evictions=%d entries=%d", cache.decodes, cache.evictions, len(cache.entries))
	}
	if _, ok := cache.entries[[sha256.Size]byte{}]; ok {
		t.Fatal("oldest entry not evicted")
	}
}

func TestCachedJPEGStillEnforcesRouteLimits(t *testing.T) {
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewGray(image.Rect(0, 0, 513, 1)), nil); err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	c := e.NewContext(httptest.NewRequest("POST", "/api/posts", nil), httptest.NewRecorder())
	if err := decodeUploadJPEG(c, data.Bytes(), 4096, "invalid_post"); err != nil {
		t.Fatal(err)
	}
	err := decodeUploadJPEG(c, data.Bytes(), 512, "invalid_profile_image")
	var api *APIError
	if !errors.As(err, &api) || api.Status != 400 || api.Code != "invalid_profile_image" {
		t.Fatalf("cached dimensions bypassed limit: %v", err)
	}
	malformed := data.Bytes()[:len(data.Bytes())-10]
	if decodeUploadJPEG(c, malformed, 512, "invalid_profile_image") == nil || c.Get("abuse") != "invalid_image" {
		t.Fatal("invalid profile abuse behavior changed")
	}
}

func BenchmarkJPEGValidationCache(b *testing.B) {
	data := jpegFixture(b)
	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		var cache jpegValidationCache
		cache.dimensions(data)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				cache.dimensions(data)
			}
		})
	})
}
