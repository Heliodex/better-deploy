package main

import "log"

// overByteBudget reports whether used exceeds a positive limit. Zero means unlimited.
func overByteBudget(limit, used int64) bool {
	return limit > 0 && used > limit
}

// budgetWatch logs one byte-budget crossing per transition.
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
