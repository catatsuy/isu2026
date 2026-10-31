package isureal

import (
	"bytes"
	"crypto/sha256"
	"expvar"
	"image"
	"image/jpeg"
	"sync"

	"golang.org/x/sync/singleflight"
)

const jpegValidationCacheLimit = 4096

// Cache only successful full decodes of identical bytes. Keep dimensions, not
// decoded pixels or input buffers; callers enforce their own dimension limits.
type jpegValidationCache struct {
	sync.Mutex
	entries                           map[[sha256.Size]byte]image.Point
	order                             [jpegValidationCacheLimit][sha256.Size]byte
	next                              int
	pending                           singleflight.Group
	hits, decodes, invalid, evictions uint64
}

var validatedJPEGs jpegValidationCache

func init() {
	expvar.Publish("jpeg_validation_cache", expvar.Func(func() any {
		validatedJPEGs.Lock()
		defer validatedJPEGs.Unlock()
		return map[string]uint64{"entries": uint64(len(validatedJPEGs.entries)), "hits": validatedJPEGs.hits, "decodes": validatedJPEGs.decodes, "invalid": validatedJPEGs.invalid, "evictions": validatedJPEGs.evictions}
	}))
}

func (cache *jpegValidationCache) lookup(key [sha256.Size]byte) (image.Point, bool) {
	cache.Lock()
	defer cache.Unlock()
	size, ok := cache.entries[key]
	if ok {
		cache.hits++
	}
	return size, ok
}

func (cache *jpegValidationCache) dimensions(data []byte) (image.Point, error) {
	key := sha256.Sum256(data)
	if size, ok := cache.lookup(key); ok {
		return size, nil
	}
	value, err, _ := cache.pending.Do(string(key[:]), func() (any, error) {
		if size, ok := cache.lookup(key); ok {
			return size, nil
		}
		img, err := jpeg.Decode(bytes.NewReader(data))
		cache.Lock()
		defer cache.Unlock()
		cache.decodes++
		if err != nil {
			cache.invalid++
			return image.Point{}, err
		}
		size := img.Bounds().Size()
		if cache.entries == nil {
			cache.entries = make(map[[sha256.Size]byte]image.Point)
		}
		if len(cache.entries) >= jpegValidationCacheLimit {
			delete(cache.entries, cache.order[cache.next])
			cache.evictions++
		}
		cache.order[cache.next] = key
		cache.next = (cache.next + 1) % jpegValidationCacheLimit
		cache.entries[key] = size
		return size, nil
	})
	if err != nil {
		return image.Point{}, err
	}
	return value.(image.Point), nil
}
