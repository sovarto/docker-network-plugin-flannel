package etcd

import (
	"encoding/json"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
)

type WriteOnlyStore[T common.Equaler] interface {
	GetAll() map[string]T // itemID -> item
	GetItem(itemID string) (item T, itemExists bool)
	AddOrUpdateItem(itemID string, item T) error
	DeleteItem(itemID string) error
	Sync(items map[string]T) error
	Init(items map[string]T) error
	InitFromEtcd() error
}

type writeOnlyStory[T common.Equaler] struct {
	storeBase[T]
}

func NewWriteOnlyStore[T common.Equaler](client Client, handlers ItemsHandlers[T]) WriteOnlyStore[T] {
	return &writeOnlyStory[T]{
		storeBase: storeBase[T]{
			client:   client,
			handlers: handlers,
			data:     make(map[string]T),
		},
	}
}

func (s *writeOnlyStory[T]) AddOrUpdateItem(itemID string, item T) error {
	s.Lock()

	previousItem, exists := s.data[itemID]
	s.data[itemID] = item

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		_, err := s.storeItem(connection, itemID, item)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "failed to store item %s", itemID)
		}

		return struct{}{}, nil
	})

	s.Unlock()

	if err != nil {
		return err
	}

	if exists {
		if !previousItem.Equals(item) {
			if s.handlers.OnChanged != nil {
				s.handlers.OnChanged([]ItemChange[T]{{
					ID:       itemID,
					Previous: previousItem,
					Current:  item,
				}})
			}
		}
	} else {
		if s.handlers.OnAdded != nil {
			s.handlers.OnAdded([]Item[T]{{
				ID:    itemID,
				Value: item,
			}})
		}
	}

	return nil
}

func (s *writeOnlyStory[T]) DeleteItem(itemID string) error {
	s.Lock()

	previousItem, exists := s.data[itemID]
	delete(s.data, itemID)

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		key := s.client.GetKey(itemID)
		_, err := connection.Client.Delete(connection.Ctx, key)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "failed to delete item %s", key)
		}

		return struct{}{}, nil
	})

	s.Unlock()

	if err != nil {
		return err
	}

	if exists {
		if s.handlers.OnRemoved != nil {
			s.handlers.OnRemoved([]Item[T]{{ID: itemID, Value: previousItem}})
		}
	}

	return nil
}

func (s *writeOnlyStory[T]) Init(items map[string]T) error { return s.sync(items, true) }
func (s *writeOnlyStory[T]) Sync(items map[string]T) error { return s.sync(items, true) }
func (s *writeOnlyStory[T]) sync(items map[string]T, callHandlers bool) error {
	s.Lock()

	changes, added, removed := s.syncToInternalState(items)

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		etcdData, err := s.loadData(connection)
		if err != nil {
			return struct{}{}, err
		}

		toBeDeletedFromEtcd, _ := lo.Difference(lo.Keys(etcdData), lo.Keys(items))
		for id, item := range items {
			_, err = s.storeItem(connection, id, item)
			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "failed to store item %s", id)
			}
		}
		for _, id := range toBeDeletedFromEtcd {
			key := s.client.GetKey(id)
			_, err = connection.Client.Delete(connection.Ctx, key)
			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "failed to delete item %s", key)
			}
		}

		return struct{}{}, nil
	})

	s.Unlock()

	if err != nil {
		return err
	}

	if callHandlers {
		if len(added) > 0 && s.handlers.OnAdded != nil {
			s.handlers.OnAdded(added)
		}
		if len(changes) > 0 && s.handlers.OnChanged != nil {
			s.handlers.OnChanged(changes)
		}
		if len(removed) > 0 && s.handlers.OnRemoved != nil {
			s.handlers.OnRemoved(removed)
		}
	}

	return nil
}

func (s *writeOnlyStory[T]) InitFromEtcd() error {
	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		etcdData, err := s.loadData(connection)
		if err != nil {
			return struct{}{}, err
		}
		s.data = etcdData
		return struct{}{}, nil
	})

	return err
}

func (s *writeOnlyStory[T]) storeItem(connection *Connection, itemID string, item T) (wasWritten bool, err error) {
	bytes, err := json.Marshal(item)
	if err != nil {
		return false, errors.WithMessagef(err, "Failed to serialize item %s. value: %+v", itemID, item)
	}
	wasWritten, err = connection.PutIfNewOrChanged(s.client.GetKey(itemID), string(bytes))
	return
}
