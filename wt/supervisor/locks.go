package main

import "sync"

// appLocks hands out one mutex per app name so unrelated apps can deploy, restart and edit config concurrently.
type appLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (l *appLocks) get(name string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = map[string]*sync.Mutex{}
	}
	lock := l.locks[name]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[name] = lock
	}
	return lock
}

// lockApp serializes deploy, config, lifecycle, backup and database operations for one app.
func (s *supervisor) lockApp(name string) func() {
	lock := s.deployLocks.get(name)
	lock.Lock()
	return lock.Unlock
}
