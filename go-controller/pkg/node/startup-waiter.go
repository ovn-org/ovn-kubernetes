package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

type startupWaiter struct {
	tasks   []*waitTask
	wg      *sync.WaitGroup
	timeout time.Duration
}

type waitFunc func() (bool, error)
type postWaitFunc func() error

type waitTask struct {
	waitFn waitFunc
	postFn postWaitFunc
}

func newStartupWaiterWithTimeout(timeout time.Duration) *startupWaiter {
	return &startupWaiter{
		tasks:   make([]*waitTask, 0, 2),
		wg:      &sync.WaitGroup{},
		timeout: timeout,
	}
}

func newStartupWaiter() *startupWaiter {
	return newStartupWaiterWithTimeout(300 * time.Second)
}

func (w *startupWaiter) AddWait(waitFn waitFunc, postFn postWaitFunc) {
	w.tasks = append(w.tasks, &waitTask{
		waitFn: waitFn,
		postFn: postFn,
	})
}

func (w *startupWaiter) Wait() error {
	errors := make(chan error, len(w.tasks))
	for _, t := range w.tasks {
		w.wg.Add(1)
		go func(task *waitTask) {
			defer w.wg.Done()
			err := wait.PollUntilContextTimeout(context.Background(), 500*time.Millisecond, w.timeout, true, func(ctx context.Context) (bool, error) {
				return task.waitFn()
			})
			if err == nil && task.postFn != nil {
				err = task.postFn()
			}
			if err != nil {
				errors <- err
			}
		}(t)
	}
	w.wg.Wait()
	close(errors)
	for err := range errors {
		return fmt.Errorf("error waiting for node readiness: %v", err)
	}
	return nil
}
