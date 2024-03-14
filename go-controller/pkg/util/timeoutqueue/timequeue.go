package timeoutqueue

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// TimeoutQueue is a thread safe queue of items that can only be popped after a
// timeout
type TimeoutQueue[T comparable] struct {
	popM      sync.Mutex
	pushM     sync.Mutex
	items     heapImpl[T]
	consumers map[chan struct{}]time.Time
}

// New TimeoutQueue
func New[T comparable]() *TimeoutQueue[T] {
	tq := TimeoutQueue[T]{
		popM:      sync.Mutex{},
		pushM:     sync.Mutex{},
		consumers: make(map[chan struct{}]time.Time),
	}
	return &tq
}

// Pop an item from the queue no earlier than its associated timeout. It blocks
// at least until that time is reached, the context expires or if there are no
// items in the queue. Timing precission is equivalent to that of time.Timer.
func (tq *TimeoutQueue[T]) Pop(ctx context.Context) T {
	return tq.pop(ctx).item
}

// Push an item into the queue that can't be popped before its associated
// timeout
func (tq *TimeoutQueue[T]) Push(it T, timeout time.Duration) {
	tq.push(item[T]{item: it, deadline: time.Now().Add(timeout)})
}

// item is the internal representation of an item with its asssociated deadline
type item[T comparable] struct {
	item     T
	deadline time.Time
}

func (tq *TimeoutQueue[T]) pop(ctx context.Context) item[T] {
	// We make extensive use of channels and timers. Suposedly they are cheap,
	// but it might make sense to cache and reuse them.
	var zero item[T]
	var timer *time.Timer
	var timeoutC <-chan time.Time
	signal := make(chan struct{})

	pop := func() (item[T], bool) {
		tq.popM.Lock()
		defer tq.popM.Unlock()

		// if there are items pending to pop, pop the next one and track its
		// deadline
		var it item[T]
		var deadline time.Time
		if tq.items.Len() > 0 {
			it = heap.Pop(&tq.items).(item[T])
			deadline = it.deadline
			d := time.Until(deadline)
			if d <= 0 {
				return it, true
			}
			if timer == nil {
				timer = time.NewTimer(d)
			} else {
				timer.Reset(d)
			}
			timeoutC = timer.C
		}

		// prepare to be signaled by producers when a new item arrives, either
		// when we are pending for an item to consume or when a new item has a
		// earlier deadline than the one we are tracking
		tq.consumers[signal] = deadline
		return it, false
	}

	unpop := func(item item[T]) {
		tq.popM.Lock()
		delete(tq.consumers, signal)
		tq.popM.Unlock()
		if item != zero {
			// use tq.push rather than heap.Push so that we make sure this item
			// has a chance to be picked up by another consumer if it needs be
			tq.push(item)
		}
		if timeoutC != nil && !timer.Stop() {
			// consume the pending timeout
			<-timeoutC
		}
		timeoutC = nil
	}

	for {
		item, due := pop()
		if due {
			unpop(zero)
			return item
		}
		select {
		case <-timeoutC:
			// timeout already consumed, flag it
			timeoutC = nil
			unpop(zero)
			return item
		case <-ctx.Done():
			unpop(item)
			return zero
		case <-signal:
			unpop(item)
		}
	}
}

func (tq *TimeoutQueue[T]) push(item item[T]) {
	// no concurrent push
	tq.pushM.Lock()
	defer tq.pushM.Unlock()

	// pop lock while we insert the item and evaluate consumers
	tq.popM.Lock()

	heap.Push(&tq.items, item)

	deadline := item.deadline
	for {
		// find a free consumer or the one with the latest deadline later than
		// this item's deadline and signal it so that this item can be picked up
		var signal chan<- struct{}
		var latest time.Time
		for s, d := range tq.consumers {
			if d.IsZero() {
				// free consumer
				signal = s
				break
			}
			if d.After(deadline) && d.After(latest) {
				// latest consumer deadline later than this item's deadline
				signal = s
				latest = d
			}
		}

		// Unlock so that consumers can unregister themselves if they timed out
		// while we were checking on them, otherwise we might be in a loop
		// signaling an old consumer on a channel it will never receive on.
		// Any consumer might become free and pick up this item from this point
		// onwards, so the signal might end up being not needed and spurious,
		// but this should happen rarely and should have no functional
		// consequences.
		tq.popM.Unlock()

		// either there were no consumers or none of them were tracking an item
		// with an later deadline than the one pushed here
		if signal == nil {
			return
		}

		// signal the consumer if it is still receiving
		select {
		case signal <- struct{}{}:
			return
		default:
		}

		// rare but we might not be able to signal the consumer we intended if
		// it timed out at the same time, in which case that consumer might be
		// gone and not come back, so evaluate consumers again
		tq.popM.Lock()
	}
}

// heapImpl is a slice implementing the heap interface as a time based priority
// queue of items
type heapImpl[T comparable] []item[T]

func (h heapImpl[T]) Len() int {
	return len(h)
}

func (h heapImpl[T]) Less(i, j int) bool {
	// item with the earliest deadline has higher priority
	return h[i].deadline.Before(h[j].deadline)
}

func (h heapImpl[T]) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *heapImpl[T]) Push(x any) {
	*h = append(*h, x.(item[T]))
}

func (h *heapImpl[T]) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	var zero item[T]
	old[n-1] = zero // avoid memory leak
	*h = old[0 : n-1]
	return it
}
