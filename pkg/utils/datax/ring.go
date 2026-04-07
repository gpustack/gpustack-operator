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
