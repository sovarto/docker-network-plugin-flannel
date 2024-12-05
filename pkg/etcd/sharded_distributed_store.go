package etcd

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/exp/maps"
	"log"
	"strings"
	"sync"
)

type ShardItem[T any] struct {
	ShardKey string
	ID       string
	Value    T
}

type ShardItemChange[T any] struct {
	ShardKey string
	ID       string
	Previous T
	Current  T
}

type ShardItemsHandlers[T any] struct {
	OnChanged func(changes []ShardItemChange[T])
	OnAdded   func(added []ShardItem[T])
	OnRemoved func(removed []ShardItem[T])
}

// TODO: Make use of ReadOnlyStore and WriteOnlyStore internally for the shards, instead of re-implementing all that logic

// ShardedDistributedStore Notes:
// - Even though items are placed below the shard keys, itemIDs need to be unique across all shards
// - Because this performs a two-way sync, the handlers will be called even for items we add, update, delete or sync
// These handlers will be called synchronously from within the method, at the end, after all internal and etcd state
// have been properly brought into sync
type ShardedDistributedStore[T common.Equaler] interface {
	GetAll() map[string]map[string]T // shardKey -> itemID -> item
	GetLocalShardKey() string
	GetShard(key string) (shardItems map[string]T, shardExists bool) // itemID -> item
	GetItem(itemID string) (shardKey string, item T, itemExists bool)
	// AddOrUpdateItem Will always add to the local shard
	AddOrUpdateItem(itemID string, item T) error
	DeleteItem(itemID string) error
	Sync(localShardItems map[string]T) error
	Init(localShardItems map[string]T) error
}

type shardedDistributedStore[T common.Equaler] struct {
	client         Client
	localShardKey  string
	handlers       ShardItemsHandlers[T]
	shardedData    map[string]map[string]T // shardKey -> itemID -> item
	data           map[string]T            // itemID -> item
	itemToShardKey map[string]string       // itemID -> shardKey
	sync.Mutex
}

func NewShardedDistributedStore[T common.Equaler](client Client, localShardKey string, handlers ShardItemsHandlers[T]) ShardedDistributedStore[T] {
	shardedData := make(map[string]map[string]T)
	shardedData[localShardKey] = make(map[string]T)
	return &shardedDistributedStore[T]{
		client:         client,
		localShardKey:  localShardKey,
		handlers:       handlers,
		shardedData:    shardedData,
		data:           make(map[string]T),
		itemToShardKey: make(map[string]string),
	}
}

func (s *shardedDistributedStore[T]) Init(localShardItems map[string]T) error {
	err := s.sync(localShardItems, false)
	if err != nil {
		return err
	}

	_, _, err = s.client.Watch(s.client.GetKey(), true, s.handleWatchEvents)
	if err != nil {
		return errors.WithMessagef(err, "Couldn't start watcher for %s", s.client.GetKey())
	}

	return nil
}

func (s *shardedDistributedStore[T]) GetAll() map[string]map[string]T { return s.shardedData }
func (s *shardedDistributedStore[T]) GetLocalShardKey() string        { return s.localShardKey }

func (s *shardedDistributedStore[T]) GetShard(key string) (shardItems map[string]T, shardExists bool) {
	s.Lock()
	defer s.Unlock()

	shardItems, shardExists = s.shardedData[key]
	return
}

func (s *shardedDistributedStore[T]) GetItem(itemID string) (shardKey string, item T, itemExists bool) {
	s.Lock()
	defer s.Unlock()

	item, itemExists = s.data[itemID]
	shardKey = s.itemToShardKey[itemID]
	return
}

