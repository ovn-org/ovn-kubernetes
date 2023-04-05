//go:build !linux
// +build !linux

package ovspinning

import (
	"k8s.io/klog/v2"
)

func Run(_ <-chan struct{}) {
	klog.Infof("OVS CPU pinning is supported on linux platform only")
}
