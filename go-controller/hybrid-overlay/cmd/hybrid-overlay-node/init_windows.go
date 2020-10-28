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
*/

package main

import (
	"context"

	"github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/windows/service"
	"github.com/urfave/cli/v2"
	"k8s.io/klog/v2"
)

// initForOS performs Windows specific app initialization
func initForOS(c *cli.Context, ctx context.Context) error {
	if c.Bool(windowServiceArgName) {
		klog.Infof("Initializing Windows service")
		return service.InitService(appName, ctx)
	}
	signalHandler(ctx)
	return nil
}
