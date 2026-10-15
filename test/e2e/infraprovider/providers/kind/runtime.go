// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kind

import (
	"fmt"
	"os"
	"strings"
)

type containerRuntime string

func (ce containerRuntime) String() string {
	return string(ce)
}

const (
	docker containerRuntime = "docker"
	podman containerRuntime = "podman"
)

func getContainerRuntime() containerRuntime {
	cr := os.Getenv("CONTAINER_RUNTIME")
	if cr == "" {
		return docker
	}
	switch strings.ToLower(cr) {
	case docker.String():
		return docker
	case podman.String():
		return podman
	default:
		panic(fmt.Sprintf("unknown container engine %q. Supported engines are docker or podman.", cr))
	}
}
