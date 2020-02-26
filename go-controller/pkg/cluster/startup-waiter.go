package cluster

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

type startupWaiter struct {
	nodeName string
	tasks    []*waitTask
	wg       *sync.WaitGroup
	errors   chan error
}

type waitFunc func(string) (bool, error)
type postWaitFunc func() error

type waitTask struct {
	waitFn waitFunc
	postFn postWaitFunc
}

func newStartupWaiter(nodeName string) *startupWaiter {
	return &startupWaiter{
		nodeName: nodeName,
		tasks:    make([]*waitTask, 0, 2),
		wg:       &sync.WaitGroup{},
		errors:   make(chan error),
	}
}

func (w *startupWaiter) AddWait(waitFn waitFunc, postFn postWaitFunc) {
	w.tasks = append(w.tasks, &waitTask{
		waitFn: waitFn,
		postFn: postFn,
	})
}

func (w *startupWaiter) Wait() error {
	for _, t := range w.tasks {
		w.wg.Add(1)
		go func(task *waitTask) {
			defer w.wg.Done()
			err := wait.PollImmediate(500*time.Millisecond, 300*time.Second, func() (bool, error) {
				return task.waitFn(w.nodeName)
			})
			if task.postFn != nil {
				err = task.postFn()
			}
			if err != nil {
				w.errors <- err
			}
		}(t)
	}
	w.wg.Wait()
	close(w.errors)
	for err := range w.errors {
		return fmt.Errorf("error waiting for node readiness: %v", err)
	}
	return nil
}
