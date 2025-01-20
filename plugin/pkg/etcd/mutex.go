package etcd

import (
	"context"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/client/v3/concurrency"
)

type Mutex struct {
	session *concurrency.Session
	mutex   *concurrency.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewMutex(session *concurrency.Session, mutex *concurrency.Mutex, ctx context.Context, cancel context.CancelFunc) *Mutex {
	return &Mutex{session: session, mutex: mutex, ctx: ctx, cancel: cancel}
}

func (m *Mutex) UnlockAndCloseSession() error {
	err := m.mutex.Unlock(m.ctx)
	if err != nil {
		return errors.WithMessage(err, "failed to unlock mutex")
	}

	err = m.session.Close()
	if err != nil {
		return errors.WithMessage(err, "failed to close session")
	}

	m.cancel()

	return nil
}
