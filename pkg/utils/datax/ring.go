package datax

import (
	"math/bits"
	"runtime"
	"sync/atomic"
)

// _RingNode is a node in the ring, containing a sequence number and a value.
type _RingNode[T any] struct {
	seq atomic.Uint64
	val T
}

// RingStack is a lock-free ring stack that supports concurrent push and pop operations.
type RingStack[T any] struct {
	mask uint64
	data []_RingNode[T]
	_    [32]byte
	head atomic.Uint64
	_    [56]byte
	tail atomic.Uint64
	_    [56]byte
}

// NewRingStack creates a new RingStack with the given size.
// The size must be greater than 0.
func NewRingStack[T any](size uint64) *RingStack[T] {
	// Pad size to power of 2 for better performance.
	if size == 0 {
		panic("size must be greater than 0")
	}
	if size&(size-1) != 0 {
		size = 1 << (bits.Len64(size - 1))
	}

	r := &RingStack[T]{
		mask: size - 1,
		data: make([]_RingNode[T], size),
	}
	for i := uint64(0); i < size; i++ {
		r.data[i].seq.Store(i)
	}
	return r
}

// Push adds an element to the top of the stack.
func (r *RingStack[T]) Push(v T) {
	for {
		tail := r.tail.Load()
		head := r.head.Load()
		n := &r.data[tail&r.mask]

		seq := n.seq.Load()
		diff := int64(seq - tail)
		// diff < 0 means the slot is full, need to overwrite the oldest data by moving head forward.
		// diff = 0 means the slot is empty and can be used for enqueueing.
		// diff > 0 means the slot is being enqueued by another goroutine, try again.
		if diff <= 0 {
			if r.tail.CompareAndSwap(tail, tail+1) {
				n.val = v
				n.seq.Store(tail + 1)
				// If the stack is full, move head forward to overwrite the oldest data.
				if tail-head >= r.mask+1 {
					r.head.CompareAndSwap(head, head+1)
				}
				return
			}
		}
		runtime.Gosched()
	}
}

// Pop removes and returns the top element of the stack.
func (r *RingStack[T]) Pop() (v T, ok bool) {
	for {
		tail := r.tail.Load()
		if tail == r.head.Load() {
			return v, false // Empty stack
		}

		n := &r.data[(tail-1)&r.mask]
		seq := n.seq.Load()
		if seq == tail {
			if r.tail.CompareAndSwap(tail, tail-1) {
				v = n.val
				var zero T
				n.val = zero
				n.seq.Store(tail - r.mask - 1)
				return v, true
			}
		}
		runtime.Gosched()
	}
}

// Peek returns the top element of the stack without removing it.
func (r *RingStack[T]) Peek() (v T, ok bool) {
	for {
		tail := r.tail.Load()
		if tail == r.head.Load() {
			return v, false // Empty stack
		}

		n := &r.data[(tail-1)&r.mask]
		seq := n.seq.Load()
		if seq == tail {
			return n.val, true
		}
		runtime.Gosched()
	}
}

// Data returns a copied slice of the elements in the stack.
//
// Note that the returned slice may not be consistent with the actual data in the stack,
// as the stack may be modified by other goroutines concurrently.
func (r *RingStack[T]) Data() []T {
	head := r.head.Load()
	tail := r.tail.Load()

	result := make([]T, 0, tail-head)
	for i := tail; i > head; i-- {
		n := &r.data[(i-1)&r.mask]
		seq := n.seq.Load()
		if seq == i {
			result = append(result, n.val)
		}
	}
	return result
}

func (r *RingStack[T]) Len() int {
	head := r.head.Load()
	tail := r.tail.Load()
	return int(tail - head)
}

func (r *RingStack[T]) Cap() int {
	return int(r.mask + 1)
}

// RingQueue is a lock-free ring queue that supports concurrent enqueue and dequeue operations.
type RingQueue[T any] struct {
	mask uint64
	data []_RingNode[T]
	_    [32]byte
	head atomic.Uint64
	_    [56]byte
	tail atomic.Uint64
	_    [56]byte
}

// NewRingQueue creates a new RingQueue with the given size.
// The size must be greater than 0.
func NewRingQueue[T any](size uint64) *RingQueue[T] {
	// Pad size to power of 2 for better performance.
	if size <= 0 {
		panic("size must be greater than 0")
	}
	if size&(size-1) != 0 {
		size = 1 << (bits.Len64(size - 1))
	}

	r := &RingQueue[T]{
		mask: size - 1,
		data: make([]_RingNode[T], size),
	}
	for i := uint64(0); i < size; i++ {
		r.data[i].seq.Store(i)
	}
	return r
}

