package builder

import "sync"

// queue is a FIFO of domains with a fixed capacity (CACHE_SIZE + 1).
type queue struct {
	mu    sync.Mutex
	cap   int
	items []string
}

func newQueue(cap int) *queue { return &queue{cap: cap} }

// Push appends domain; false means the queue is full.
func (q *queue) Push(d string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= q.cap {
		return false
	}
	q.items = append(q.items, d)
	return true
}

// Pop returns the oldest item; ok is false when the queue is empty.
func (q *queue) Pop() (d string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return "", false
	}
	d = q.items[0]
	q.items = q.items[1:]
	return d, true
}

// Remove drops domain from the queue; false when it was not there.
func (q *queue) Remove(d string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, it := range q.items {
		if it == d {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return true
		}
	}
	return false
}

// Len returns the number of queued items.
func (q *queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Items returns a copy of the queue contents in order.
func (q *queue) Items() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.items...)
}
