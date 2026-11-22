package broker

const minRingCap = 16

// RingBuffer is a dynamically-resized FIFO circular buffer that zeroes popped
// elements and shrinks its backing array when utilization drops, preventing
// slice backing-array memory leaks.
type RingBuffer[T any] struct {
	buf   []T
	head  int
	tail  int
	count int
}

// NewRingBuffer creates an empty RingBuffer.
func NewRingBuffer[T any]() *RingBuffer[T] {
	return &RingBuffer[T]{}
}

// Len returns the number of items currently stored.
func (r *RingBuffer[T]) Len() int {
	return r.count
}

// Cap returns the current backing array capacity.
func (r *RingBuffer[T]) Cap() int {
	return len(r.buf)
}

// Push appends v to the back of the queue.
func (r *RingBuffer[T]) Push(v T) {
	if r.count == len(r.buf) {
		r.grow()
	}
	r.buf[r.tail] = v
	r.tail = (r.tail + 1) % len(r.buf)
	r.count++
}

// PushFront inserts v at the front of the queue (used for redelivery/requeue).
func (r *RingBuffer[T]) PushFront(v T) {
	if r.count == len(r.buf) {
		r.grow()
	}
	r.head = (r.head - 1 + len(r.buf)) % len(r.buf)
	r.buf[r.head] = v
	r.count++
}

// Pop removes and returns the front item.
func (r *RingBuffer[T]) Pop() (T, bool) {
	var zero T
	if r.count == 0 {
		return zero, false
	}
	v := r.buf[r.head]
	r.buf[r.head] = zero // Release references for GC immediately.
	r.head = (r.head + 1) % len(r.buf)
	r.count--

	if r.count == 0 {
		// Release oversized backing array completely when drained.
		if len(r.buf) > minRingCap {
			r.buf = nil
		}
		r.head = 0
		r.tail = 0
	} else if len(r.buf) > minRingCap*4 && r.count <= len(r.buf)/4 {
		r.resize(len(r.buf) / 2)
	}
	return v, true
}

// Peek returns up to limit items from the front without removing them.
func (r *RingBuffer[T]) Peek(limit int) []T {
	if r.count == 0 || limit == 0 {
		return nil
	}
	n := r.count
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]T, n)
	for i := 0; i < n; i++ {
		out[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	return out
}

// ToSlice returns all items in FIFO order.
func (r *RingBuffer[T]) ToSlice() []T {
	return r.Peek(r.count)
}

// Clear removes all elements and frees the backing array.
func (r *RingBuffer[T]) Clear() {
	r.buf = nil
	r.head = 0
	r.tail = 0
	r.count = 0
}

func (r *RingBuffer[T]) grow() {
	newCap := len(r.buf) * 2
	if newCap < minRingCap {
		newCap = minRingCap
	}
	r.resize(newCap)
}

func (r *RingBuffer[T]) resize(newCap int) {
	next := make([]T, newCap)
	for i := 0; i < r.count; i++ {
		next[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	r.buf = next
	r.head = 0
	r.tail = r.count
}
