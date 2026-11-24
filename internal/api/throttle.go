package api

import (
	"container/list"
	"slices"
	"sync"
	"time"
)

// Sign-in throttling, in memory: per account a doubling delay after five consecutive failures,
// per address a block after twenty failures in ten minutes.
const (
	accountFreeFailures = 5
	accountFirstDelay   = time.Second
	accountMaxDelay     = 15 * time.Minute
	addressFailures     = 20
	addressWindow       = 10 * time.Minute
	addressBlock        = 15 * time.Minute
	addressKept         = 64 // failure times kept per address
	throttleTracked     = 10000
)

type accountThrottle struct {
	failures int
	latest   time.Time // the latest failure of the streak, whatever order they were recorded in
	until    time.Time
	inFlight int // password checks running, each a failure until it ends
}

type addressThrottle struct {
	failures []time.Time // in time order
	until    time.Time
	inFlight int
	starts   []time.Time // when each check in flight was admitted: no failure of it comes earlier
}

// accountDelay is the wait after the failures-th consecutive failure.
func accountDelay(failures int) time.Duration {
	extra := failures - accountFreeFailures
	if extra < 0 {
		return 0
	}
	if extra >= 10 {
		return accountMaxDelay
	}
	return min(accountFirstDelay<<extra, accountMaxDelay)
}

// throttle is shared by the network channel's sign-ins and the owner's recovery.
type throttle struct {
	mu        sync.Mutex
	accounts  lru[*accountThrottle]
	addresses lru[*addressThrottle]
}

// wait is how long the account or the address must still wait; zero allows the attempt. An empty
// account checks the address alone. A password check in flight counts as a failure: should the
// failures and the checks running reach a threshold, the next attempt waits as if they had failed.
func (t *throttle) wait(account, address string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.waitLocked(account, address, now)
}

func (t *throttle) waitLocked(account, address string, now time.Time) time.Duration {
	var wait time.Duration
	if a, ok := t.addresses.get(address); ok {
		if now.Before(a.until) {
			wait = a.until.Sub(now)
		} else if a.inFlight > 0 && a.recent(now)+a.inFlight >= addressFailures {
			wait = addressBlock
		}
	}
	if account != "" {
		if a, ok := t.accounts.get(account); ok {
			if now.Before(a.until) {
				wait = max(wait, a.until.Sub(now))
			} else if a.inFlight > 0 && a.failures+a.inFlight >= accountFreeFailures {
				wait = max(wait, accountDelay(a.failures+a.inFlight))
			}
		}
	}
	return wait
}

// recent counts the failures inside the window at now.
func (a *addressThrottle) recent(now time.Time) int {
	n := 0
	for _, at := range a.failures {
		if now.Sub(at) < addressWindow {
			n++
		}
	}
	return n
}

// checking is a password check admitted past the throttle; until it ends it counts as a failure
// of its account and address.
type checking struct {
	t       *throttle
	account *accountThrottle
	address *addressThrottle
	names   [2]string
	started time.Time
	ended   bool
}

// admit lets a password check start when nothing must wait, counting it in flight.
func (t *throttle) admit(account, address string, now time.Time) (*checking, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if wait := t.waitLocked(account, address, now); wait > 0 {
		return nil, wait
	}
	c := &checking{t: t, names: [2]string{account, address}, started: now}
	if account != "" {
		c.account = t.accounts.getOrAdd(account, func() *accountThrottle { return &accountThrottle{} })
		c.account.inFlight++
	}
	c.address = t.addresses.getOrAdd(address, func() *addressThrottle { return &addressThrottle{} })
	c.address.inFlight++
	c.address.starts = append(c.address.starts, now)
	return c, 0
}

func (c *checking) endLocked() bool {
	if c.ended {
		return false
	}
	c.ended = true
	if c.account != nil {
		c.account.inFlight--
	}
	c.address.inFlight--
	if i := slices.IndexFunc(c.address.starts, c.started.Equal); i >= 0 {
		c.address.starts = slices.Delete(c.address.starts, i, i+1)
	}
	return true
}

