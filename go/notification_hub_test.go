package isureal

import (
	"context"
	"sync"
	"testing"
	"time"
)

func awaitNotificationSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for hub")
	}
}

func TestNotificationHubBroadcastAndUserChanges(t *testing.T) {
	var mu sync.Mutex
	window := "first"
	users := map[string]string{"one": "jp", "two": "jp", "other": "us"}
	h := &notificationHub{poll: func(ctx context.Context, ids []string) (map[string]string, map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		result := make(map[string]string)
		for _, id := range ids {
			if r, ok := users[id]; ok {
				result[id] = r
			}
		}
		return map[string]string{"jp": window, "us": "unchanged"}, result, nil
	}}
	one := h.subscribe("one", "jp")
	defer h.unsubscribe(one)
	two := h.subscribe("two", "jp")
	defer h.unsubscribe(two)
	other := h.subscribe("other", "us")
	defer h.unsubscribe(other)
	for _, s := range []*notificationSubscription{one, two, other} {
		awaitNotificationSignal(t, s.ready)
	}
	mu.Lock()
	window = "second"
	mu.Unlock()
	awaitNotificationSignal(t, one.wake)
	awaitNotificationSignal(t, two.wake)
	select {
	case <-other.wake:
		t.Fatal("unrelated region woke")
	default:
	}
	mu.Lock()
	users["one"] = "us"
	delete(users, "two")
	mu.Unlock()
	awaitNotificationSignal(t, one.done)
	awaitNotificationSignal(t, two.done)
	h.unsubscribe(other)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscribers) != 0 || h.cancel != nil {
		t.Fatal("idle hub retained resources")
	}
}

func TestNotificationHubSubscribeDuringPoll(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	h := &notificationHub{poll: func(ctx context.Context, ids []string) (map[string]string, map[string]string, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		users := make(map[string]string)
		for _, id := range ids {
			users[id] = "jp"
		}
		return map[string]string{"jp": "new"}, users, nil
	}}
	one := h.subscribe("one", "jp")
	defer h.unsubscribe(one)
	awaitNotificationSignal(t, started)
	two := h.subscribe("two", "jp")
	defer h.unsubscribe(two)
	close(release)
	awaitNotificationSignal(t, one.ready)
	awaitNotificationSignal(t, two.ready)
	// A subscriber absent from an in-flight snapshot must receive its own initial
	// check, even though the hub has already observed this window.
	select {
	case <-two.done:
		t.Fatal("new subscriber incorrectly invalidated")
	default:
	}
}

func TestNotificationHubLastDisconnectCancelsPoll(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	h := &notificationHub{poll: func(ctx context.Context, _ []string) (map[string]string, map[string]string, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, nil, ctx.Err()
	}}
	s := h.subscribe("one", "jp")
	awaitNotificationSignal(t, started)
	h.unsubscribe(s)
	h.unsubscribe(s)
	awaitNotificationSignal(t, stopped)
	awaitNotificationSignal(t, s.done)
}

func TestNotificationHubWakeRemainsPending(t *testing.T) {
	var mu sync.Mutex
	window := "first"
	observed := make(chan struct{}, 8)
	h := &notificationHub{poll: func(context.Context, []string) (map[string]string, map[string]string, error) {
		mu.Lock()
		w := window
		mu.Unlock()
		observed <- struct{}{}
		return map[string]string{"jp": w}, map[string]string{"one": "jp"}, nil
	}}
	s := h.subscribe("one", "jp")
	defer h.unsubscribe(s)
	awaitNotificationSignal(t, s.ready)
	awaitNotificationSignal(t, observed)
	// Model a handler still executing its initial SELECT when a new window commits.
	mu.Lock()
	window = "second"
	mu.Unlock()
	awaitNotificationSignal(t, observed)
	// A following unchanged poll must not discard the unconsumed change event.
	awaitNotificationSignal(t, observed)
	awaitNotificationSignal(t, s.wake)
}

func TestNotificationHubConcurrentSubscriptions(t *testing.T) {
	h := &notificationHub{poll: func(ctx context.Context, ids []string) (map[string]string, map[string]string, error) {
		users := make(map[string]string)
		for _, id := range ids {
			users[id] = "jp"
		}
		return map[string]string{"jp": "first"}, users, nil
	}}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			s := h.subscribe("same-user", "jp")
			h.unsubscribe(s)
		})
	}
	wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subscribers) != 0 || h.cancel != nil {
		t.Fatal("subscriptions leaked")
	}
}

func TestNotificationHubRecoversFromPollErrors(t *testing.T) {
	var mu sync.Mutex
	fail := false
	window := "first"
	failed := make(chan struct{}, 1)
	h := &notificationHub{poll: func(context.Context, []string) (map[string]string, map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			select {
			case failed <- struct{}{}:
			default:
			}
			return nil, nil, context.DeadlineExceeded
		}
		return map[string]string{"jp": window}, map[string]string{"one": "jp", "two": "jp"}, nil
	}}
	one := h.subscribe("one", "jp")
	defer h.unsubscribe(one)
	awaitNotificationSignal(t, one.ready)
	mu.Lock()
	fail = true
	mu.Unlock()
	awaitNotificationSignal(t, failed)
	two := h.subscribe("two", "jp")
	defer h.unsubscribe(two)
	awaitNotificationSignal(t, failed)
	for _, s := range []*notificationSubscription{one, two} {
		select {
		case <-s.done:
			t.Fatal("poll error disconnected subscriber")
		default:
		}
	}
	mu.Lock()
	fail = false
	window = "second"
	mu.Unlock()
	awaitNotificationSignal(t, one.wake)
	awaitNotificationSignal(t, two.ready)
	// A failed handler SELECT retries after a shared poll, even without a change.
	h.retry(one)
	awaitNotificationSignal(t, one.wake)
}
