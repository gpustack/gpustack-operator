package bus

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

// Handler is a function that processes messages of type T.
type Handler[T any] func(context.Context, T) error

type bus struct {
	mu         sync.RWMutex
	typedBuses map[reflect.Type]any
}

func typeOf[T any]() reflect.Type {
	var zero T
	return reflect.TypeOf(zero)
}

type typedBus[T any] struct {
	handlers []namedHandler[T]
}

type namedHandler[T any] struct {
	name string
	call Handler[T]
}

func subscribe[T any](b *bus, name string, h Handler[T]) error {
	if h == nil {
		return errors.New("nil handler")
	}

	typ := typeOf[T]()

	b.mu.Lock()
	defer b.mu.Unlock()

	tb, ok := b.typedBuses[typ]
	if !ok {
		tb = &typedBus[T]{}
		b.typedBuses[typ] = tb
	}

	tbus := tb.(*typedBus[T])
	tbus.handlers = append(tbus.handlers, namedHandler[T]{name: name, call: h})

	return nil
}

func publish[T any](b *bus, ctx context.Context, msg T) error {
	typ := typeOf[T]()

	b.mu.RLock()
	tb, ok := b.typedBuses[typ]
	b.mu.RUnlock()

	if !ok {
		return nil
	}

	tbus := tb.(*typedBus[T])
	for i := range tbus.handlers {
		if err := tbus.handlers[i].call(ctx, msg); err != nil {
			return fmt.Errorf("call %q handler: %w", tbus.handlers[i].name, err)
		}
	}

	return nil
}

var globalBus = &bus{
	typedBuses: make(map[reflect.Type]any),
}

// Subscribe subscribes a handler to the bus for messages of type T.
// The name is used for logging and debugging purposes.
func Subscribe[T any](name string, h Handler[T]) error {
	return subscribe(globalBus, name, h)
}

// Publish publishes a message of type T to all handlers subscribed to that type.
// The first registered handler will receive the message first.
// If any handler returns an error, Publish will return that error and stop calling subsequent handlers.
func Publish[T any](ctx context.Context, msg T) error {
	return publish(globalBus, ctx, msg)
}
