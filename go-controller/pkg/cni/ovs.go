// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// checkCancelSandbox checks that this sandbox is still valid for the current
// instance of the Pod known to the node. Sandbox requests and Pod instances
// have a 1:1 relationship determined by pod UID. If we detect that the pod
// has changed either UID or MAC terminate this sandbox request early instead
// of waiting for OVN to set up flows that will never exist.
func checkCancelSandbox(mac string, getter PodInfoGetter, namespace, name, nadKey, initialPodUID string) error {
	// Not all node CNI modes have a Pod information source.
	if getter == nil {
		return nil
	}
	pod, err := getter.getPod(namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("pod deleted")
		}
		klog.Warningf("[%s/%s] failed to get pod while waiting for OVS port binding: %v", namespace, name, err)
		return nil
	}

	if string(pod.UID) != initialPodUID {
		// Pod UID changed and this sandbox should be canceled
		// so the new pod sandbox can run
		return fmt.Errorf("canceled old pod sandbox")
	}

	ovnAnnot, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return fmt.Errorf("pod OVN annotations deleted or invalid")
	}

	// Pod OVN annotation changed and this sandbox should
	// be canceled so the new pod sandbox can run with the
	// updated MAC/IP
	if mac != ovnAnnot.MAC.String() {
		return fmt.Errorf("pod OVN annotations changed")
	}

	return nil
}

func waitForPodInterface(ctx context.Context, ovsClient client.Client, ifInfo *PodInterfaceInfo,
	ifaceName, ifaceID string, getter PodInfoGetter,
	namespace, name, initialPodUID string) error {
	const (
		initialPollInterval = 2 * time.Millisecond
		maxPollInterval     = 200 * time.Millisecond
	)
	var changes <-chan struct{}
	if watcher, ok := ovsClient.(libovsdb.OVSInterfaceWatcher); ok {
		var stopWatching func()
		changes, stopWatching = watcher.WatchOVSInterface(ifaceName)
		defer stopWatching()
	}
	// Note that this function is called either the Full mode or the DPU mode
	mac := ifInfo.MAC.String()
	ifAddrs := ifInfo.IPs
	pollInterval := initialPollInterval
	var nextPodCheck time.Time
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			errDetail := "timed out"
			if ctx.Err() == context.Canceled {
				errDetail = "canceled while"
			}
			return fmt.Errorf("%s waiting for OVS port binding (ovn-installed) for %s %v", errDetail, mac, ifAddrs)
		default:
			// check to see if the interface has its expected external id set, which indicates if it is active
			existingIfaceID := ""
			ovnInstalled := ""
			iface := &vswitchd.Interface{Name: ifaceName}
			err := ovsClient.Get(ctx, iface)
			if err != nil {
				if !errors.Is(err, client.ErrNotFound) {
					return fmt.Errorf("failed to get OVS interface %s while waiting for binding: %w", ifaceName, err)
				}
			} else {
				existingIfaceID = iface.ExternalIDs["iface-id"]
				ovnInstalled = iface.ExternalIDs["ovn-installed"]
			}
			// It may have been cleared by a subsequent CNI ADD and if so, there's no need to keep checking for flows
			if existingIfaceID != "" && existingIfaceID != ifaceID {
				return fmt.Errorf("OVS sandbox port %s is no longer active (probably due to a subsequent "+
					"CNI ADD)", ifaceName)
			}
			if ovnInstalled == "true" {
				klog.V(5).Infof("Interface %s has ovn-installed=true", ifaceName)
				return nil
			}
			klog.V(5).Infof("Still waiting for OVS port %s to have ovn-installed=true", ifaceName)

			// Fast cache polls must not increase Kubernetes API traffic from the shim.
			if !time.Now().Before(nextPodCheck) {
				if err := checkCancelSandbox(mac, getter, namespace, name, ifInfo.NADKey, initialPodUID); err != nil {
					return fmt.Errorf("%v waiting for OVS port binding for %s %v", err, mac, ifAddrs)
				}
				nextPodCheck = time.Now().Add(maxPollInterval)
			}

			// Avoid a full polling interval for fast bindings, but back off for
			// slow ones. Keep the existing cadence for sandbox cancellation checks.
			waitTime := min(pollInterval, time.Until(nextPodCheck))
			start := time.Now()
			timer.Reset(waitTime)
			select {
			case <-ctx.Done():
			case <-changes:
				// A notification only wakes the loop; the cache is authoritative.
			case <-timer.C:
				// Keep polling as a fallback for dropped cache notifications.
				pollInterval = min(2*pollInterval, maxPollInterval)
			}
			metrics.MetricOvsInterfaceUpWait.Add(time.Since(start).Seconds())
		}
	}
}
