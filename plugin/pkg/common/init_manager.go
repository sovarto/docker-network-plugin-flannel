package common

import (
	"context"
	"sync"
)

type InitManager struct {
	once     sync.Once
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	err      error
	initFunc func(ctx context.Context) error
}

// NewInitManager creates a new InitManager with the provided initialization callback.
// The initFunc should perform the actual initialization and respect context cancellation.
func NewInitManager(initFunc func(ctx context.Context) error) *InitManager {
	return &InitManager{
		done:     make(chan struct{}),
		initFunc: initFunc,
	}
}

// Init starts the initialization process in the background.
// It ensures that initialization runs only once, even if called multiple times.
func (m *InitManager) Init() {
	m.once.Do(func() {
		// Create a cancellable context for the initialization process.
		m.ctx, m.cancel = context.WithCancel(context.Background())

		// Start the initialization in a separate goroutine.
		go func() {
			defer close(m.done)
			// Execute the user-provided initialization function.
			if err := m.initFunc(m.ctx); err != nil {
				m.mu.Lock()
				m.err = err
				m.mu.Unlock()
			}
		}()
	})
}

// IsDone checks if the initialization has completed.
// It returns true if done, false otherwise.
func (m *InitManager) IsDone() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// Wait blocks until the initialization is complete or the context is canceled.
// It returns nil if initialization succeeds, or an error if it fails or was canceled.
func (m *InitManager) Wait() error {
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

// Cancel requests cancellation of the ongoing initialization process.
// If the initialization is already complete or canceled, it has no effect.
func (m *InitManager) Cancel() {
	// Only call cancel if it's been initialized.
	if m.cancel != nil {
		m.cancel()
	}
}
