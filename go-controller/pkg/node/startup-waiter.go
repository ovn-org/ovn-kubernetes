package node

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

type startupWaiter struct {
	tasks []*waitTask
	wg    *sync.WaitGroup
}

type waitFunc func() (bool, error)
type postWaitFunc func() error

type waitTask struct {
	waitFn waitFunc
	postFn postWaitFunc
}

func newStartupWaiter() *startupWaiter {
	return &startupWaiter{
		tasks: make([]*waitTask, 0, 2),
		wg:    &sync.WaitGroup{},
	}
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
			err := wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
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