func (s *shardedDistributedStore[T]) AddOrUpdateItem(itemID string, item T) error {
	s.Lock()
	defer s.Unlock()

	shardKey := s.localShardKey
	shardItems := s.shardedData[shardKey]

	previousItem, exists := shardItems[itemID]

	shardItems[itemID] = item
	s.data[itemID] = item
	s.itemToShardKey[itemID] = shardKey

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		_, err := s.storeItem(connection, shardKey, itemID, item)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "failed to store item %s for shard %s", itemID, shardKey)
		}

		return struct{}{}, nil
	})

	if err != nil {
		return err
	}

	if exists {
		if !previousItem.Equals(item) {
			if s.handlers.OnChanged != nil {
				s.handlers.OnChanged([]ShardItemChange[T]{{ShardKey: shardKey, ID: itemID, Previous: previousItem, Current: item}})
			}
		}
	} else {
		if s.handlers.OnAdded != nil {
			s.handlers.OnAdded([]ShardItem[T]{{ShardKey: shardKey, ID: itemID, Value: item}})
		}
	}

	return nil
}

func (s *shardedDistributedStore[T]) DeleteItem(itemID string) error {
	s.Lock()
	defer s.Unlock()

	shardKey := s.localShardKey
	shardItems := s.shardedData[shardKey]

	previousItem, exists := shardItems[itemID]
	delete(shardItems, itemID)
	delete(s.data, itemID)
	delete(s.itemToShardKey, itemID)

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		key := s.client.GetKey(shardKey, itemID)
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
			s.handlers.OnRemoved([]ShardItem[T]{{ID: itemID, Value: previousItem, ShardKey: shardKey}})
		}
	}

	return nil
}

func (s *shardedDistributedStore[T]) Sync(localShardItems map[string]T) error {
	return s.sync(localShardItems, true)
}

func (s *shardedDistributedStore[T]) sync(localShardItems map[string]T, callHandlers bool) error {
	s.Lock()
	defer s.Unlock()

	changes, added, removed := s.syncToInternalState(s.localShardKey, localShardItems)

	_, err := WithConnection(s.client, func(connection *Connection) (struct{}, error) {
		etcdData, err := s.loadData(connection)
		if err != nil {
			return struct{}{}, err
		}

		for shardKey, shardItems := range etcdData {
			if shardKey != s.localShardKey {
				localChanges, localAdded, localRemoved := s.syncToInternalState(shardKey, shardItems)
				changes = append(changes, localChanges...)
				added = append(added, localAdded...)
				removed = append(removed, localRemoved...)
			} else {
				toBeDeletedFromEtcd, _ := lo.Difference(lo.Keys(shardItems), lo.Keys(localShardItems))
				for id, item := range localShardItems {
					_, err = s.storeItem(connection, shardKey, id, item)
					if err != nil {
						return struct{}{}, errors.WithMessagef(err, "failed to store item %s for shard %s", id, shardKey)
					}
				}
				for _, id := range toBeDeletedFromEtcd {
					key := s.client.GetKey(shardKey, id)
					_, err = connection.Client.Delete(connection.Ctx, key)
					if err != nil {
						return struct{}{}, errors.WithMessagef(err, "failed to delete item %s", key)
					}
				}
			}
		}

		return struct{}{}, nil
	})

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

func (s *shardedDistributedStore[T]) syncToInternalState(shardKey string, truth map[string]T) (changes []ShardItemChange[T], added []ShardItem[T], removed []ShardItem[T]) {
	changes = []ShardItemChange[T]{}
	added = []ShardItem[T]{}
	removed = []ShardItem[T]{}

	shardItems := s.shardedData[shardKey]

	for id, item := range truth {
		previousItem, exists := shardItems[id]
		if exists {
			if !previousItem.Equals(item) {
				changes = append(changes, ShardItemChange[T]{
					ID:       id,
					ShardKey: shardKey,
					Previous: previousItem,
					Current:  item,
				})
			}
		} else {
			added = append(added, ShardItem[T]{
				ShardKey: shardKey,
				ID:       id,
				Value:    item,
			})
		}
	}

	toBeDeletedFromInternalState, _ := lo.Difference(lo.Keys(shardItems), lo.Keys(truth))
	for _, id := range toBeDeletedFromInternalState {
		previousItem := shardItems[id]
		removed = append(removed, ShardItem[T]{ID: id, Value: previousItem, ShardKey: shardKey})
	}

	s.shardedData[shardKey] = maps.Clone(truth)
	s.data = map[string]T{}
	for shardKey, shardItems := range s.shardedData {
		for id, item := range shardItems {
			s.data[id] = item
			s.itemToShardKey[id] = shardKey
		}
	}

	return changes, added, removed
}

func (s *shardedDistributedStore[T]) loadData(connection *Connection) (map[string]map[string]T, error) {
	prefix := s.client.GetKey()
	resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, errors.WithMessagef(err, "Error getting data from etcd for prefix %s", prefix)
	}

	result := map[string]map[string]T{}

	for _, kv := range resp.Kvs {
		shardKey, itemID, item, ignored, err := s.parseItem(kv, prefix)
		if err != nil {
			return nil, err
		}
		if ignored {
			continue
		}

		setShardedItem(result, shardKey, itemID, item)
	}

	return result, nil
}

