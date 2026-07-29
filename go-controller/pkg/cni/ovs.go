// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/libovsdb/client"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

var runner kexec.Interface
var vsctlPath string

func SetExec(r kexec.Interface) error {
	runner = r
	var err error
	vsctlPath, err = r.LookPath("ovs-vsctl")
	return err
}

// ResetRunner used by unit-tests to reset runner to its initial (un-initialized) value
func ResetRunner() {
	runner = nil
}

func ovsExec(args ...string) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("OVS exec runner not initialized")
	}

	args = append([]string{"--timeout=30"}, args...)
	output, err := runner.Command(vsctlPath, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to run 'ovs-vsctl %s': %v\n  %q", strings.Join(args, " "), err, string(output))
	}

	return strings.TrimSuffix(string(output), "\n"), nil
}

// ovsGetMultiOutput allows running get command with multiple columns
// returns a slice of requested fields
// if row doesn't exist command output will be a slice with an empty string
// empty ovsdb value [] will be replaced with an empty string
func ovsGetMultiOutput(table, record string, columns []string) ([]string, error) {
	args := []string{"--if-exists", "get", table, record}
	args = append(args, columns...)
	output, err := ovsExec(args...)
	var result []string
	// columns are separated with \n
	// remove \" as with --data=bare formatting
	for _, column := range strings.Split(strings.ReplaceAll(output, "\"", ""), "\n") {
		if column == "[]" {
			result = append(result, "")
		} else {
			result = append(result, column)
		}
	}
	return result, err
}

func ovsCreate(table string, values ...string) (string, error) {
	args := append([]string{"create", table}, values...)
	return ovsExec(args...)
}

func ovsDestroy(table, record string) error {
	_, err := ovsExec("--if-exists", "destroy", table, record)
	return err
}

func ovsSet(table, record string, values ...string) error {
	args := append([]string{"set", table, record}, values...)
	_, err := ovsExec(args...)
	return err
}

func getDatapathType(bridge string) (string, error) {
	br_type, err := ovsGet("bridge", bridge, "datapath_type", "")
	if err != nil {
		return "", err
	}
	return br_type, nil
}

func ovsGet(table, record, column, key string) (string, error) {
	args := []string{"--if-exists", "get", table, record}
	if key != "" {
		args = append(args, fmt.Sprintf("%s:%s", column, key))
	} else {
		args = append(args, column)
	}
	output, err := ovsExec(args...)
	return strings.Trim(strings.TrimSpace(string(output)), "\""), err
}

// Returns the given column of records that match the condition
func ovsFind(table, column string, conditions ...string) ([]string, error) {
	args := append([]string{"--no-heading", "--format=csv", "--data=bare", "--columns=" + column, "find", table}, conditions...)
	output, err := ovsExec(args...)
	if err != nil {
		return nil, err
	}
	if output == "" {
		return nil, nil
	}
	return strings.Split(output, "\n"), nil
}

func ovsClear(table, record string, columns ...string) error {
	args := append([]string{"--if-exists", "clear", table, record}, columns...)
	_, err := ovsExec(args...)
	return err
}

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
	// Note that this function is called either the Full mode or the DPU mode
	columns := []string{"external-ids:iface-id", "external-ids:ovn-installed"}

	mac := ifInfo.MAC.String()
	ifAddrs := ifInfo.IPs
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
			if ovsClient != nil {
				iface, err := ovsops.GetOVSInterface(ovsClient, ifaceName)
				if err != nil {
					if !errors.Is(err, client.ErrNotFound) {
						return fmt.Errorf("failed to get OVS interface %s while waiting for binding: %w", ifaceName, err)
					}
				} else {
					existingIfaceID = iface.ExternalIDs["iface-id"]
					ovnInstalled = iface.ExternalIDs["ovn-installed"]
				}
			} else {
				output, err := ovsGetMultiOutput("Interface", ifaceName, columns)
				if err == nil && len(output) > 0 {
					existingIfaceID = output[0]
				}
				if err == nil && len(output) == 2 {
					ovnInstalled = output[1]
				}
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

			if err := checkCancelSandbox(mac, getter, namespace, name, ifInfo.NADKey, initialPodUID); err != nil {
				return fmt.Errorf("%v waiting for OVS port binding for %s %v", err, mac, ifAddrs)
			}

			// try again later
			waitTime := 200 * time.Millisecond
			time.Sleep(waitTime)
			metrics.MetricOvsInterfaceUpWait.Add(waitTime.Seconds())
		}
	}
}
