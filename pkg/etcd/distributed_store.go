package etcd

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	clientv3 "go.etcd.io/etcd/client/v3"
	"maps"
	"strings"
	"sync"
)

type Item[T any] struct {
	ID    string
	Value T
}

type ItemChange[T any] struct {
	Previous Item[T]
	Current  Item[T]
}

type ItemsHandlers[T any] struct {
	OnChanged func(changes []ItemChange[T])
	OnAdded   func(added []Item[T])
	OnRemoved func(removed []Item[T])
}

// DistributedStore Notes:
// - Because this performs a two-way sync, the handlers will be called even for items we add, update, delete or sync
// These handlers will be called synchronously from within the method, at the end, after all internal and etcd state
// have been properly brought into sync
type DistributedStore[T common.Equaler] interface {
	GetAll() map[string]T // itemID -> item
	GetItem(itemID string) (item T, itemExists bool)
	AddOrUpdateItem(itemID string, item T) error
	DeleteItem(itemID string) error
	Sync(localShardItems map[string]T) error
}

type distributedStore[T common.Equaler] struct {
	client        Client
	localShardKey string
	handlers      ItemsHandlers[T]
	data          map[string]T // itemID -> item
	sync.RWMutex
}

func NewDistributedStore[T common.Equaler](client Client, handlers ItemsHandlers[T]) DistributedStore[T] {
	return &distributedStore[T]{
		client:   client,
		handlers: handlers,
		data:     make(map[string]T),
	}
}

func (s *distributedStore[T]) GetAll() map[string]T { return s.data }
func (s *distributedStore[T]) GetItem(itemID string) (item T, itemExists bool) {
	s.RLock()
	defer s.RUnlock()

	item, itemExists = s.data[itemID]
	return
}

func (s *distributedStore[T]) AddOrUpdateItem(itemID string, item T) error {
	s.Lock()
	defer s.Unlock()

	previousItem, exists := s.data[itemID]
	currentItem := Item[T]{
		ID:    itemID,
		Value: item,
	}
	s.data[itemID] = item

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		_, err := s.storeItem(connection, itemID, item)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "failed to store item %s", itemID)
		}

		return struct{}{}, nil
	})

	if err != nil {
		return err
	}

	if exists {
		if !previousItem.Equals(item) {
			if s.handlers.OnChanged != nil {
				s.handlers.OnChanged([]ItemChange[T]{{
					Previous: Item[T]{ID: itemID, Value: previousItem},
					Current:  currentItem,
				}})
			}
		}
	} else {
		if s.handlers.OnAdded != nil {
			s.handlers.OnAdded([]Item[T]{currentItem})
		}
	}

	return nil
}

func (s *distributedStore[T]) DeleteItem(itemID string) error {
	s.Lock()
	defer s.Unlock()

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

func (s *distributedStore[T]) Sync(items map[string]T) error {
	s.Lock()
	defer s.Unlock()

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

	if err != nil {
		return err
	}

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

func (s *distributedStore[T]) syncToInternalState(truth map[string]T) (changes []ItemChange[T], added []Item[T], removed []Item[T]) {
	changes = []ItemChange[T]{}
	added = []Item[T]{}
	removed = []Item[T]{}

	for id, item := range truth {
		previousItem, exists := s.data[id]
		currentItem := Item[T]{
			ID:    id,
			Value: item,
		}
		if exists {
			if !previousItem.Equals(item) {
				changes = append(changes, ItemChange[T]{
					Previous: Item[T]{ID: id, Value: previousItem},
					Current:  currentItem,
				})
			}
		} else {
			added = append(added, currentItem)
		}
	}

	toBeDeletedFromInternalState, _ := lo.Difference(lo.Keys(s.data), lo.Keys(truth))
	for _, id := range toBeDeletedFromInternalState {
		previousItem := s.data[id]
		removed = append(removed, Item[T]{ID: id, Value: previousItem})
	}

	s.data = maps.Clone(truth)

	return changes, added, removed
}

func (s *distributedStore[T]) loadData(connection *Connection) (map[string]T, error) {
	prefix := s.client.GetKey()
	resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, errors.WithMessagef(err, "Error getting data from etcd for prefix %s", prefix)
	}

	result := map[string]T{}

	for _, kv := range resp.Kvs {
		key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
		parts := strings.Split(key, "/")
		if len(parts) != 1 {
			fmt.Printf("Found unexpected key %s under prefix %s", key, prefix)
			continue
		}
		itemID := parts[0]
		var item T
		err = json.Unmarshal(kv.Value, &item)
		if err != nil {
			return nil, errors.WithMessagef(err, "error parsing item %s: err: %+v, value: %+v", itemID, err, string(kv.Value))
		}

		result[itemID] = item
	}

	return result, nil
}

func (s *distributedStore[T]) storeItem(connection *Connection, itemID string, item T) (wasWritten bool, err error) {
	bytes, err := json.Marshal(item)
	if err != nil {
		return false, errors.WithMessagef(err, "Failed to serialize item %s. value: %+v", itemID, item)
	}
	wasWritten, err = connection.PutIfNewOrChanged(s.client.GetKey(itemID), string(bytes))
	return
}
