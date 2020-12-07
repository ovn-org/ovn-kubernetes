// +build windows

/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Based on https://github.com/kubernetes/kubernetes/blob/master/pkg/windows/service/service.go
The reason for making a copy is that packages in kubernetes/kubernetes
are not setup to be imported as modules without vendoring and pinning the sub
components using the "require" directive. If more packages need to be imported
then we can revisit importing this module too.
*/

package service

import (
	"context"
	"sync"

	"k8s.io/klog/v2"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

type handler struct {
	svcName string
	ctx     context.Context
	wg      sync.WaitGroup
}

func InitService(serviceName string, ctx context.Context) error {
	h := &handler{
		svcName: serviceName,
		ctx:     ctx,
	}

	var err error
	h.wg.Add(1)
	go func() {
		err = svc.Run(serviceName, h)
		if err != nil {
			// Failure before Execute() is called
			h.wg.Done()
		}
	}()
	h.wg.Wait()

	return err
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending, Accepts: 0}
	// Unblock initService()
	h.wg.Done()

	ctx, cancel := context.WithCancel(h.ctx)

	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.Accepted(windows.SERVICE_ACCEPT_PARAMCHANGE)}
	klog.Infof("Running %s as a Windows service", h.svcName)
Loop:
	for {
		select {
		case <-ctx.Done():
			s <- svc.Status{State: svc.StopPending}
			break Loop
		case c := <-r:
			switch c.Cmd {
			case svc.Cmd(windows.SERVICE_CONTROL_PARAMCHANGE):
				s <- c.CurrentStatus
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				klog.Infof("Service stopping")
				// Free up the control handler and let us terminate as gracefully as possible.
				// If that takes too long, the service controller will kill the remaining threads.
				// As per https://docs.microsoft.com/en-us/windows/desktop/services/service-control-handler-function
				s <- svc.Status{State: svc.StopPending}

				cancel()
				break Loop
			}
		}
	}

	return false, 0
}
