package timeoutqueue

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"
)

type testItem struct{}

func sortTestItems(s []item[testItem]) []item[testItem] {
	c := make([]item[testItem], len(s))
	copy(c, s)
	sort.Slice(c, func(i, j int) bool { return c[i].deadline.Before(c[j].deadline) })
	return c
}

func TestConcurrentPushAndPopOrder(t *testing.T) {
	tq := New[testItem]()

	// will push some test items that deadline within now+200ms to now+800ms
	nitems := 100
	jitter := 500
	deadlinePush := time.Now().Add(time.Duration(200) * time.Millisecond)
	deadlinePop := deadlinePush.Add(time.Duration(jitter+100) * time.Millisecond)
	items := make([]item[testItem], 0, nitems)
	for i := 0; i < nitems; i++ {
		d := deadlinePush.Add(time.Duration(rand.Intn(jitter)) * time.Millisecond)
		items = append(items, item[testItem]{deadline: d})
	}

	expected := make([]item[testItem], 0, nitems)
	var it item[testItem]
	var zero item[testItem]

	// pop items until deadlinePop
	var poppedAt time.Time
	ctx, cancel := context.WithDeadline(context.Background(), deadlinePop)
	defer cancel()
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			it = tq.pop(ctx)
			poppedAt = time.Now()
			if it != zero && poppedAt.Before(it.deadline) {
				// we popped an item before it was due
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				expected = append(expected, it)
			}
		}
	}()

	// now push the items concurrently
	for _, item := range items {
		tq.push(item)
	}

	// if push was not done by deadlinePush we can't ensure the
	// consistency of the test as we might be pushing items at the same
	// time other items deadline and pop in a way that the order is not
	// guaranteed and can't be tested later on
	if time.Now().After(deadlinePush) {
		t.Fatalf("Ended push at %s but expected to be done by %s", time.Now(), deadlinePush)
	}

	// wait for pop to be done
	wg.Wait()

	// after the pop deadline, all items should have been popped and
	// returned item should be zero
	if it != zero {
		t.Fatalf("Expected last item to be zero but got %s popped at %s", it.deadline, poppedAt)
	}

	// the backing slice is expected to be empty as all items have been
	// popped
	if len(tq.items) != 0 {
		t.Fatalf("Expected backing slice to empty but has length %d", len(tq.items))
	}

	// we popped as many items as we pushed
	if len(items) != len(expected) {
		t.Fatalf("Expected %d items but got %d", len(items), len(expected))
	}

	// we popped items in the correct order
	for i := 1; i < len(expected); i++ {
		if expected[i].deadline.Before(expected[i-1].deadline) {
			t.Fatalf("Expected %s to be later than %s", expected[i].deadline, expected[i-1].deadline)
		}
	}
}

func TestConcurrentPushAndPopPastDeadlines(t *testing.T) {
	tq := New[testItem]()

	// will push some test items that deadline within now-500ms to now+500ms
	// while past deadlines are not easily possible with the current exported
	// API it could still happen with enough contention so worth checking
	// however if we push items that have already deadlined we cannot really
	// guarantee a strict pop order at the end of the test so don't check for
	// that

	nitems := 100
	jitter := 500
	items := make([]item[testItem], 0, nitems)
	deadlinePop := time.Now().Add(time.Duration(jitter+100) * time.Millisecond)
	for i := 0; i < nitems; i++ {
		d := time.Now().Add(time.Duration(rand.Intn(jitter*2)-jitter) * time.Millisecond)
		items = append(items, item[testItem]{deadline: d})
	}

	expected := make([]item[testItem], 0, nitems)
	var it item[testItem]
	var zero item[testItem]

	// pop items until deadlinePop
	var poppedAt time.Time
	ctx, cancel := context.WithDeadline(context.Background(), deadlinePop)
	defer cancel()
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			it = tq.pop(ctx)
			poppedAt = time.Now()
			if it != zero && poppedAt.Before(it.deadline) {
				// we popped an item before it was due
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				expected = append(expected, it)
			}
		}
	}()

	// now push the items concurrently
	for _, item := range items {
		tq.push(item)
	}

	// wait for pop to be done
	wg.Wait()

	// after the pop deadline, all items should have been popped and
	// returned item should be zero
	if it != zero {
		t.Fatalf("Expected last item to be zero but got %s", it.deadline)
	}

	// the backing slice is expected to be empty as all items have been
	// popped
	if len(tq.items) != 0 {
		t.Fatalf("Expected backing slice to empty but has length %d", len(tq.items))
	}

	// we popped as many items as we pushed
	if len(items) != len(expected) {
		t.Fatalf("Expected %d items but got %d", len(items), len(expected))
	}
}

func TestConcurrentPop(t *testing.T) {
	// make test items that do not deadline until far in the future
	nitems := 100
	jitter := 500
	items := make([]item[testItem], 0, nitems)
	now := time.Now()
	start := now
	for i := 0; i < nitems; i++ {
		d := start.Add(time.Duration(1000+rand.Intn(jitter)) * time.Hour)
		items = append(items, item[testItem]{deadline: d})
	}

	tq := New[testItem]()

	// spawn some consumers
	nconsumers := 5
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < nconsumers; i++ {
		go tq.pop(ctx)
	}

	// give some time for the consumers to be waiting for items
	time.Sleep(time.Duration(100) * time.Millisecond)

	// push the items
	for _, item := range items {
		tq.push(item)
	}

	// give some time for the consumers to be waiting on items with the
	// earliest deadlines
	time.Sleep(time.Duration(100) * time.Millisecond)

	tq.popM.Lock()
	defer tq.popM.Unlock()

	// all consumers should be waiting on item deadlines
	if len(tq.consumers) != nconsumers {
		t.Fatalf("Expected %d consumers but got %d", nconsumers, len(tq.consumers))
	}

	// all consumers should have popped one item from the backing slice
	if len(tq.items) != nitems-nconsumers {
		t.Fatalf("Expected %d items in the backing slice but got %d", nitems-nconsumers, len(tq.items))
	}

	// check that the consumers are tracking the items with the earliest
	// deadlines irrespective of push order, so the backing slice should
	// not contain the items with the earliest deadlines which have been
	// popped by the consumers
	sorted := sortTestItems(items)
	boundary := sorted[nconsumers-1].deadline

	for _, item := range tq.items {
		if item.deadline.Before(boundary) {
			fmt.Printf("Inserted items: %d\n", len(items))
			for _, i := range items {
				fmt.Printf("%s\n", i.deadline)
			}

			fmt.Printf("Sorted items: %d\n", len(sorted))
			for _, i := range sorted {
				fmt.Printf("%s\n", i.deadline)
			}

			backing := sortTestItems(tq.items)
			fmt.Printf("Backing items: %d\n", len(backing))
			for _, i := range backing {
				fmt.Printf("%s\n", i.deadline)
			}

			t.Fatalf("Found non-popped item %s older than expected boundary %s", item.deadline, boundary)
		}
	}
}
