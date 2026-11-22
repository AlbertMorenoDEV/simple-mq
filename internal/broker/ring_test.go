package broker

import "testing"

func TestRingBufferFIFOAndShrink(t *testing.T) {
	rb := NewRingBuffer[int]()
	const n = 500
	for i := 0; i < n; i++ {
		rb.Push(i)
	}
	if rb.Len() != n {
		t.Fatalf("expected len %d, got %d", n, rb.Len())
	}
	peakCap := rb.Cap()
	if peakCap < n {
		t.Fatalf("expected cap >= %d, got %d", n, peakCap)
	}

	for i := 0; i < n-5; i++ {
		v, ok := rb.Pop()
		if !ok || v != i {
			t.Fatalf("pop %d: got (%d, %v)", i, v, ok)
		}
	}
	if rb.Cap() >= peakCap {
		t.Fatalf("expected backing array to shrink from %d, still %d", peakCap, rb.Cap())
	}

	for i := n - 5; i < n; i++ {
		v, ok := rb.Pop()
		if !ok || v != i {
			t.Fatalf("pop %d: got (%d, %v)", i, v, ok)
		}
	}
	if rb.Len() != 0 || rb.Cap() != 0 {
		t.Fatalf("expected drained buffer to release backing array, len=%d cap=%d", rb.Len(), rb.Cap())
	}
}

func TestRingBufferPushFrontAndPeek(t *testing.T) {
	rb := NewRingBuffer[string]()
	rb.Push("b")
	rb.Push("c")
	rb.PushFront("a")

	peeked := rb.Peek(2)
	if len(peeked) != 2 || peeked[0] != "a" || peeked[1] != "b" {
		t.Fatalf("unexpected peek: %v", peeked)
	}

	expected := []string{"a", "b", "c"}
	for _, want := range expected {
		got, ok := rb.Pop()
		if !ok || got != want {
			t.Fatalf("want %q, got (%q, %v)", want, got, ok)
		}
	}
}