// Enqueue adds an element to the end of the queue.
func (r *RingQueue[T]) Enqueue(v T) {
	for {
		tail := r.tail.Load()
		head := r.head.Load()
		n := &r.data[tail&r.mask]

		seq := n.seq.Load()
		diff := int64(seq - tail)
		// diff < 0 means the slot is full, need to overwrite the oldest data by moving head forward.
		// diff = 0 means the slot is empty and can be used for enqueueing.
		// diff > 0 means the slot is being enqueued by another goroutine, try again.
		if diff <= 0 {
			if r.tail.CompareAndSwap(tail, tail+1) {
				n.val = v
				n.seq.Store(tail + 1)
				// If the queue is full, move head forward to overwrite the oldest data.
				if tail-head >= r.mask+1 {
					r.head.CompareAndSwap(head, head+1)
				}
				return
			}
		}
		runtime.Gosched()
	}
}

// Dequeue removes and returns the front element of the queue.
func (r *RingQueue[T]) Dequeue() (v T, ok bool) {
	for {
		head := r.head.Load()
		n := &r.data[head&r.mask]

		seq := n.seq.Load()
		diff := int64(seq - head - 1)
		// diff < 0 means the slot is empty, return false.
		// diff = 0 means the slot is full and can be used for dequeueing.
		// diff > 0 means the slot is being dequeued by another goroutine, try again.
		switch {
		case diff < 0:
			return v, false // Empty queue
		case diff == 0:
			if r.head.CompareAndSwap(head, head+1) {
				v = n.val
				var zero T
				n.val = zero
				n.seq.Store(head + r.mask + 1)
				return v, true
			}
		}
		runtime.Gosched()
	}
}

// Data returns a copied slice of the elements in the queue.
//
// Note that the returned slice may not be consistent with the actual data in the queue,
// as the queue may be modified by other goroutines concurrently.
func (r *RingQueue[T]) Data() []T {
	head := r.head.Load()
	tail := r.tail.Load()

	result := make([]T, 0, tail-head)
	for i := head; i < tail; i++ {
		n := &r.data[i&r.mask]
		seq := n.seq.Load()
		if seq == i+1 {
			result = append(result, n.val)
		}
	}
	return result
}

// Len returns the number of elements in the queue.
func (r *RingQueue[T]) Len() int {
	head := r.head.Load()
	tail := r.tail.Load()
	return int(tail - head)
}

// Cap returns the capacity of the queue.
func (r *RingQueue[T]) Cap() int {
	return int(r.mask + 1)
}

type RingBuffer[T any] struct {
	mask uint64
	data []_RingNode[T]
	_    [32]byte
	head atomic.Uint64
	_    [56]byte
	tail atomic.Uint64
	_    [56]byte
}

func NewRingBuffer[T any](size uint64) *RingBuffer[T] {
	// Pad size to power of 2 for better performance.
	if size <= 0 {
		panic("size must be greater than 0")
	}
	if size&(size-1) != 0 {
		size = 1 << (bits.Len64(size - 1))
	}

	r := &RingBuffer[T]{
		mask: size - 1,
		data: make([]_RingNode[T], size),
	}
	for i := uint64(0); i < size; i++ {
		r.data[i].seq.Store(i)
	}

	return r
}

// Push adds an element to the top of the stack.
func (r *RingBuffer[T]) Push(v T) {
	for {
		tail := r.tail.Load()
		head := r.head.Load()
		n := &r.data[tail&r.mask]

		seq := n.seq.Load()
		diff := int64(seq - tail)
		// diff < 0 means the slot is full, need to overwrite the oldest data by moving head forward.
		// diff = 0 means the slot is empty and can be used for enqueueing.
		// diff > 0 means the slot is being enqueued by another goroutine, try again.
		if diff <= 0 {
			if r.tail.CompareAndSwap(tail, tail+1) {
				n.val = v
				n.seq.Store(tail + 1)
				// If the stack is full, move head forward to overwrite the oldest data.
				if tail-head >= r.mask+1 {
					r.head.CompareAndSwap(head, head+1)
				}
				return
			}
		}
		runtime.Gosched()
	}
}

// Clone returns a shallow copy of the buffer.
//
// CLONE MUST BE EXTERNALLY SYNCHRONIZED against concurrent Push operations, because
// the underlying slice copy is not atomic across all slots.
func (r *RingBuffer[T]) Clone() *RingBuffer[T] {
	if r == nil {
		return nil
	}
	size := r.mask + 1
	dest := &RingBuffer[T]{
		mask: r.mask,
		data: make([]_RingNode[T], size),
	}
	copy(dest.data, r.data)
	dest.head.Store(r.head.Load())
	dest.tail.Store(r.tail.Load())
	return dest
}

