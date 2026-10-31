package isureal

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"golang.org/x/sync/singleflight"
)

const maxImageFileSize = 32 << 20

type ImageFileStore struct {
	Root              string
	Peers             []string
	token             []byte
	client            *http.Client
	fills             singleflight.Group
	ensureFallback    atomic.Uint64
	ensureFailures    atomic.Uint64
	replicationOnce   sync.Once
	replicationCancel context.CancelFunc
	replicationWG     sync.WaitGroup
	wake              chan struct{}
	dirtyMu           sync.Mutex
	dirty             [256]bool
	replicationErr    error
	replicationLocks  [256]sync.Mutex
}

func NewImageFileStore(config *Config) (*ImageFileStore, error) {
	token, err := os.ReadFile(config.ImageTokenFile)
	if err != nil {
		return nil, fmt.Errorf("read image replication credentials: %w", err)
	}
	token = bytes.TrimSpace(token)
	if len(token) < 32 {
		return nil, errors.New("image replication credentials are too short")
	}
	if !filepath.IsAbs(config.ImageFileDir) {
		return nil, errors.New("image file directory must be absolute")
	}
	if len(config.ImagePeers) == 0 {
		return nil, errors.New("no image replica peers configured")
	}
	for _, peer := range config.ImagePeers {
		address, err := url.Parse(peer)
		if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Host == "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" || (address.Path != "" && address.Path != "/") {
			return nil, errors.New("invalid image replica address")
		}
	}
	if _, err := os.Stat(filepath.Join(config.ImageFileDir, ".export-complete")); err != nil {
		return nil, errors.New("initial image file export is not complete")
	}
	store := &ImageFileStore{Root: config.ImageFileDir, Peers: config.ImagePeers, token: token, client: &http.Client{Timeout: 15 * time.Second}}
	if err := store.startReplication(); err != nil {
		return nil, err
	}
	return store, nil
}

func imageFileKey(id string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(id))) }
func ImageFilePath(root, id string) string {
	key := imageFileKey(id)
	return filepath.Join(root, key[:2], key)
}

// imageDirectorySync serializes the first parent sync, including callers that
// observe a directory created by an in-flight writer. Roots are initialized by
// the exporter and must not be removed or replaced while the service is running.
var imageDirectorySync = struct {
	sync.Mutex
	ready map[string]bool
}{ready: make(map[string]bool)}

func syncImageFile(f *os.File) error { return f.Sync() }

func prepareImageDirectory(dir string, durable bool, syncFile func(*os.File) error) error {
	if !durable {
		return os.MkdirAll(dir, 0755)
	}
	imageDirectorySync.Lock()
	defer imageDirectorySync.Unlock()
	if imageDirectorySync.ready[dir] {
		return nil
	}
	// The root must already exist durably (the initial export establishes it).
	if err := os.Mkdir(dir, 0755); err != nil && !os.IsExist(err) {
		return err
	}
	parent, err := os.Open(filepath.Dir(dir))
	if err != nil {
		return err
	}
	defer parent.Close()
	// Even EEXIST may mean another process just created this directory. Never
	// infer durability from existence, and never cache a failed sync.
	if err := syncFile(parent); err != nil {
		return err
	}
	imageDirectorySync.ready[dir] = true
	return nil
}

// WriteImageFile never replaces different bytes for an existing image ID.
// Exporters may batch fsync; application uploads must use durable=true.
func WriteImageFile(root, id string, data []byte, durable bool) error {
	return writeImageFile(root, id, data, durable, syncImageFile, (*os.File).Write)
}

