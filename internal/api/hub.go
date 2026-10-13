package api

import (
	"sync"
	"time"
)

// Hub wakes event readers; the store remains the source of truth.
type Hub struct {
	mu                sync.Mutex
	subs              map[string]map[chan struct{}]struct{}
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
}

func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[chan struct{}]struct{}), PollInterval: time.Second, HeartbeatInterval: 15 * time.Second}
}

func (h *Hub) Notify(attemptID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[attemptID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Hub) subscribe(attemptID string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{}, 1)
	if h.subs == nil {
		h.subs = make(map[string]map[chan struct{}]struct{})
	}
	if h.subs[attemptID] == nil {
		h.subs[attemptID] = make(map[chan struct{}]struct{})
	}
	h.subs[attemptID][ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs[attemptID], ch)
		if len(h.subs[attemptID]) == 0 {
			delete(h.subs, attemptID)
		}
	}
}

func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, sub := range h.subs {
		n += len(sub)
	}
	return n
}
