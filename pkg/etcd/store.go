package etcd

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"maps"
	"strings"
	"sync"
)

type Store[T common.Equaler] interface {
	GetAll() map[string]T
	GetItem(itemID string) (item T, exists bool)
}

type Item[T any] struct {
	ID    string
	Value T
}

type ItemChange[T any] struct {
	ID       string
	Previous T
	Current  T
}

type ItemsHandlers[T any] struct {
	OnChanged func(changes []ItemChange[T])
	OnAdded   func(added []Item[T])
	OnRemoved func(removed []Item[T])
}

type storeBase[T common.Equaler] struct {
	client   Client
	data     map[string]T // itemID -> item
	handlers ItemsHandlers[T]
	sync.Mutex
}

func (s *storeBase[T]) GetAll() map[string]T {
	s.Lock()
	defer s.Unlock()

	return maps.Clone(s.data)
}

func (s *storeBase[T]) GetItem(itemID string) (item T, itemExists bool) {
	s.Lock()
	defer s.Unlock()

	item, itemExists = s.data[itemID]
	return
}

func (s *storeBase[T]) syncToInternalState(truth map[string]T) (changes []ItemChange[T], added []Item[T], removed []Item[T]) {
	changes = []ItemChange[T]{}
	added = []Item[T]{}
	removed = []Item[T]{}

	for id, item := range truth {
		previousItem, exists := s.data[id]
		if exists {
			if !previousItem.Equals(item) {
				changes = append(changes, ItemChange[T]{
					ID:       id,
					Previous: previousItem,
					Current:  item,
				})
			}
		} else {
			added = append(added, Item[T]{
				ID:    id,
				Value: item,
			})
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

func (s *storeBase[T]) parseItem(kv *mvccpb.KeyValue, prefix string) (itemID string, item T, ignored bool, err error) {
	key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
	parts := strings.Split(key, "/")
	if len(parts) != 1 {
		fmt.Printf("Found unexpected key %s under prefix %s. Ignoring...", key, prefix)
		ignored = true
		return
	}
	itemID = parts[0]
	err = json.Unmarshal(kv.Value, &item)
	if err != nil {
		err = errors.WithMessagef(err, "error parsing item %s: err: %+v, value: %+v", itemID, err, string(kv.Value))
		return
	}

	return
}

func (s *storeBase[T]) loadData(connection *Connection) (map[string]T, error) {
	prefix := s.client.GetKey()
	resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, errors.WithMessagef(err, "Error getting data from etcd for prefix %s", prefix)
	}

	result := map[string]T{}

	for _, kv := range resp.Kvs {
		itemID, item, ignored, err := s.parseItem(kv, prefix)
		if err != nil {
			log.Printf("%+v, skipping...", err)
			continue
		}
		if ignored {
			continue
		}

		result[itemID] = item
	}

	return result, nil
}

func (s *storeBase[T]) invokeAddOrChangedHandler(id string, previous T, existed bool, current T) {
	if existed {
		if !previous.Equals(current) {
			if s.handlers.OnChanged != nil {
				s.handlers.OnChanged([]ItemChange[T]{
					{
						ID:       id,
						Previous: previous,
						Current:  current,
					},
				})
			}
		}
	} else {
		if s.handlers.OnAdded != nil {
			s.handlers.OnAdded([]Item[T]{{ID: id, Value: current}})
		}
	}
}
