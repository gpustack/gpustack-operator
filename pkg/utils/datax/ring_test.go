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

func TestRingBufferDropWhile(t *testing.T) {
	tests := []struct {
		name     string
		size     uint64
		pushed   []int
		drop     func(int) bool
		wantDrop int
		wantData []int
	}{
		{
			name:     "an empty buffer drops nothing",
			size:     4,
			drop:     func(int) bool { return true },
			wantDrop: 0,
			wantData: []int{},
		},
		{
			name:     "a predicate that never holds drops nothing",
			size:     4,
			pushed:   []int{1, 2, 3},
			drop:     func(int) bool { return false },
			wantDrop: 0,
			wantData: []int{1, 2, 3},
		},
		{
			name:     "a partial drop removes the front",
			size:     4,
			pushed:   []int{1, 2, 3, 4},
			drop:     func(value int) bool { return value < 3 },
			wantDrop: 2,
			wantData: []int{3, 4},
		},
		{
			name:     "dropping every element empties the buffer",
			size:     4,
			pushed:   []int{1, 2, 3},
			drop:     func(int) bool { return true },
			wantDrop: 3,
			wantData: []int{},
		},
		{
			name:     "a drop after an overwrite removes the oldest",
			size:     3,
			pushed:   []int{1, 2, 3, 4, 5},
			drop:     func(value int) bool { return value < 4 },
			wantDrop: 2,
			wantData: []int{4, 5},
		},
		{
			name:     "dropping every element after an overwrite empties the buffer",
			size:     3,
			pushed:   []int{1, 2, 3, 4, 5},
			drop:     func(int) bool { return true },
			wantDrop: 4,
			wantData: []int{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer := NewRingBuffer[int](test.size)
			for _, value := range test.pushed {
				buffer.Push(value)
			}

			dropped := buffer.DropWhile(test.drop)

			assert.Equal(t, test.wantDrop, dropped, "dropped count")
			assert.Equal(t, test.wantData, buffer.Data(), "remaining data")
			assert.Equal(t, len(test.wantData), buffer.Len(), "remaining length")
		})
	}
}

func TestRingBufferReset(t *testing.T) {
	tests := []struct {
		name   string
		size   uint64
		pushed []int
	}{
		{
			name: "resetting an empty buffer keeps it empty",
			size: 4,
		},
		{
			name:   "resetting a filled buffer empties it",
			size:   4,
			pushed: []int{1, 2, 3},
		},
		{
			name:   "resetting a buffer that overwrote empties it",
			size:   3,
			pushed: []int{1, 2, 3, 4, 5},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			buffer := NewRingBuffer[int](test.size)
			for _, value := range test.pushed {
				buffer.Push(value)
			}

			buffer.Reset()

			assert.Equal(t, 0, buffer.Len(), "length after reset")
			assert.Equal(t, []int{}, buffer.Data(), "data after reset")
			_, ok := buffer.First()
			assert.False(t, ok, "first should fail after a reset")
			_, ok = buffer.Last()
			assert.False(t, ok, "last should fail after a reset")
		})
	}
}

func TestRingBufferResetKeepsTheBufferUsable(t *testing.T) {
	buffer := NewRingBuffer[int](3)
	for _, value := range []int{1, 2, 3, 4, 5} {
		buffer.Push(value)
	}

	buffer.Reset()

	for _, value := range []int{10, 11, 12, 13, 14} {
		buffer.Push(value)
	}
	assert.Equal(t, []int{11, 12, 13, 14}, buffer.Data(), "data after the buffer is filled again")
}

func TestRingBufferResetClearsTheValuesItHeld(t *testing.T) {
	type payload struct{ name string }

	buffer := NewRingBuffer[*payload](4)
	buffer.Push(&payload{name: "held"})

	buffer.Reset()

	assert.Equal(t, 0, buffer.Len(), "length after reset")
	_, ok := buffer.First()
	assert.False(t, ok, "first should fail after a reset")
	for i := range buffer.data {
		assert.Nil(t, buffer.data[i].val, "slot %d should be zeroed after reset", i)
	}
}

func TestRingBufferAt(t *testing.T) {
	buffer := NewRingBuffer[int](3)
	for _, value := range []int{1, 2, 3, 4, 5} {
		buffer.Push(value)
	}

	value, ok := buffer.At(0)
	assert.True(t, ok, "the oldest element should be readable")
	assert.Equal(t, 2, value, "the oldest element")
	value, ok = buffer.At(3)
	assert.True(t, ok, "the newest element should be readable")
	assert.Equal(t, 5, value, "the newest element")
	_, ok = buffer.At(4)
	assert.False(t, ok, "an index outside the buffer should fail")
	_, ok = buffer.At(-1)
	assert.False(t, ok, "a negative index should fail")
}

func TestRingBufferClone(t *testing.T) {
	var nilBuffer *RingBuffer[int]
	assert.Nil(t, nilBuffer.Clone())

	buffer := NewRingBuffer[int](4)
	for _, value := range []int{1, 2, 3} {
		buffer.Push(value)
	}

	cloned := buffer.Clone()
	if !assert.NotNil(t, cloned) {
		return
	}
	assert.Equal(t, buffer.Len(), cloned.Len())
	assert.Equal(t, buffer.Data(), cloned.Data())

	// Modifying cloned should not affect original.
	cloned.Push(4)
	assert.Equal(t, 3, buffer.Len())
	assert.Equal(t, 4, cloned.Len())
}
