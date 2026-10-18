package app

import (
	"context"
	"sync"
)

// runMutex is a mutex whose wait can be abandoned through a context. The zero
// value is an unlocked mutex. It must not be copied after first use.
type runMutex struct {
	once sync.Once
	slot chan struct{}
}

func (m *runMutex) init() {
	m.once.Do(func() { m.slot = make(chan struct{}, 1) })
}

func (m *runMutex) Lock() {
	m.init()
	m.slot <- struct{}{}
}

// LockCtx takes the lock, or returns ctx's error without the lock once ctx is done.
func (m *runMutex) LockCtx(ctx context.Context) error {
	m.init()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock on an unlocked mutex panics, like sync.Mutex.
func (m *runMutex) Unlock() {
	m.init()
	select {
	case <-m.slot:
	default:
		panic("app: unlock of unlocked runMutex")
	}
}
