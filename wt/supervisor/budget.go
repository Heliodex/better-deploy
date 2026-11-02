package main

import "log"

// byteBudget is a named byte limit where zero means unlimited.
type byteBudget struct {
	name  string
	limit int64
	watch budgetWatch
}

func (b *byteBudget) enabled() bool {
	return b != nil && b.limit > 0
}

// exceeded reports whether used is above the limit.
func (b *byteBudget) exceeded(used int64) bool {
	return b.enabled() && used > b.limit
}

// observe records usage and logs a transition when it crosses the limit.
func (b *byteBudget) observe(used int64) bool {
	over := b.exceeded(used)
	b.watch.update(over, b.name, used, b.limit)

	return over
}

// countBudget is a maximum item count where zero means unlimited.
type countBudget int64

func (b countBudget) enabled() bool {
	return b > 0
}

// allows reports whether one more item fits under the limit.
func (b countBudget) allows(count int64) bool {
	return !b.enabled() || count < int64(b)
}

// budgetWatch logs one budget crossing per transition.
type budgetWatch struct {
	over bool
}

func (w *budgetWatch) update(over bool, name string, used, limit int64) {
	if over == w.over {
		return
	}

	w.over = over
	if over {
		log.Printf("warning: %s exceeded: %d of %d bytes used", name, used, limit)
		return
	}

	log.Printf("%s usage is back under the limit", name)
}

// overByteBudget reports whether used exceeds a positive limit. Zero means unlimited.
func overByteBudget(limit, used int64) bool {
	budget := byteBudget{limit: limit}
	return budget.exceeded(used)
}
