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
	drainOnce         sync.Once
	done              chan struct{}
	draining          chan struct{} // closed by Drain: open streams send what is committed and end
	subs              map[string]map[chan struct{}]struct{}
	streamUsers       map[string]int
	streamTotal       int
	budgetUsed        int
	budgetQueue       []*budgetWaiter // readers waiting for budget, in arrival order
	BudgetBytes       int
	WriteDeadline     time.Duration
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
}

func NewHub() *Hub {
	return &Hub{done: make(chan struct{}), draining: make(chan struct{}), subs: make(map[string]map[chan struct{}]struct{}), PollInterval: time.Second, HeartbeatInterval: 15 * time.Second, BudgetBytes: 32 << 20, WriteDeadline: 10 * time.Second}
}

// channelsLocked gives a zero-value Hub its channels.
func (h *Hub) channelsLocked() {
	if h.done == nil {
		h.done = make(chan struct{})
	}
	if h.draining == nil {
		h.draining = make(chan struct{})
	}
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
		if h.streamUsers[user] == 0 {
			delete(h.streamUsers, user)
		}
		h.streamTotal--
	}, true
}

type budgetWaiter struct {
	n       int
	granted chan struct{} // closed once the bytes are the waiter's
}

func (h *Hub) capacity() int {
	if h.BudgetBytes <= 0 {
		return 32 << 20
	}
	return h.BudgetBytes
}

// reserveBytes takes n bytes of the shared budget, queueing behind earlier
// waiters: a release serves the queue head first, so a later request cannot
// overtake an earlier one even when it is smaller. It returns false, holding
// nothing, when ctx has ended or the hub has closed, whether before the call
// or while a grant was on its way, and at once for n over the whole capacity,
// which no release could ever serve and would block every later waiter.
func (h *Hub) reserveBytes(ctx context.Context, n int) bool {
	h.mu.Lock()
	if n > h.capacity() || ctx.Err() != nil || h.closedLocked() {
		h.mu.Unlock()
		return false
	}
	if len(h.budgetQueue) == 0 && h.budgetUsed+n <= h.capacity() {
		h.budgetUsed += n
		h.mu.Unlock()
		return true
	}
	w := &budgetWaiter{n: n, granted: make(chan struct{})}
	h.budgetQueue = append(h.budgetQueue, w)
	done := h.done
	h.mu.Unlock()
	select {
	case <-w.granted:
	case <-ctx.Done():
	case <-done:
	}
	// A grant and the end of the wait can be ready together: the grant counts
	// only for a waiter whose context and hub are still live, checked under
	// the lock so a later release cannot be double counted.
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-w.granted:
		if ctx.Err() == nil && !h.closedLocked() {
			return true
		}
		h.budgetUsed -= n // granted while leaving: hand the bytes on, once
	default:
		for i, queued := range h.budgetQueue {
			if queued == w {
				h.budgetQueue = append(h.budgetQueue[:i], h.budgetQueue[i+1:]...)
				break
			}
		}
	}
	h.grantLocked()
	return false
}

func (h *Hub) closedLocked() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// grantLocked serves waiters from the head of the queue while they fit. A
// closed hub grants nothing: its waiters leave through done.
func (h *Hub) grantLocked() {
	if h.closedLocked() {
		return
	}
	for len(h.budgetQueue) != 0 {
		w := h.budgetQueue[0]
		if h.budgetUsed+w.n > h.capacity() {
			return
		}
		h.budgetUsed += w.n
		close(w.granted)
		h.budgetQueue[0] = nil
		h.budgetQueue = h.budgetQueue[1:]
	}
}

func (h *Hub) releaseBytes(n int) {
	if n == 0 {
		return
	}
	h.mu.Lock()
	h.budgetUsed -= n
	h.grantLocked()
	h.mu.Unlock()
}

// Drain is the bounded final drain before Close: every open stream sends what
// the store holds and ends, and Drain waits up to the write deadline for them,
// so events committed before it, a stop's reason among them, reach an open
// stream as its last. Repeated calls are safe.
func (h *Hub) Drain() {
	h.mu.Lock()
	h.channelsLocked()
	draining := h.draining
	h.mu.Unlock()
	h.drainOnce.Do(func() { close(draining) })
	deadline := h.WriteDeadline
	if deadline <= 0 {
		deadline = 10 * time.Second
	}
	until := time.Now().Add(deadline)
	for h.SubscriberCount() != 0 && time.Now().Before(until) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Close stops live event streams. Repeated calls are safe.
func (h *Hub) Close() {
	h.mu.Lock()
	h.channelsLocked()
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
	h.channelsLocked()
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
