package common

import (
	"github.com/pkg/errors"
	"golang.org/x/exp/maps"
	"sync"
)

var (
	ErrConflict = errors.New("both keys exist with different associated values")
)

type ConcurrentDualKeyMap[T any, K1, K2 comparable, V comparable] struct {
	mu      sync.RWMutex
	m1      map[K1]V
	m2      map[K2]V
	getKey1 func(T) K1
	getKey2 func(T) K2
}

func NewConcurrentDualKeyMap[T any, K1, K2 comparable, V comparable](getKey1 func(T) K1, getKey2 func(T) K2) *ConcurrentDualKeyMap[T, K1, K2, V] {
	return &ConcurrentDualKeyMap[T, K1, K2, V]{
		m1:      make(map[K1]V),
		m2:      make(map[K2]V),
		getKey1: getKey1,
		getKey2: getKey2,
	}
}

func (dkm *ConcurrentDualKeyMap[T, K1, K2, V]) Set(key T, value V) {
	k1 := dkm.getKey1(key)
	k2 := dkm.getKey2(key)

	dkm.mu.Lock()
	defer dkm.mu.Unlock()

	var zeroK1 K1
	var zeroK2 K2

	if k1 != zeroK1 {
		dkm.m1[k1] = value
	}
	if k2 != zeroK2 {
		dkm.m2[k2] = value
	}
}

func (dkm *ConcurrentDualKeyMap[T, K1, K2, V]) Get(key T) (value V, exists bool, err error) {
	k1 := dkm.getKey1(key)
	k2 := dkm.getKey2(key)

	dkm.mu.Lock()
	defer dkm.mu.Unlock()

	v1, ok1 := dkm.m1[k1]
	v2, ok2 := dkm.m2[k2]

	switch {
	case ok1 && ok2:
		if v1 != v2 {
			var zero V
			return zero, true, ErrConflict
		}
		return v1, true, nil
	case ok1:
		var zeroK2 K2
		if k2 != zeroK2 {
			dkm.m2[k2] = v1
		}
		return v1, true, nil
	case ok2:
		var zeroK1 K1
		if k1 != zeroK1 {
			dkm.m1[k1] = v2
		}
		return v2, true, nil
	default:
		var zero V
		return zero, false, nil
	}
}

func (dkm *ConcurrentDualKeyMap[T, K1, K2, V]) Count() int {
	dkm.mu.RLock()
	defer dkm.mu.RUnlock()

	return Max(len(dkm.m1), len(dkm.m2))
}

func (dkm *ConcurrentDualKeyMap[T, K1, K2, V]) Remove(key T) error {
	k1 := dkm.getKey1(key)
	k2 := dkm.getKey2(key)

	dkm.mu.Lock()
	defer dkm.mu.Unlock()

	v1, ok1 := dkm.m1[k1]
	v2, ok2 := dkm.m2[k2]

	switch {
	case ok1 && ok2:
		if v1 != v2 {
			return ErrConflict
		}
		// Both keys map to the same value. Remove both associations.
		delete(dkm.m1, k1)
		delete(dkm.m2, k2)
		return nil
	case ok1:
		delete(dkm.m1, k1)
		return nil
	case ok2:
		delete(dkm.m2, k2)
		return nil
	default:
		return nil
	}
}

func (dkm *ConcurrentDualKeyMap[T, K1, K2, V]) Keys() ([]K1, []K2) {
	return maps.Keys(dkm.m1), maps.Keys(dkm.m2)
}
