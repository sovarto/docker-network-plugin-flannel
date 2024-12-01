package etcd

import (
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"strings"
)

// ReadOnlyStore defines the interface for a Read-Only Store
type ReadOnlyStore[T common.Equaler] interface {
	GetAll() map[string]T
	GetItem(itemID string) (item T, exists bool)
	Sync() error
	Init() error
}

type readOnlyStore[T common.Equaler] struct {
	storeBase[T]
}

// NewReadOnlyStore creates a new ReadOnlyStore instance
func NewReadOnlyStore[T common.Equaler](client Client, handlers ItemsHandlers[T]) ReadOnlyStore[T] {
	return &readOnlyStore[T]{
		storeBase: storeBase[T]{
			client:   client,
			data:     make(map[string]T),
			handlers: handlers,
		},
	}
}

// Init initializes the ReadOnlyStore by loading data from etcd and starting the watcher
func (s *readOnlyStore[T]) Init() error {
	err := s.Sync()
	if err != nil {
		return errors.WithMessage(err, "Error performing initial sync")
	}
	_, _, err = s.client.Watch(s.client.GetKey(), true, s.watchEventHandler)
	if err != nil {
		return errors.WithMessagef(err, "ReadOnlyStore: couldn't start watcher for %s: %v", s.client.GetKey(), err)
	}

	return nil
}

// Sync synchronizes the internal state with etcd (etcd is the source of truth)
func (s *readOnlyStore[T]) Sync() error {
	s.Lock()

	etcdData, err := WithConnection(s.client, func(connection *Connection) (map[string]T, error) {
		return s.loadData(connection)
	})

	if err != nil {
		s.Unlock()
		return err
	}

	changes, added, removed := s.syncToInternalState(etcdData)

	// Invoke handlers outside the lock to prevent potential deadlocks
	s.Unlock()
	if len(added) > 0 && s.handlers.OnAdded != nil {
		s.handlers.OnAdded(added)
	}
	if len(changes) > 0 && s.handlers.OnChanged != nil {
		s.handlers.OnChanged(changes)
	}
	if len(removed) > 0 && s.handlers.OnRemoved != nil {
		s.handlers.OnRemoved(removed)
	}

	return nil
}

// watchEtcd watches etcd for changes and updates internal state accordingly
func (s *readOnlyStore[T]) watchEventHandler(watchChan clientv3.WatchChan, prefix string) {
	for wresp := range watchChan {
		for _, ev := range wresp.Events {
			itemID := strings.TrimPrefix(string(ev.Kv.Key), s.client.GetKey()+"/")

			switch ev.Type {
			case clientv3.EventTypePut:
				itemID, item, ignored, err := s.parseItem(ev.Kv, prefix)

				if err != nil {
					log.Printf("Error parsing item %s: %v", itemID, err)
					continue
				}
				if ignored {
					continue
				}

				s.Lock()
				previousItem, exists := s.data[itemID]
				s.data[itemID] = item
				s.Unlock()

				s.invokeAddOrChangedHandler(itemID, previousItem, exists, item)

			case clientv3.EventTypeDelete:
				s.Lock()
				previousItem, exists := s.data[itemID]
				delete(s.data, itemID)
				s.Unlock()

				if exists {
					if s.handlers.OnRemoved != nil {
						s.handlers.OnRemoved([]Item[T]{{ID: itemID, Value: previousItem}})
					}
				}
			}
		}
	}
}
