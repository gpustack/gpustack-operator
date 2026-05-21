package topic

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"

	"github.com/google/uuid"
)

// Topic represents a topic to which subscribers can subscribe and publishers can publish events.
type Topic string

// Event represents an event that is published to a topic.
// It contains the topic and the data associated with the event.
type Event[T any] struct {
	Topic Topic
	Data  T
}

// Subscriber represents a subscriber that can receive events from a topic.
type Subscriber[T any] interface {
	// Receive blocks until an event is received or the context is canceled.
	Receive(context.Context) (Event[T], error)
	// Unsubscribe unsubscribes the subscriber from the topic.
	// Must be called to clean up resources when the subscriber is no longer needed.
	Unsubscribe()
}

type hub[T any] struct {
	t Topic

	mu sync.RWMutex
	m  map[string]chan Event[T]
}

func newHub[T any](t Topic) *hub[T] {
	return &hub[T]{
		t: t,
		m: make(map[string]chan Event[T]),
	}
}

func (h *hub[T]) subscribe() Subscriber[T] {
	n := uuid.NewString()
	c := make(chan Event[T], runtime.GOMAXPROCS(0)*2)

	h.mu.Lock()
	h.m[n] = c
	h.mu.Unlock()

	return &subscriber[T]{h: h, n: n, c: c}
}

func (h *hub[T]) publish(ctx context.Context, data T) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for n, c := range h.m {
		select {
		case <-ctx.Done():
			return
		case c <- Event[T]{Topic: h.t, Data: data}:
		default:
			close(c)
			delete(h.m, n)
		}
	}
}

type subscriber[T any] struct {
	h *hub[T]
	n string
	c chan Event[T]
}

func (s *subscriber[T]) Receive(ctx context.Context) (Event[T], error) {
	select {
	case <-ctx.Done():
		return Event[T]{}, ctx.Err()
	case e, ok := <-s.c:
		if !ok {
			return Event[T]{}, errors.New("topic is closed")
		}
		return e, nil
	}
}

func (s *subscriber[T]) Unsubscribe() {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()

	if c, ok := s.h.m[s.n]; ok {
		close(c)
		delete(s.h.m, s.n)
	}
}

type bus struct {
	mu          sync.RWMutex
	typedTopics map[reflect.Type]map[Topic]any
}

func typeOf[T any]() reflect.Type {
	var zero T
	return reflect.TypeOf(zero)
}

func subscribe[T any](b *bus, t Topic) (Subscriber[T], error) {
	typ := typeOf[T]()

	b.mu.Lock()
	defer b.mu.Unlock()

	tm, ok := b.typedTopics[typ]
	if !ok {
		tm = make(map[Topic]any)
		b.typedTopics[typ] = tm
	}

	h, ok := tm[t]
	if !ok {
		nh := newHub[T](t)
		tm[t] = nh
		return nh.subscribe(), nil
	}

	return h.(*hub[T]).subscribe(), nil
}

func publish[T any](b *bus, ctx context.Context, t Topic, data T) error {
	typ := typeOf[T]()

	b.mu.RLock()
	tm, ok := b.typedTopics[typ]
	b.mu.RUnlock()

	if !ok {
		return nil
	}

	h, ok := tm[t]
	if !ok {
		return nil
	}

	h.(*hub[T]).publish(ctx, data)
	return nil
}

// unsubscribe removes all subscribers for a typed topic.
func unsubscribe[T any](b *bus, t Topic) error {
	typ := typeOf[T]()

	b.mu.Lock()
	defer b.mu.Unlock()

	tm, ok := b.typedTopics[typ]
	if !ok {
		return nil
	}

	v, ok := tm[t]
	if !ok {
		return nil
	}

	h := v.(*hub[T])

	// hub lock is nested under bus lock consistently with subscribe/publish paths.
	h.mu.Lock()
	for n, c := range h.m {
		close(c)
		delete(h.m, n)
	}
	h.mu.Unlock()

	delete(tm, t)
	if len(tm) == 0 {
		delete(b.typedTopics, typ)
	}

	return nil
}

var globalBus = &bus{
	typedTopics: make(map[reflect.Type]map[Topic]any),
}

// Subscribe subscribes to a topic using the global bus and returns a Subscriber that can receive events from the topic.
func Subscribe[T any](t Topic) (Subscriber[T], error) {
	return subscribe[T](globalBus, t)
}

// Publish publishes an event to a topic using the global bus.
// It returns an error if the publish operation fails.
func Publish[T any](ctx context.Context, t Topic, data T) error {
	return publish[T](globalBus, ctx, t, data)
}

// UnsubscribeAll unsubscribes all subscribers from a topic on the global bus.
func UnsubscribeAll[T any](t Topic) error {
	return unsubscribe[T](globalBus, t)
}
