package isureal

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/singleflight"
)

var localImageCache imageFileCache

type imageFileCache struct {
	fills singleflight.Group
}

func imageCacheKey(generation, id string, createdAt time.Time) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(generation+"\x00"+id+"\x00"+createdAt.UTC().Format(time.RFC3339Nano))))
}

// Files contain immutable image bytes only. Authorization must precede every call.
func (cache *imageFileCache) ensure(ctx context.Context, root, key string, load func() ([]byte, error)) (string, error) {
	name := filepath.Join(root, key[:2], key)
	result := cache.fills.DoChan(name, func() (any, error) {
		if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
			return nil, nil
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		data, err := load()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			return nil, err
		}
		file, err := os.CreateTemp(filepath.Dir(name), ".image-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(file.Name())
		defer file.Close()
		if _, err := file.Write(data); err != nil {
			return nil, err
		}
		if err := file.Chmod(0644); err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		return nil, os.Rename(file.Name(), name)
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case done := <-result:
		if done.Err != nil {
			return "", done.Err
		}
		return "/_internal/images/" + key[:2] + "/" + key, nil
	}
}
