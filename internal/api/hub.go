package api

import (
	"context"
	"sync"
	"time"
)

// Hub wakes event readers; the store remains the source of truth.
type Hub struct {
	mu                sync.Mutex
	closeOnce         sync.Once
	done              chan struct{}
	subs              map[string]map[chan struct{}]struct{}
	streamUsers       map[string]int
	streamTotal       int
	budgetUsed        int
	budgetWake        chan struct{}
	BudgetBytes       int
	WriteDeadline     time.Duration
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
}

func NewHub() *Hub {
	return &Hub{done: make(chan struct{}), subs: make(map[string]map[chan struct{}]struct{}), PollInterval: time.Second, HeartbeatInterval: 15 * time.Second, BudgetBytes: 32 << 20, WriteDeadline: 10 * time.Second}
}

func (h *Hub) reserveStream(user string) (func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streamUsers == nil {
		h.streamUsers = make(map[string]int)
	}
	if h.streamUsers[user] >= 16 || h.streamTotal >= 128 {
		return nil, false
	}
	h.streamUsers[user]++
	h.streamTotal++
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.streamUsers[user]--
		h.streamTotal--
	}, true
}

func (h *Hub) reserveBytes(ctx context.Context, n int) bool {
	for {
		h.mu.Lock()
		if h.budgetWake == nil {
			h.budgetWake = make(chan struct{})
		}
		capacity := h.BudgetBytes
		if capacity <= 0 {
			capacity = 32 << 20
		}
		if h.budgetUsed+n <= capacity {
			h.budgetUsed += n
			h.mu.Unlock()
			return true
		}
		wake := h.budgetWake
		done := h.done
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-done:
			return false
		case <-wake:
		}
	}
}

func (h *Hub) releaseBytes(n int) {
	if n == 0 {
		return
	}
	h.mu.Lock()
	h.budgetUsed -= n
	if h.budgetWake != nil {
		close(h.budgetWake)
		h.budgetWake = make(chan struct{})
	}
	h.mu.Unlock()
}

// Close stops live event streams. Repeated calls are safe.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.done == nil {
		h.done = make(chan struct{})
	}
	h.mu.Unlock()
	h.closeOnce.Do(func() { close(h.done) })
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
	if h.done == nil {
		h.done = make(chan struct{})
	}
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
