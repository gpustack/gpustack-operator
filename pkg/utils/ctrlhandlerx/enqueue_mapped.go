package ctrlhandlerx

import (
	"context"
	"reflect"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/priorityqueue"
	"sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DedupEnqueueRequestsFromMapFunc copies from the controller-runtime EnqueueRequestsFromMapFunc,
// but supports to configure a TTL to deduplicate requests for the same object within a specified time window.
//
// Within the TTL window after the most recent fire, subsequent events for the
// same request are coalesced into a single deferred enqueue scheduled at the
// end of the window via q.AddAfter — so the latest event is always covered,
// never silently dropped.
func DedupEnqueueRequestsFromMapFunc(
	ttl time.Duration,
	fn ctrlhandler.MapFunc,
) ctrlhandler.EventHandler {
	return TypedDedupEnqueueRequestsFromMapFunc(ttl, fn)
}

// DedupEnqueueRequestsFromMapFuncWithWindow is the same as
// DedupEnqueueRequestsFromMapFunc, but allows handlers from different watches
// to share one dedup window and coalesce duplicate requests together.
func DedupEnqueueRequestsFromMapFuncWithWindow(
	ttl time.Duration,
	window *DedupWindow[ctrlreconcile.Request],
	fn ctrlhandler.MapFunc,
) ctrlhandler.EventHandler {
	return TypedDedupEnqueueRequestsFromMapFuncWithWindow(ttl, window, fn)
}

// TypedDedupEnqueueRequestsFromMapFunc copies from the controller-runtime TypedEnqueueRequestsFromMapFunc,
// but supports to configure a TTL to deduplicate requests for the same object within a specified time window.
//
// Within the TTL window after the most recent fire, subsequent events for the
// same request are coalesced into a single deferred enqueue scheduled at the
// end of the window via q.AddAfter — so the latest event is always covered,
// never silently dropped.
func TypedDedupEnqueueRequestsFromMapFunc[object any, request comparable](
	ttl time.Duration,
	fn ctrlhandler.TypedMapFunc[object, request],
) ctrlhandler.TypedEventHandler[object, request] {
	return TypedDedupEnqueueRequestsFromMapFuncWithWindow(ttl, nil, fn)
}

// TypedDedupEnqueueRequestsFromMapFuncWithWindow is the same as
// TypedDedupEnqueueRequestsFromMapFunc, but allows sharing a dedup window
// between handlers, which enables cross-watch deduplication.
func TypedDedupEnqueueRequestsFromMapFuncWithWindow[object any, request comparable](
	ttl time.Duration,
	window *DedupWindow[request],
	fn ctrlhandler.TypedMapFunc[object, request],
) ctrlhandler.TypedEventHandler[object, request] {
	if window == nil {
		window = NewDedupWindow[request]()
	}
	return &dedupEnqueueRequestsFromMapFunc[object, request]{
		ttl:                          ttl,
		window:                       window,
		toRequests:                   fn,
		objectImplementsClientObject: implementsClientObject[object](),
	}
}

// DedupWindow tracks the last enqueue/scheduled time per request.
// Reuse one DedupWindow across handlers to dedup duplicate requests globally.
type DedupWindow[request comparable] struct {
	m     sync.RWMutex
	until map[request]time.Time
}

// NewDedupWindow creates a new DedupWindow,
// which tracks the last enqueue/scheduled time per request for deduplication.
func NewDedupWindow[request comparable]() *DedupWindow[request] {
	return &DedupWindow[request]{
		until: make(map[request]time.Time),
	}
}

var _ ctrlhandler.EventHandler = &dedupEnqueueRequestsFromMapFunc[ctrlcli.Object, ctrlreconcile.Request]{}

type dedupEnqueueRequestsFromMapFunc[object any, request comparable] struct {
	ttl                          time.Duration
	window                       *DedupWindow[request]
	toRequests                   ctrlhandler.TypedMapFunc[object, request]
	objectImplementsClientObject bool
}

// Create implements EventHandler.
func (e *dedupEnqueueRequestsFromMapFunc[object, request]) Create(
	ctx context.Context,
	evt event.TypedCreateEvent[object],
	q workqueue.TypedRateLimitingInterface[request],
) {
	reqs := map[request]empty{}

	var lowPriority bool
	if isPriorityQueue(q) && !isNil(evt.Object) {
		if evt.IsInInitialList {
			lowPriority = true
		}
	}
	e.mapAndEnqueue(ctx, q, evt.Object, reqs, lowPriority)
}

// Update implements EventHandler.
func (e *dedupEnqueueRequestsFromMapFunc[object, request]) Update(
	ctx context.Context,
	evt event.TypedUpdateEvent[object],
	q workqueue.TypedRateLimitingInterface[request],
) {
	var lowPriority bool
	if e.objectImplementsClientObject && isPriorityQueue(q) && !isNil(evt.ObjectOld) && !isNil(evt.ObjectNew) {
		lowPriority = any(evt.ObjectOld).(ctrlcli.Object).GetResourceVersion() == any(evt.ObjectNew).(ctrlcli.Object).GetResourceVersion()
	}
	reqs := map[request]empty{}
	e.mapAndEnqueue(ctx, q, evt.ObjectOld, reqs, lowPriority)
	e.mapAndEnqueue(ctx, q, evt.ObjectNew, reqs, lowPriority)
}

// Delete implements EventHandler.
func (e *dedupEnqueueRequestsFromMapFunc[object, request]) Delete(
	ctx context.Context,
	evt event.TypedDeleteEvent[object],
	q workqueue.TypedRateLimitingInterface[request],
) {
	reqs := map[request]empty{}
	e.mapAndEnqueue(ctx, q, evt.Object, reqs, false)
}

// Generic implements EventHandler.
func (e *dedupEnqueueRequestsFromMapFunc[object, request]) Generic(
	ctx context.Context,
	evt event.TypedGenericEvent[object],
	q workqueue.TypedRateLimitingInterface[request],
) {
	reqs := map[request]empty{}
	e.mapAndEnqueue(ctx, q, evt.Object, reqs, false)
}

func (e *dedupEnqueueRequestsFromMapFunc[object, request]) mapAndEnqueue(
	ctx context.Context,
	q workqueue.TypedRateLimitingInterface[request],
	o object,
	reqs map[request]empty,
	lowPriority bool,
) {
	requests := e.toRequests(ctx, o)
	if len(requests) == 0 {
		return
	}

	now := time.Now()

	type pending struct {
		req   request
		delay time.Duration
	}
	pendings := make([]pending, 0, len(requests))

	e.window.m.Lock()
	for _, req := range requests {
		if _, dup := reqs[req]; dup {
			continue
		}
		reqs[req] = empty{}

		// e.until[req] is the timestamp of the last (or scheduled future)
		// enqueue for req. The active TTL window is [t, t+ttl).
		t, exists := e.window.until[req]
		switch {
		case !exists || !now.Before(t.Add(e.ttl)):
			// No prior fire or the TTL window has expired — enqueue
			// immediately and open a new window starting at now.
			e.window.until[req] = now
			pendings = append(pendings, pending{req: req})
		case now.Before(t):
			// A deferred enqueue is already scheduled at t in the future —
			// the latest event is guaranteed to be covered by it, so this
			// event can be safely coalesced (dropped).
		default:
			// Inside the TTL window (t <= now < t+ttl) — defer the enqueue
			// to the end of the window so that the latest event is still
			// reconciled. Advance e.until[req] to the scheduled fire time
			// so further events in the same window fall into the
			// "now.Before(t)" branch above and are coalesced.
			delay := t.Add(e.ttl).Sub(now)
			e.window.until[req] = now.Add(delay)
			pendings = append(pendings, pending{req: req, delay: delay})
		}
	}

	// Opportunistically drop entries whose TTL window ended at least one
	// TTL ago — the next event for them would fire immediately anyway, so
	// keeping the entry serves no purpose. Skips future-scheduled entries
	// (now.Sub(t) < 0).
	for req, t := range e.window.until {
		if now.Sub(t) >= 2*e.ttl {
			delete(e.window.until, req)
		}
	}
	e.window.m.Unlock()

	for _, p := range pendings {
		e.enqueue(q, p.req, p.delay, lowPriority)
	}
}

// enqueue adds req to q. When delay > 0 the add is deferred via AddAfter (or
// the priorityqueue's After option). The underlying queue further coalesces
// duplicate items by min-duration / max-priority, providing a second line of
// defense against amplification.
func (e *dedupEnqueueRequestsFromMapFunc[object, request]) enqueue(
	q workqueue.TypedRateLimitingInterface[request],
	req request,
	delay time.Duration,
	lowPriority bool,
) {
	if pq, ok := q.(priorityqueue.PriorityQueue[request]); ok {
		opts := priorityqueue.AddOpts{After: delay}
		if lowPriority {
			opts.Priority = ptr.To(-100)
		}
		pq.AddWithOpts(opts, req)
		return
	}
	if delay > 0 {
		q.AddAfter(req, delay)
		return
	}
	q.Add(req)
}

var typeForClientObject = reflect.TypeFor[ctrlcli.Object]()

func implementsClientObject[object any]() bool {
	return reflect.TypeFor[object]().Implements(typeForClientObject)
}

func isPriorityQueue[request comparable](q workqueue.TypedRateLimitingInterface[request]) bool {
	_, ok := q.(priorityqueue.PriorityQueue[request])
	return ok
}

type empty struct{}

func isNil(arg any) bool {
	if v := reflect.ValueOf(arg); !v.IsValid() || ((v.Kind() == reflect.Ptr ||
		v.Kind() == reflect.Interface ||
		v.Kind() == reflect.Slice ||
		v.Kind() == reflect.Map ||
		v.Kind() == reflect.Chan ||
		v.Kind() == reflect.Func) && v.IsNil()) {
		return true
	}
	return false
}
