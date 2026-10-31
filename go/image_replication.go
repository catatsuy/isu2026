package isureal

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const imageReplicationWorkers = 4

func (s *ImageFileStore) jobRoot() string { return filepath.Join(s.Root, ".replication") }

// startReplication is also lazy for stores built directly by tests. Configuration
// and Root must remain immutable. A store owns exactly one scheduler and four workers.
func (s *ImageFileStore) startReplication() error {
	s.replicationOnce.Do(func() {
		if err := prepareImageDirectory(s.jobRoot(), true, syncImageFile); err != nil {
			s.replicationErr = err
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.replicationCancel = cancel
		s.wake = make(chan struct{}, 1)
		jobs := make(chan string)
		completed := make(chan string, imageReplicationWorkers)
		s.replicationWG.Add(imageReplicationWorkers + 1)
		for range imageReplicationWorkers {
			go func() {
				defer s.replicationWG.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case name := <-jobs:
						s.replicateJob(ctx, name)
						select {
						case completed <- name:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
		}
		go func() {
			defer s.replicationWG.Done()
			// Only active workers are tracked. The disk is the queue; scans use bounded
			// batches rather than loading all pending image IDs or bytes into memory.
			active := make(map[string]bool)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				for i := 0; i < 256; i++ {
					dir := filepath.Join(s.jobRoot(), shardName(i))
					f, err := os.Open(dir)
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						log.Printf("image replication scan: %v", err)
						continue
					}
					for {
						select {
						case <-ctx.Done():
							f.Close()
							return
						default:
						}
						entries, err := f.ReadDir(64)
						for _, entry := range entries {
							if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || len(entry.Name()) != 64 {
								continue
							}
							info, statErr := entry.Info()
							if statErr != nil || info.ModTime().After(time.Now()) {
								continue
							}
							name := filepath.Join(dir, entry.Name())
							for {
								select {
								case done := <-completed:
									delete(active, done)
								default:
									goto drained
								}
							}
						drained:
							if active[name] {
								continue
							}
							// Never wait for worker capacity while holding up Save: this scheduler
							// alone may block; Save merely writes a marker and signals wake.
							for {
								select {
								case <-ctx.Done():
									f.Close()
									return
								case done := <-completed:
									delete(active, done)
								case jobs <- name:
									active[name] = true
									goto dispatched
								}
							}
						dispatched:
						}
						if err != nil {
							if err != io.EOF {
								log.Printf("image replication scan: %v", err)
							}
							break
						}
					}
					f.Close()
				}
				select {
				case <-ctx.Done():
					return
				case done := <-completed:
					delete(active, done)
				case <-s.wake:
				case <-ticker.C:
				}
			}
		}()
	})
	return s.replicationErr
}

func shardName(i int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[i/16], hex[i%16]})
}

// Close cancels in-flight HTTP calls and joins all workers.
// Pending markers survive cancellation. Call after stopping upload handlers.
func (s *ImageFileStore) Close() {
	if s.replicationCancel != nil {
		s.replicationCancel()
		s.replicationWG.Wait()
	}
}

// Sharded locks serialize marker durability with completion/removal. Their count
// stays fixed even when many uploads share an ID or the disk queue grows.
func (s *ImageFileStore) replicationJobLock(name string) *sync.Mutex {
	shard, _ := strconv.ParseUint(filepath.Base(name)[:2], 16, 8)
	return &s.replicationLocks[shard]
}

func (s *ImageFileStore) replicateJob(ctx context.Context, name string) {
	retryName := filepath.Join(filepath.Dir(name), ".retry-"+filepath.Base(name))
	err := s.replicateAttempt(ctx, name)
	lock := s.replicationJobLock(name)
	lock.Lock()
	defer lock.Unlock()
	if err == nil {
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			log.Printf("image replication remove %s: %v", name, err)
		}
		os.Remove(retryName)
		// A crash before this sync can only cause a harmless duplicate PUT.
		if d, err := os.Open(filepath.Dir(name)); err == nil {
			if err := d.Sync(); err != nil {
				log.Printf("image replication sync: %v", err)
			}
			d.Close()
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	// An entry observed by a scan can finish before that scan dispatches it.
	if _, jobErr := os.Stat(name); os.IsNotExist(jobErr) {
		return
	}
	// Retry metadata is advisory: losing it only resets the backoff. The immutable,
	// fsynced job marker is the authoritative record. No in-memory retry map grows.
	raw, _ := os.ReadFile(retryName)
	seconds, _ := strconv.Atoi(string(raw))
	if seconds < 1 {
		seconds = 1
	} else {
		seconds *= 2
	}
	if seconds > 60 {
		seconds = 60
	}
	delay := time.Duration(seconds) * time.Second
	log.Printf("image replication %s failed (retry in %s): %v", name, delay, err)
	if err := os.WriteFile(retryName, []byte(strconv.Itoa(seconds)), 0600); err != nil {
		log.Printf("image replication retry metadata: %v", err)
	}
	next := time.Now().Add(delay)
	if err := os.Chtimes(name, next, next); err != nil {
		log.Printf("image replication retry schedule: %v", err)
	}
}

func (s *ImageFileStore) replicateAttempt(ctx context.Context, name string) error {
	id, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	if ImageFilePath(s.jobRoot(), string(id)) != name {
		return errors.New("invalid replication marker")
	}
	data, err := os.ReadFile(ImageFilePath(s.Root, string(id)))
	if err != nil {
		return err
	}
	if len(data) > maxImageFileSize {
		return errors.New("image exceeds file limit")
	}
	failed := 0
	for _, peer := range s.Peers {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		response, err := s.peerRequest(attemptCtx, peer, http.MethodPut, string(id), data)
		ok := err == nil && response != nil && response.StatusCode == http.StatusNoContent
		if response != nil {
			response.Body.Close()
		}
		cancel()
		if !ok {
			log.Printf("image replication id=%s peer=%s: response=%v error=%v", id, peer, statusCode(response), err)
			failed++
		}
	}
	if failed != 0 {
		return errors.New("peer copies incomplete")
	}
	return nil
}

func statusCode(response *http.Response) int {
	if response == nil {
		return 0
	}
	return response.StatusCode
}