func (s *shardedDistributedStore[T]) storeItem(connection *Connection, shardKey, itemID string, item T) (wasWritten bool, err error) {
	bytes, err := json.Marshal(item)
	if err != nil {
		return false, errors.WithMessagef(err, "Failed to serialize item %s for shard %s. value: %+v", itemID, shardKey, item)
	}
	wasWritten, err = connection.PutIfNewOrChanged(s.client.GetKey(shardKey, itemID), string(bytes))
	return
}

func (s *shardedDistributedStore[T]) parseItem(kv *mvccpb.KeyValue, prefix string) (shardKey, itemID string, item T, ignored bool, err error) {
	key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		fmt.Printf("Found unexpected key %s under prefix %s. Ignoring...\n", key, prefix)
		ignored = true
		return
	}
	shardKey = parts[0]
	itemID = parts[1]
	err = json.Unmarshal(kv.Value, &item)
	if err != nil {
		err = errors.WithMessagef(err, "error parsing item %s for shard %s: err: %+v, value: %+v, prefix: %s", itemID, shardKey, err, string(kv.Value), prefix)
		return
	}

	return
}

func (s *shardedDistributedStore[T]) handleWatchEvents(watcher clientv3.WatchChan, prefix string) {
	for wresp := range watcher {
		for _, ev := range wresp.Events {
			shardKey, itemID, item, ignored, err := s.parseItem(ev.Kv, prefix)
			if err != nil {
				log.Printf("Error in event watcher: %+v", err)
			}
			if ignored {
				continue
			}
			if shardKey == s.localShardKey {
				// Ignore for now
				// TODO: Check if item change matches our internal state
				// - if yes: ignore
				// - if not: print error message as this shouldn't happen
			} else {
				if ev.Type == mvccpb.DELETE {
					shardedItems, exists := s.shardedData[shardKey]
					if exists {
						delete(shardedItems, itemID)
					}
					delete(s.data, itemID)
					delete(s.itemToShardKey, itemID)
					if s.handlers.OnRemoved != nil {
						s.handlers.OnRemoved([]ShardItem[T]{{ID: itemID, Value: item, ShardKey: shardKey}})
					}
				} else if ev.Type == mvccpb.PUT {
					shardedItems, shardExists := s.shardedData[shardKey]
					if !shardExists {
						shardedItems = make(map[string]T)
						s.shardedData[shardKey] = shardedItems
					}
					previousItem, exists := shardedItems[itemID]
					shardedItems[itemID] = item
					if exists {
						if s.handlers.OnChanged != nil && !previousItem.Equals(item) {
							s.handlers.OnChanged([]ShardItemChange[T]{{
								ShardKey: shardKey,
								ID:       itemID,
								Previous: previousItem,
								Current:  item,
							}})
						}
					} else {
						if s.handlers.OnAdded != nil {
							s.handlers.OnAdded([]ShardItem[T]{{ShardKey: shardKey, ID: itemID, Value: item}})
						}
					}
				}
			}
		}
	}
}

func setShardedItem[T any](dst map[string]map[string]T, shardKey, itemID string, item T) {
	shard, exists := dst[shardKey]
	if !exists {
		shard = map[string]T{}
		dst[shardKey] = shard
	}
	shard[itemID] = item
}
