// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"sync"
	"time"
)

type FakeController struct {
	sync.Mutex
	Reconciles []string
}

func (f *FakeController) Reconcile(key string) {
	f.Lock()
	defer f.Unlock()
	f.Reconciles = append(f.Reconciles, fmt.Sprintf("Reconcile:%s", key))
}

func (f *FakeController) ReconcileRateLimited(key string) {
	f.Lock()
	defer f.Unlock()
	f.Reconciles = append(f.Reconciles, fmt.Sprintf("RateLimited:%s", key))
}

func (f *FakeController) ReconcileAfter(key string, _ time.Duration) {
	f.Lock()
	defer f.Unlock()
	f.Reconciles = append(f.Reconciles, fmt.Sprintf("After:%s", key))
}

func (f *FakeController) addHandler() error   { return nil }
func (f *FakeController) startWorkers() error { return nil }
func (f *FakeController) stop()               {}

func (f *FakeController) ReconcileAll() {
	f.Lock()
	defer f.Unlock()
	f.Reconciles = append(f.Reconciles, "ReconcileAll")
}
