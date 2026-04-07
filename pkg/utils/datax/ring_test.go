package datax

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRingStack(t *testing.T) {
	s := NewRingStack[int](3)

	// Padding to power of 2 capacity
	assert.Equal(t, 4, s.Cap(), "capacity should be 4")

	_, ok := s.Pop()
	assert.False(t, ok, "pop should fail when stack is empty")

	// Push when empty
	s.Push(1)
	s.Push(2)
	s.Push(3)
	assert.Equal(t, []int{3, 2, 1}, s.Data(), "data should be [3, 2, 1]")

	// Pop when not full
	s.Pop()
	assert.Equal(t, []int{2, 1}, s.Data(), "data should be [2, 1]")

	// Push when not full
	s.Push(4)
	s.Push(5)
	assert.Equal(t, []int{5, 4, 2, 1}, s.Data(), "data should be [5, 4, 2, 1]")

	// Push when full
	s.Push(6)
	assert.Equal(t, []int{6, 5, 4, 2}, s.Data(), "data should be [6, 5, 4, 2]")

	// Pop when full
	s.Pop()
	s.Pop()
	assert.Equal(t, []int{4, 2}, s.Data(), "data should be [4, 2]")

	// Stats
	assert.Equal(t, 2, s.Len(), "length should be 2")

	// Push more elements to test overwriting
	s.Push(7)
	s.Push(8)
	s.Push(9)
	assert.Equal(t, []int{9, 8, 7, 4}, s.Data(), "data should be [9, 8, 7, 4]")

	// Peek the top element
	v, _ := s.Peek()
	assert.Equal(t, 9, v, "top element should be 9")

	// Push more elements to test overwriting again
	s.Push(10)
	s.Push(11)
	s.Push(12)
	assert.Equal(t, []int{12, 11, 10, 9}, s.Data(), "data should be [12, 11, 10, 9]")

	// Peek the top element again
	v, _ = s.Peek()
	assert.Equal(t, 12, v, "top element should be 12")
}

func TestRingQueue(t *testing.T) {
	q := NewRingQueue[int](3)

	// Padding to power of 2 capacity
	assert.Equal(t, 4, q.Cap(), "capacity should be 4")

	_, ok := q.Dequeue()
	assert.False(t, ok, "dequeue should fail when queue is empty")

	// Enqueue when empty
	q.Enqueue(1)
	q.Enqueue(2)
	q.Enqueue(3)
	assert.Equal(t, []int{1, 2, 3}, q.Data(), "data should be [1, 2, 3]")

	// Dequeue when not full
	q.Dequeue()
	assert.Equal(t, []int{2, 3}, q.Data(), "data should be [2, 3]")

	// Enqueue when not full
	q.Enqueue(4)
	q.Enqueue(5)
	assert.Equal(t, []int{2, 3, 4, 5}, q.Data(), "data should be [2, 3, 4, 5]")

	// Enqueue when full
	q.Enqueue(6)
	assert.Equal(t, []int{3, 4, 5, 6}, q.Data(), "data should be [3, 4, 5, 6]")

	// Dequeue when full
	q.Dequeue()
	q.Dequeue()
	assert.Equal(t, []int{5, 6}, q.Data(), "data should be [5, 6]")

	// Stats
	assert.Equal(t, 2, q.Len(), "length should be 2")
}

func TestRingBuffer(t *testing.T) {
	b := NewRingBuffer[int](3)

	// Padding to power of 2 capacity
	assert.Equal(t, 4, b.Cap(), "capacity should be 4")

	_, ok := b.First()
	assert.False(t, ok, "first should fail when buffer is empty")

	_, ok = b.Last()
	assert.False(t, ok, "last should fail when buffer is empty")

	// Push when empty
	b.Push(1)
	b.Push(2)
	b.Push(3)
	assert.Equal(t, []int{1, 2, 3}, b.Data(), "data should be [1, 2, 3]")

	// Push when not full
	b.Push(4)
	assert.Equal(t, []int{1, 2, 3, 4}, b.Data(), "data should be [1, 2, 3, 4]")

	// Push when full
	b.Push(5)
	assert.Equal(t, []int{2, 3, 4, 5}, b.Data(), "data should be [2, 3, 4, 5]")

	// First and Last
	v, _ := b.First()
	assert.Equal(t, 2, v, "first element should be 2")
	v, _ = b.Last()
	assert.Equal(t, 5, v, "last element should be 5")

	// Stats
	assert.Equal(t, 4, b.Len(), "length should be 4")
}