// The two operation arguments allow deterministic durability and write failures
// in tests without replacing process-global filesystem functions.
func writeImageFile(root, id string, data []byte, durable bool, syncFile func(*os.File) error, write func(*os.File, []byte) (int, error)) error {
	name, err := filepath.Abs(ImageFilePath(root, id))
	if err != nil {
		return err
	}
	dir := filepath.Dir(name)
	if err := prepareImageDirectory(dir, durable, syncFile); err != nil {
		return err
	}
	if existing, err := os.ReadFile(name); err == nil {
		if !bytes.Equal(existing, data) {
			return errors.New("immutable image content mismatch")
		}
		if !durable {
			return nil
		}
		// A previous writer may have published but not completed its syncs, or
		// returned a sync error. A retry must establish durability itself.
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := syncFile(f); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		f, err := os.CreateTemp(dir, ".image-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		n, err := write(f, data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
		if err := f.Chmod(0644); err != nil {
			return err
		}
		if durable {
			if err := syncFile(f); err != nil {
				return err
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
		// link publishes atomically without overwriting an existing immutable object.
		if err := os.Link(f.Name(), name); err != nil {
			if !os.IsExist(err) {
				return err
			}
			existing, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			if !bytes.Equal(existing, data) {
				return errors.New("concurrent immutable image content mismatch")
			}
			// Our synced inode may differ from the winner's inode.
			if durable {
				winner, err := os.Open(name)
				if err != nil {
					return err
				}
				defer winner.Close()
				if err := syncFile(winner); err != nil {
					return err
				}
			}
		}
	}
	if durable {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return syncFile(d)
	}
	return nil
}

func (s *ImageFileStore) peerRequest(ctx context.Context, peer, method, id string, data []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(peer, "/")+"/internal/image-files/"+id, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	req.Header.Set("X-Image-Time", timestamp)
	req.Header.Set("X-Image-Digest", digest)
	req.Header.Set("X-Image-Signature", s.signature(method, id, timestamp, digest))
	return s.client.Do(req)
}
func (s *ImageFileStore) signature(method, id, timestamp, digest string) string {
	mac := hmac.New(sha256.New, s.token)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s", method, id, timestamp, digest)
	return hex.EncodeToString(mac.Sum(nil))
}

// Save guarantees local durability and durable replication intent only. Remote
// failures cannot fail a committed upload. ctx belongs to the request, not workers.
func (s *ImageFileStore) Save(ctx context.Context, id string, data []byte) error {
	if len(data) > maxImageFileSize {
		return errors.New("image exceeds file limit")
	}
	if len(s.Peers) == 0 {
		return errors.New("no image replica peers configured")
	}
	if err := s.startReplication(); err != nil {
		return err
	}
	if err := WriteImageFile(s.Root, id, data, true); err != nil {
		return err
	}
	// Persist the queue entry before success (and before callers commit DB metadata).
	// A crash before this point may leave an unreferenced local image, but cannot
	// lose replication work for an acknowledged Save.
	lock := s.replicationJobLock(ImageFilePath(s.jobRoot(), id))
	lock.Lock()
	err := WriteImageFile(s.jobRoot(), id, []byte(id), true)
	lock.Unlock()
	if err != nil {
		return err
	}
	s.markReplicationShard(ImageFilePath(s.jobRoot(), id))
	return nil
}

// Ensure reads local files or another file replica; it never reads DB image bytes.
func (s *ImageFileStore) Ensure(ctx context.Context, id string) (string, error) {
	key := imageFileKey(id)
	name := ImageFilePath(s.Root, id)
	if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
		return "/_internal/image-files/" + key[:2] + "/" + key, nil
	}
	result := s.fills.DoChan(id, func() (any, error) {
		// A cancelled first caller must not poison concurrent waiters. Bound the shared
		// fill independently, and cancel losing peer GETs as soon as a valid copy wins.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
			return nil, nil
		}
		s.ensureFallback.Add(1)
		copies := make(chan []byte, len(s.Peers))
		for _, peer := range s.Peers {
			go func(peer string) {
				var verified []byte
				defer func() { copies <- verified }()
				response, err := s.peerRequest(ctx, peer, http.MethodGet, id, nil)
				if err != nil {
					return
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					return
				}
				data, err := io.ReadAll(io.LimitReader(response.Body, maxImageFileSize+1))
				if err != nil || len(data) > maxImageFileSize {
					return
				}
				digest := fmt.Sprintf("%x", sha256.Sum256(data))
				got, err := hex.DecodeString(response.Header.Get("X-Image-Signature"))
				want, _ := hex.DecodeString(s.signature("RESPONSE", id, response.Request.Header.Get("X-Image-Time"), digest))
				if err != nil || !hmac.Equal(got, want) {
					return
				}
				verified = data
			}(peer)
		}
		for range s.Peers {
			select {
			case <-ctx.Done():
				s.ensureFailures.Add(1)
				return nil, ctx.Err()
			case data := <-copies:
				if data == nil {
					continue
				}
				if err := WriteImageFile(s.Root, id, data, true); err != nil {
					s.ensureFailures.Add(1)
					return nil, err
				}
				return nil, nil
			}
		}
		s.ensureFailures.Add(1)
		return nil, errors.New("image file unavailable on all replicas")
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case done := <-result:
		if done.Err != nil {
			return "", done.Err
		}
		return "/_internal/image-files/" + key[:2] + "/" + key, nil
	}
}
func (s *ImageFileStore) Read(ctx context.Context, id string) ([]byte, error) {
	if _, err := s.Ensure(ctx, id); err != nil {
		return nil, err
	}
	return os.ReadFile(ImageFilePath(s.Root, id))
}

func (s *ImageFileStore) peerHandler() echo.HandlerFunc {
	return func(c *echo.Context) error {
		id := c.Param("imageId")
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id {
			return c.NoContent(http.StatusForbidden)
		}
		timestamp := c.Request().Header.Get("X-Image-Time")
		seconds, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil || time.Since(time.Unix(seconds, 0)) > 30*time.Second || time.Until(time.Unix(seconds, 0)) > 30*time.Second {
			return c.NoContent(http.StatusForbidden)
		}
		digest := c.Request().Header.Get("X-Image-Digest")
		got, err := hex.DecodeString(c.Request().Header.Get("X-Image-Signature"))
		if err != nil {
			return c.NoContent(http.StatusForbidden)
		}
		want, _ := hex.DecodeString(s.signature(c.Request().Method, id, timestamp, digest))
		if !hmac.Equal(got, want) {
			return c.NoContent(http.StatusForbidden)
		}
		if c.Request().Method == http.MethodGet {
			c.Response().Header().Set("Cache-Control", "no-store")
			data, err := os.ReadFile(ImageFilePath(s.Root, id))
			if os.IsNotExist(err) {
				return c.NoContent(http.StatusNotFound)
			}
			if err != nil {
				return err
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(data))
			c.Response().Header().Set("X-Image-Signature", s.signature("RESPONSE", id, timestamp, digest))
			return c.Blob(http.StatusOK, "application/octet-stream", data)
		}
		data, err := io.ReadAll(io.LimitReader(c.Request().Body, maxImageFileSize+1))
		if err != nil {
			return err
		}
		if len(data) > maxImageFileSize {
			return c.NoContent(http.StatusRequestEntityTooLarge)
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
			return c.NoContent(http.StatusForbidden)
		}
		if err := WriteImageFile(s.Root, id, data, true); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}
