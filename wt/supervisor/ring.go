package main

// boundedRing keeps the newest items under both an item count and a byte limit.
type boundedRing[T any] struct {
	items    []T
	bytes    int
	maxItems int
	maxBytes int
	size     func(T) int
}

func newBoundedRing[T any](maxItems, maxBytes int, size func(T) int) *boundedRing[T] {
	return &boundedRing[T]{maxItems: maxItems, maxBytes: maxBytes, size: size}
}

// add appends one item and drops the oldest until both limits hold.
func (r *boundedRing[T]) add(item T) {
	r.items = append(r.items, item)
	r.bytes += r.size(item)
	for len(r.items) > r.maxItems || (r.bytes > r.maxBytes && len(r.items) > 1) {
		dropped := r.items[0]
		r.items = r.items[1:]
		r.bytes -= r.size(dropped)
	}
}

func (r *boundedRing[T]) len() int {
	return len(r.items)
}

func (r *boundedRing[T]) at(index int) T {
	return r.items[index]
}

func (r *boundedRing[T]) reset() {
	r.items = nil
	r.bytes = 0
}
