package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

const imageURLCacheLimit = 8192

type imageURLCacheKey struct {
	key, imageID string
}

type imageURLCacheEntry struct {
	url     string
	created time.Time
}

type imageURLCache struct {
	sync.Mutex
	entries map[imageURLCacheKey]imageURLCacheEntry
}

var signedImageURLs imageURLCache

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

func (cache *imageURLCache) get(key []byte, imageID string, now time.Time) string {
	cacheKey := imageURLCacheKey{key: string(key), imageID: imageID}
	cache.Lock()
	entry, ok := cache.entries[cacheKey]
	cache.Unlock()
	// Reuse only recently issued URLs. Key rotation and image changes naturally
	// miss the cache; image authorization is still checked on every download.
	if age := now.Sub(entry.created); ok && age >= 0 && age < time.Minute {
		return entry.url
	}
	exp := now.Add(imageURLLifetime).Unix()
	query := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {base64.RawURLEncoding.EncodeToString(imageSignature(key, imageID, exp))},
	}
	result := "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
	cache.Lock()
	if cache.entries == nil || len(cache.entries) >= imageURLCacheLimit {
		cache.entries = make(map[imageURLCacheKey]imageURLCacheEntry)
	}
	cache.entries[cacheKey] = imageURLCacheEntry{url: result, created: now}
	cache.Unlock()
	return result
}
