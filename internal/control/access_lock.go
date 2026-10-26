package control

import "sync"

// accessMu is exclusive for administrative changes that touch accounts, plans,
// keys or sources. Authorization, billing and pacing for one account hold it
// shared plus that account's own lock, so different accounts run in parallel.
type accountLocks struct {
	mu sync.Mutex
	m  map[string]*accountLock
}
type accountLock struct {
	sync.Mutex
	refs int
}

func (l *accountLocks) acquire(id string) *accountLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*accountLock{}
	}
	e := l.m[id]
	if e == nil {
		e = &accountLock{}
		l.m[id] = e
	}
	e.refs++
	return e
}
func (l *accountLocks) release(id string, e *accountLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.refs--; e.refs == 0 {
		delete(l.m, id)
	}
}

func (a *App) lockAccount(id string) func() {
	a.accessMu.RLock()
	e := a.accountLocks.acquire(id)
	e.Lock()
	return func() {
		e.Unlock()
		a.accountLocks.release(id, e)
		a.accessMu.RUnlock()
	}
}
