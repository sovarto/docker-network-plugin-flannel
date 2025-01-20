package common

import (
	"golang.org/x/exp/maps"
	"sync"
)

// ConcurrentMap is a simple concurrent map that provides methods similar to C#'s ConcurrentDictionary.
type ConcurrentMap[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
}

// New creates a new ConcurrentMap.
func NewConcurrentMap[K comparable, V any]() *ConcurrentMap[K, V] {
	return &ConcurrentMap[K, V]{
		m: make(map[K]V),
	}
}

// Get retrieves the value stored with the given key.
// The boolean result indicates whether the key was found.
func (cm *ConcurrentMap[K, V]) Get(key K) (V, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	val, ok := cm.m[key]
	return val, ok
}

// Set stores or updates the value for the given key.
func (cm *ConcurrentMap[K, V]) Set(key K, value V) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.m[key] = value
}

// Remove deletes the value for the given key.
func (cm *ConcurrentMap[K, V]) Remove(key K) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.m, key)
}

func (cm *ConcurrentMap[K, V]) TryAdd(key K, factory func() (V, error)) (wasAdded bool, errorInFactory error) {
	_, wasSet, errorInFactory := cm.GetOrAdd(key, factory)
	return wasSet, errorInFactory
}

func (cm *ConcurrentMap[K, V]) TryRemove(key K) (value V, wasRemoved bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	value, ok := cm.m[key]
	if ok {
		delete(cm.m, key)
	}
	return value, ok
}

// GetOrAdd returns the existing value for key if present,
// or uses the factory to create, store, and then return a new value.
// The factory is only called if the key is not present.
func (cm *ConcurrentMap[K, V]) GetOrAdd(key K, factory func() (V, error)) (value V, wasAdded bool, errorInFactory error) {
	cm.mu.RLock()
	v, ok := cm.m[key]
	cm.mu.RUnlock()
	if ok {
		return v, false, nil
	}

	// Upgrade to write lock.
	cm.mu.Lock()
	defer cm.mu.Unlock()
	// Double-check: maybe another goroutine set the value.
	if v, ok = cm.m[key]; ok {
		return v, false, nil
	}

	v, err := factory()
	cm.m[key] = v
	return v, err != nil, err
}

// AddOrUpdate either adds a new key/value if the key does not exist,
// or updates the value using the provided update function.
func (cm *ConcurrentMap[K, V]) AddOrUpdate(key K, addValue V, updateFunc func(existing V) V) (value V, wasUpdated bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if existing, ok := cm.m[key]; ok {
		updated := updateFunc(existing)
		cm.m[key] = updated
		return updated, true
	}
	cm.m[key] = addValue
	return addValue, false
}

func (cm *ConcurrentMap[K, V]) Count() int {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return len(cm.m)
}

func (cm *ConcurrentMap[K, V]) Keys() []K {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return maps.Keys(cm.m)
}

func (cm *ConcurrentMap[K, V]) Values() []V {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return maps.Values(cm.m)
}
