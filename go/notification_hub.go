package isureal

import (
	"context"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

type notificationRecipient struct {
	ID     string `db:"id"`
	Region string `db:"region"`
}

type notificationSubscription struct {
	userID, region string
	wake           chan struct{}
	done           chan struct{}
	ready          chan struct{}
	retry          bool
	initialized    bool
}

// notificationHub owns one polling loop while there are subscribers. Channels
// belong to the hub; handlers only receive and unregister their subscription.
type notificationHub struct {
	mu          sync.Mutex
	subscribers map[*notificationSubscription]struct{}
	cancel      context.CancelFunc
	poll        func(context.Context, []string) (map[string]string, map[string]string, error)
}

func newNotificationHub(db *sqlx.DB) *notificationHub {
	return &notificationHub{poll: func(ctx context.Context, ids []string) (map[string]string, map[string]string, error) {
		var windows []struct {
			ID     string `db:"id"`
			Region string `db:"region"`
		}
		if err := db.SelectContext(ctx, &windows, `SELECT id, region FROM posting_windows WHERE ended_at IS NULL`); err != nil {
			return nil, nil, err
		}
		var users []notificationRecipient
		if err := db.SelectContext(ctx, &users, `SELECT id, region FROM users WHERE status = 'active' AND id = ANY($1::text[])`, ids); err != nil {
			return nil, nil, err
		}
		current := make(map[string]string, len(windows))
		for _, w := range windows {
			current[w.Region] = w.ID
		}
		recipients := make(map[string]string, len(users))
		for _, u := range users {
			recipients[u.ID] = u.Region
		}
		return current, recipients, nil
	}}
}

func (h *notificationHub) subscribe(userID, region string) *notificationSubscription {
	s := &notificationSubscription{userID: userID, region: region, wake: make(chan struct{}, 1), done: make(chan struct{}), ready: make(chan struct{})}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subscribers == nil {
		h.subscribers = make(map[*notificationSubscription]struct{})
	}
	h.subscribers[s] = struct{}{}
	if h.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.run(ctx)
	}
	return s
}

// Retry a failed notification read after the next successful shared poll.
func (h *notificationHub) retry(s *notificationSubscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subscribers[s]; ok {
		s.retry = true
	}
}

func (h *notificationHub) unsubscribe(s *notificationSubscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.remove(s)
}

// remove is called with mu held. Removing the last subscriber cancels any SQL
// in flight as well as the ticker wait; a later subscription starts a fresh loop.
func (h *notificationHub) remove(s *notificationSubscription) {
	if _, ok := h.subscribers[s]; !ok {
		return
	}
	delete(h.subscribers, s)
	close(s.done)
	if len(h.subscribers) == 0 && h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}

func (h *notificationHub) run(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	previous := make(map[string]string)
	for {
		if ctx.Err() != nil {
			return
		}
		h.mu.Lock()
		snapshot := make([]*notificationSubscription, 0, len(h.subscribers))
		ids := make([]string, 0, len(h.subscribers))
		seen := make(map[string]bool)
		for s := range h.subscribers {
			snapshot = append(snapshot, s)
			if !seen[s.userID] {
				ids = append(ids, s.userID)
				seen[s.userID] = true
			}
		}
		h.mu.Unlock()
		if len(ids) == 0 {
			return
		}
		windows, users, err := h.poll(ctx, ids)
		h.mu.Lock()
		// An old, cancelled loop must never affect a newly started generation.
		if ctx.Err() != nil {
			h.mu.Unlock()
			return
		}
		// A failed poll says nothing about account validity or window changes.
		// Keep subscriptions and the last successful snapshot until retry.
		if err == nil {
			for _, s := range snapshot {
				if _, ok := h.subscribers[s]; !ok {
					continue
				}
				region, active := users[s.userID]
				if !active || region != s.region {
					h.remove(s)
					continue
				}
				if s.retry || (s.initialized && windows[s.region] != previous[s.region]) {
					s.retry = false
					select {
					case s.wake <- struct{}{}:
					default:
					}
				}
				if !s.initialized {
					s.initialized = true
					close(s.ready)
				}
			}
			previous = windows
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