// failed replaces the check's provisional failure with a real one at now.
func (c *checking) failed(now time.Time) {
	c.t.mu.Lock()
	defer c.t.mu.Unlock()
	if c.endLocked() {
		c.t.failLocked(c.names[0], c.names[1], now)
	}
}

// wait is how long the check's account or address must wait now, its own reservation aside.
func (c *checking) wait(now time.Time) time.Duration {
	c.t.mu.Lock()
	defer c.t.mu.Unlock()
	if !c.ended {
		if c.account != nil {
			c.account.inFlight--
			defer func() { c.account.inFlight++ }()
		}
		c.address.inFlight--
		defer func() { c.address.inFlight++ }()
	}
	return c.t.waitLocked(c.names[0], c.names[1], now)
}

// done ends the check without a failure.
func (c *checking) done() {
	c.t.mu.Lock()
	defer c.t.mu.Unlock()
	c.endLocked()
}

func (t *throttle) fail(account, address string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failLocked(account, address, now)
}

func (t *throttle) failLocked(account, address string, now time.Time) {
	if account != "" {
		a := t.accounts.getOrAdd(account, func() *accountThrottle { return &accountThrottle{} })
		a.failures++
		if now.After(a.latest) {
			a.latest = now
		}
		// The delay runs from the streak's latest failure, and only ever moves later.
		if delay := accountDelay(a.failures); delay > 0 && a.latest.Add(delay).After(a.until) {
			a.until = a.latest.Add(delay)
		}
	}
	a := t.addresses.getOrAdd(address, func() *addressThrottle { return &addressThrottle{} })
	a.failures = append(a.failures, now)
	slices.SortFunc(a.failures, time.Time.Compare)
	// Every failure ends a ten-minute window, whatever order failures were recorded in: twenty in
	// one block the address from its end, the block only ever moving later.
	first := 0
	for end, f := range a.failures {
		for f.Sub(a.failures[first]) >= addressWindow {
			first++
		}
		if end-first+1 >= addressFailures && f.Add(addressBlock).After(a.until) {
			a.until = f.Add(addressBlock)
		}
	}
	// Keep the window before the latest failure, or before the earliest check still running,
	// whose failure may come later and need it.
	horizon := a.failures[len(a.failures)-1]
	for _, s := range a.starts {
		if s.Before(horizon) {
			horizon = s
		}
	}
	horizon = horizon.Add(-addressWindow)
	keep := len(a.failures) - min(len(a.failures), addressKept)
	for keep < len(a.failures) && a.failures[keep].Before(horizon) {
		keep++
	}
	a.failures = slices.Clone(a.failures[keep:])
}

// clear forgets the account's failures: a success or the owner's recovery.
func (t *throttle) clear(account string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Reset in place: checks of the account still running keep their reservations.
	if a, ok := t.accounts.get(account); ok {
		a.failures, a.latest, a.until = 0, time.Time{}, time.Time{}
	}
}

// lru is a map that drops its least recently seen key past throttleTracked.
type lru[V any] struct {
	limit int // throttleTracked when zero
	order *list.List
	items map[string]*list.Element
}

type lruEntry[V any] struct {
	key   string
	value V
}

func (l *lru[V]) get(key string) (V, bool) {
	if e, ok := l.items[key]; ok {
		l.order.MoveToFront(e)
		return e.Value.(*lruEntry[V]).value, true
	}
	var zero V
	return zero, false
}

func (l *lru[V]) getOrAdd(key string, create func() V) V {
	if v, ok := l.get(key); ok {
		return v
	}
	if l.items == nil {
		l.items, l.order = map[string]*list.Element{}, list.New()
	}
	limit := l.limit
	if limit <= 0 {
		limit = throttleTracked
	}
	for l.order.Len() >= limit {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.items, oldest.Value.(*lruEntry[V]).key)
	}
	v := create()
	l.items[key] = l.order.PushFront(&lruEntry[V]{key, v})
	return v
}

func (l *lru[V]) len() int { return len(l.items) }
