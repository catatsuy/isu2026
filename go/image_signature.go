package isureal

import (
	"container/list"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"expvar"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

const imageURLCacheLimit = 65536

type imageURLCacheKey struct {
	key, imageID string
}

type imageURLCacheEntry struct {
	key     imageURLCacheKey
	url     string
	created time.Time
}

type imageURLCache struct {
	sync.Mutex
	entries                          map[imageURLCacheKey]*list.Element
	recent                           list.List
	pending                          singleflight.Group
	hits, misses, expired, evictions uint64
}

var signedImageURLs imageURLCache

func init() {
	expvar.Publish("image_url_cache", expvar.Func(func() any {
		signedImageURLs.Lock()
		defer signedImageURLs.Unlock()
		return map[string]uint64{"entries": uint64(len(signedImageURLs.entries)), "hits": signedImageURLs.hits, "misses": signedImageURLs.misses, "expired": signedImageURLs.expired, "evictions": signedImageURLs.evictions}
	}))
}

func imageSigningKey(secretKeyHash string) ([]byte, error) {
	key, err := hex.DecodeString(secretKeyHash)
	if err != nil || len(key) != sha256.Size {
		return nil, fmt.Errorf("invalid image signing key")
	}
	return key, nil
}

func imageSignature(key []byte, imageID string, exp int64) []byte {
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	mac := hmac.New(sha256.New, key)
	var digest [sha256.Size]byte
	for range imageSignatureIterations {
		mac.Reset()
		mac.Write(value)
		value = mac.Sum(digest[:0])
	}
	return value
}

func signedImageURL(user User, imageID string) string {
	return signedImageURLs.get(user.ImageKey, imageID, time.Now())
}

func (cache *imageURLCache) lookup(cacheKey imageURLCacheKey, now time.Time) (string, bool) {
	cache.Lock()
	defer cache.Unlock()
	if element := cache.entries[cacheKey]; element != nil {
		entry := element.Value.(imageURLCacheEntry)
		if age := now.Sub(entry.created); age >= 0 && age < time.Minute {
			cache.recent.MoveToFront(element)
			cache.hits++
			return entry.url, true
		}
		cache.expired++
		delete(cache.entries, cacheKey)
		cache.recent.Remove(element)
	}
	return "", false
}

func (cache *imageURLCache) get(key []byte, imageID string, now time.Time) string {
	cacheKey := imageURLCacheKey{key: string(key), imageID: imageID}
	if result, ok := cache.lookup(cacheKey, now); ok {
		return result
	}
	// Coalesce simultaneous misses without holding the LRU lock during HMAC.
	value, _, _ := cache.pending.Do(strconv.Itoa(len(key))+":"+cacheKey.key+imageID, func() (any, error) {
		if result, ok := cache.lookup(cacheKey, now); ok {
			return result, nil
		}
		exp := now.Add(imageURLLifetime).Unix()
		query := url.Values{
			"exp": {strconv.FormatInt(exp, 10)},
			"sig": {base64.RawURLEncoding.EncodeToString(imageSignature(key, imageID, exp))},
		}
		result := "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
		cache.Lock()
		defer cache.Unlock()
		cache.misses++
		if cache.entries == nil {
			cache.entries = make(map[imageURLCacheKey]*list.Element)
		}
		if len(cache.entries) >= imageURLCacheLimit {
			oldest := cache.recent.Back()
			delete(cache.entries, oldest.Value.(imageURLCacheEntry).key)
			cache.recent.Remove(oldest)
			cache.evictions++
		}
		cache.entries[cacheKey] = cache.recent.PushFront(imageURLCacheEntry{key: cacheKey, url: result, created: now})
		return result, nil
	})
	return value.(string)
}
