package common

import (
	"sync"
)

// EventSubscriber represents the public interface for subscribing and unsubscribing.
type EventSubscriber[T any] interface {
	Subscribe(handler func(T)) (unsubscribe func())
}

// EventRaiser encapsulates the event logic with internal raise capability.
type EventRaiser[T any] interface {
	Raise(data T)
}

// Event to be used by the type that defines the event to avoid having to store two values
type Event[T any] interface {
	EventSubscriber[T]
	EventRaiser[T]
}

type event[T any] struct {
	sync.Mutex
	handlers map[int]func(T)
	nextID   int
}

// NewEvent creates a new encapsulated Event.
func NewEvent[T any]() Event[T] {
	e := &event[T]{
		handlers: make(map[int]func(T)),
	}
	return e
}

// Subscribe adds a handler to the event and returns an unsubscribe function.
func (e *event[T]) Subscribe(handler func(T)) (unsubscribe func()) {
	e.Lock()
	defer e.Unlock()

	id := e.nextID
	e.nextID++
	e.handlers[id] = handler

	return func() {
		e.Lock()
		defer e.Unlock()
		delete(e.handlers, id)
	}
}

func (e *event[T]) Raise(data T) {
	e.Lock()
	defer e.Unlock()

	for _, handler := range e.handlers {
		handler(data)
	}
}
