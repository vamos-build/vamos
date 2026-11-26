package telemetry

import "container/list"

// lru is a map of at most size entries, which forgets the one least recently put or got to make room for another.
// It isn't safe for concurrent use; the bridge holds its lock around it.
type lru[K comparable, V any] struct {
	size    int
	order   *list.List // of *entry[K, V], the most recent first
	entries map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key   K
	value V
}

// newLRU returns an empty lru of at most size entries.
func newLRU[K comparable, V any](size int) *lru[K, V] {
	return &lru[K, V]{size: size, order: list.New(), entries: map[K]*list.Element{}}
}

// put sets key's value, forgetting the least recent entry if the lru is full.
func (l *lru[K, V]) put(key K, value V) {
	if e, ok := l.entries[key]; ok {
		e.Value.(*entry[K, V]).value = value
		l.order.MoveToFront(e)
		return
	}
	l.entries[key] = l.order.PushFront(&entry[K, V]{key: key, value: value})
	if l.order.Len() > l.size {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.entries, oldest.Value.(*entry[K, V]).key)
	}
}

// get returns key's value, if the lru remembers it.
func (l *lru[K, V]) get(key K) (V, bool) {
	e, ok := l.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	l.order.MoveToFront(e)
	return e.Value.(*entry[K, V]).value, true
}