// Reset empties the buffer and clears the values it held.
//
// THE VALUES ARE CLEARED so that a reset buffer does not keep the objects it held alive,
// and the free markers of the slots are restored so that the buffer is usable afterwards.
// Reset must be externally synchronized against concurrent Push, DropWhile, At, First, Last, Data, and Len calls.
func (r *RingBuffer[T]) Reset() {
	tail := r.tail.Load()
	var zero T
	span := r.mask + 1
	for i := uint64(0); i < span; i++ {
		r.data[i].val = zero
		r.data[i].seq.Store(tail + (i+span-tail%span)%span)
	}
	r.head.Store(tail)
}

// DropWhile removes the elements at the front of the buffer while the predicate holds for
// them, and returns how many it removed.
//
// IT IS THE PRUNING A TIME WINDOW NEEDS, because the samples of a window are chronological
// and the ones that fell out of it are at the front: dropping them costs one pass over the
// window and no reallocation. DropWhile must be externally synchronized against concurrent
// Push, DropWhile, At, First, Last, Data, Len, and Reset calls.
func (r *RingBuffer[T]) DropWhile(predicate func(T) bool) int {
	dropped := 0
	for {
		head := r.head.Load()
		tail := r.tail.Load()
		if head == tail {
			return dropped
		}
		n := &r.data[head&r.mask]
		switch diff := int64(n.seq.Load() - head - 1); {
		case diff < 0:
			// The slot is empty, so the buffer is empty.
			return dropped
		case diff > 0:
			// The slot is being written by another goroutine, try again.
			runtime.Gosched()
			continue
		}
		if !predicate(n.val) {
			return dropped
		}
		if !r.head.CompareAndSwap(head, head+1) {
			continue
		}
		var zero T
		n.val = zero
		n.seq.Store(head + r.mask + 1)
		dropped++
	}
}

// At returns the element at the given index from the front of the buffer.
//
// IT IS COUNTED FROM THE OLDEST ELEMENT, and an index outside the buffer returns false.
// At must be externally synchronized against concurrent Push, DropWhile, At, First, Last, Data, Len, and Reset calls.
func (r *RingBuffer[T]) At(index int) (v T, ok bool) {
	if index < 0 {
		return v, false
	}
	head := r.head.Load()
	position := head + uint64(index)
	if position >= r.tail.Load() {
		return v, false
	}
	n := &r.data[position&r.mask]
	if n.seq.Load() != position+1 {
		return v, false
	}
	return n.val, true
}

// First returns the first element of the buffer.
func (r *RingBuffer[T]) First() (v T, ok bool) {
	for {
		head := r.head.Load()
		n := &r.data[head&r.mask]

		seq := n.seq.Load()
		diff := int64(seq - head - 1)
		// diff < 0 means the slot is empty, return false.
		// diff = 0 means the slot is full and can be used for retrieving.
		// diff > 0 means the slot is being dequeued by another goroutine, try again.
		switch {
		case diff < 0:
			return v, false // Empty buffer
		case diff == 0:
			return n.val, true
		}
		runtime.Gosched()
	}
}

// Last returns the last element of the buffer.
func (r *RingBuffer[T]) Last() (v T, ok bool) {
	for {
		tail := r.tail.Load()
		if tail == r.head.Load() {
			return v, false // Empty buffer
		}

		n := &r.data[(tail-1)&r.mask]
		seq := n.seq.Load()
		if seq == tail {
			return n.val, true
		}

		runtime.Gosched()
	}
}

// Data returns a copied slice of the elements in the buffer.
//
// Note that the returned slice may not be consistent with the actual data in the buffer,
// as the buffer may be modified by other goroutines concurrently.
func (r *RingBuffer[T]) Data() []T {
	head := r.head.Load()
	tail := r.tail.Load()

	result := make([]T, 0, tail-head)
	for i := head; i < tail; i++ {
		n := &r.data[i&r.mask]
		seq := n.seq.Load()
		if seq == i+1 {
			result = append(result, n.val)
		}
	}
	return result
}

// Len returns the number of elements in the buffer.
func (r *RingBuffer[T]) Len() int {
	head := r.head.Load()
	tail := r.tail.Load()
	return int(tail - head)
}

// Cap returns the capacity of the buffer.
func (r *RingBuffer[T]) Cap() int {
	return int(r.mask + 1)
}
