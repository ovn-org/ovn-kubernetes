// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package ovspinning

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/klog/v2"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	"k8s.io/utils/cpuset"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops/ovs"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

const PmdPrefix string = "pmd"

// These variables are meant to be used in unit tests
var tickDuration time.Duration = 1 * time.Second
var getOvsVSwitchdPIDFn func() (string, error) = util.GetOvsVSwitchdPID
var getOvsDBServerPIDFn func() (string, error) = util.GetOvsDBServerPID
var featureEnablerFile string = "/etc/openvswitch/enable_dynamic_cpu_affinity"
var kubeletConfigFilePath = "/host/etc/kubernetes/kubelet.conf"

// Run monitors OVS daemon's processes (ovs-vswitchd and ovsdb-server) and sets their CPU affinity
// masks to that of the current process.
// This feature is enabled by the presence of a non-empty file in the path `/etc/openvswitch/enable_dynamic_cpu_affinity`
// we're passing the podResCli from the caller, so we could support unit-tests
func Run(ctx context.Context, stopCh <-chan struct{}, podResCli podresourcesapi.PodResourcesListerClient, ovsClient libovsdbclient.Client) {
	run(ctx, stopCh, podResCli, ovsClient, nil)
}

// run reports initial readiness and subsequent enablement changes synchronously
// from the pinning loop, allowing tests to wait for in-flight reconciliation to finish.
func run(ctx context.Context, stopCh <-chan struct{}, podResCli podresourcesapi.PodResourcesListerClient, ovsClient libovsdbclient.Client, featureStateChanged func(bool)) {

	// The file must be present at startup to enable the feature
	isFeatureEnabled, err := isFileNotEmpty(featureEnablerFile)
	if err != nil {
		klog.Warningf("Can't start OVS CPU affinity pinning: %v", err)
		return
	}

	if !isFeatureEnabled {
		klog.Info("OVS CPU affinity pinning disabled")
		return
	}

	klog.Infof("Starting OVS daemon CPU pinning")
	defer klog.Infof("Stopping OVS daemon CPU pinning")

	var fsnotifyEvents chan fsnotify.Event
	var fsnotifyErrors chan error

	// Watch the parent folder, as it's the only way to get events when the file is deleted and recreated.
	fileWatcher, err := createFileWatcherFor(filepath.Dir(featureEnablerFile))
	if err != nil {
		klog.Warningf("Can't create a watcher for %s. Pinning will not stop by deleting it: %v", featureEnablerFile, err)
		fsnotifyEvents = make(chan fsnotify.Event)
		fsnotifyErrors = make(chan error)
	} else {
		fsnotifyEvents = fileWatcher.Events
		fsnotifyErrors = fileWatcher.Errors
		defer fileWatcher.Close()
	}

	// we only need to check reservedSystemCPUs once at startup.
	// any change to KubeletConfig file triggers a node reboot, which also restarts the ovnkube-node pod.
	// as a result, this logic is re-executed automatically after every change.
	reservedCPUs, err := getReservedCPUs(kubeletConfigFilePath)
	if err != nil {
		klog.Warningf("Failed to get reservedSystemCPUs from kubelet config file on: %q: err=%v\n.Falling back to detect reserved from system", kubeletConfigFilePath, err)
		reservedCPUs, err = getReservedCPUsFallback(ctx, podResCli)
		if err != nil {
			klog.Warningf("Fallback method to obtain reservedSystemCPUs failed. err=%v", err)
			return
		}
	}
	klog.Infof("OVS CPU dynamic pinning reservedSystemCPUs set: %s", reservedCPUs)

	ticker := time.NewTicker(tickDuration)
	defer ticker.Stop()

	if featureStateChanged != nil {
		featureStateChanged(isFeatureEnabled)
	}

	for {
		select {
		case event, ok := <-fsnotifyEvents:
			if !ok {
				continue
			}

			// Since we are watching the entire folder, skip all the events not related to the enabler file
			if event.Name != featureEnablerFile {
				continue
			}

			wasFeatureEnabled := isFeatureEnabled
			isFeatureEnabled, err = isFileNotEmpty(featureEnablerFile)
			if err != nil {
				klog.Warningf("Error while reading [%s]: %v", featureEnablerFile, err)
				continue
			}

			if isFeatureEnabled {
				klog.Infof("OVS daemon CPU pinning feature enabled")
			} else {
				klog.Infof("OVS daemon CPU pinning feature NOT enabled")
			}

			if featureStateChanged != nil && wasFeatureEnabled != isFeatureEnabled {
				featureStateChanged(isFeatureEnabled)
			}

		case err, ok := <-fsnotifyErrors:
			if ok {
				klog.Errorf("Error watching for file [%s] changes: %s", featureEnablerFile, err)
			}

		case <-stopCh:
			return

		case <-ticker.C:
			if !isFeatureEnabled {
				continue
			}
			cpus, err := getNonPinnedCPUs(ctx, podResCli)
			if err != nil {
				klog.Warningf("Error while trying to get system non pinned CPUs: %v", err)
			}
			// add reservedSystemCPUs as well, because PodResourcesAPI does not count for them.
			cpus = cpus.Union(reservedCPUs)

			// When DPDK is enabled, remove the CPUs dedicated to PMD threads from the
			// affinity set so that non-PMD OVS threads don't compete with them.
			pmdCPUs, enabled := getPmdCPUs(ovsClient)
			if enabled && !pmdCPUs.IsEmpty() {
				cpus = cpus.Difference(pmdCPUs)
				klog.V(5).Infof("Excluding PMD CPUs %s from ovs-vswitchd affinity", pmdCPUs)
			}

			err = setOvsVSwitchdCPUAffinity(&cpus, enabled)
			if err != nil {
				klog.Warningf("Error while aligning ovs-vswitchd CPUs to current process: %v", err)
			}

			err = setOvsDBServerCPUAffinity(&cpus)
			if err != nil {
				klog.Warningf("Error while aligning ovsdb-server CPUs to current process: %v", err)
			}
		}
	}
}

func createFileWatcherFor(path string) (*fsnotify.Watcher, error) {
	fileWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create filesystem watcher: %w", err)
	}

	err = fileWatcher.Add(path)
	if err != nil {
		return nil, fmt.Errorf("unable to watch [%s] path: %w", path, err)
	}

	return fileWatcher, nil
}

func isFileNotEmpty(filename string) (bool, error) {
	f, err := os.Stat(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("can't get file information [%s]: %w", filename, err)
	}

	// get the size
	return f.Size() > 0, nil
}

func setOvsVSwitchdCPUAffinity(set *cpuset.CPUSet, dpdkEnabled bool) error {

	ovsVSwitchdPID, err := getOvsVSwitchdPIDFn()
	if err != nil {
		return fmt.Errorf("can't retrieve ovs-vswitchd PID: %w", err)
	}

	klog.V(5).Infof("Managing ovs-vswitchd[%s] daemon CPU affinity (dpdk=%v)", ovsVSwitchdPID, dpdkEnabled)
	return setProcessCPUAffinity(ovsVSwitchdPID, set, dpdkEnabled)
}

func setOvsDBServerCPUAffinity(set *cpuset.CPUSet) error {

	ovsDBserverPID, err := getOvsDBServerPIDFn()
	if err != nil {
		return fmt.Errorf("can't retrieve ovsdb-server PID: %w", err)
	}

	klog.V(5).Infof("Managing ovsdb-server[%s] daemon CPU affinity", ovsDBserverPID)
	return setProcessCPUAffinity(ovsDBserverPID, set, false)
}

// setProcessCPUAffinity sets the CPU affinity of a target process and all its threads
// to the specified CPU set. If the provided CPU set is empty, it falls back to using
// the current process's CPU affinity as the desired affinity.
//
// The function operates at the thread level, iterating through all threads (tasks)
// of the target process and setting their individual CPU affinities. This ensures
// that both the main process and any spawned threads are properly pinned to the
// specified CPUs.
//
// Parameters:
//   - targetPIDStr: string representation of the target process ID
//   - set: pointer to the desired CPU set; if empty, current process affinity is used
//   - skipPmds: whether to try to skip PMD threads from affinity setting or not
//
// Returns:
//   - error: any error encountered during PID conversion, affinity retrieval, or setting
//
// The function skips setting affinity only if all managed threads already have
// the desired CPU affinity. PMD threads are excluded when skipPmds is true.
// Individual thread affinity setting failures are logged as warnings but do not
// stop the overall operation.
func setProcessCPUAffinity(targetPIDStr string, set *cpuset.CPUSet, skipPmds bool) error {

	targetPID, err := strconv.Atoi(targetPIDStr)
	if err != nil {
		return fmt.Errorf("can't convert PID[%s] to integer: %w", targetPIDStr, err)
	}

	desiredProcessCPUs := convertCPUSet(set)
	if set.IsEmpty() {
		selfPID := os.Getpid()
		klog.InfoS("Given CPU set is empty, setting self CPU affinity", "selfPID", selfPID, "targetPID", targetPID)
		err = unix.SchedGetaffinity(selfPID, &desiredProcessCPUs)
		if err != nil {
			return fmt.Errorf("can't get own CPU affinity")
		}
	}

	var targetProcessCPUs unix.CPUSet
	err = unix.SchedGetaffinity(targetPID, &targetProcessCPUs)
	if err != nil {
		return fmt.Errorf("can't get process (PID:%d) CPU affinity: %w", targetPID, err)
	}

	taskIDs, err := getThreadsOfProcess(targetPID)
	if err != nil {
		return fmt.Errorf("can't get tasks of PID(%d):%w", targetPID, err)
	}

	if desiredProcessCPUs == targetProcessCPUs && tasksHaveCPUAffinity(targetPID, taskIDs, desiredProcessCPUs, skipPmds) {
		klog.V(5).Infof("Process[%d] and its managed tasks already match desired process affinity %s", targetPID, printCPUSet(desiredProcessCPUs))
		return nil
	}

	klog.Infof("Setting CPU affinity of PID(%d) (ntasks=%d) to %s, was %s", targetPID, len(taskIDs), printCPUSet(desiredProcessCPUs), printCPUSet(targetProcessCPUs))
	for _, taskID := range taskIDs {
		if skipPmds && isPmdThread(taskID) {
			continue
		}
		err = unix.SchedSetaffinity(taskID, &desiredProcessCPUs)
		if err != nil {
			// The task may have been stopped, don't break the loop and continue setting CPU affinity on other tasks.
			klog.Warningf("Error while setting CPU affinity of task(%d) PID(%d) to %s: %v", taskID, targetPID, printCPUSet(desiredProcessCPUs), err)
		}
	}

	return nil
}

// tasksHaveCPUAffinity verifies every managed task before taking the no-op path.
// A failed read cannot establish a match; let reconciliation attempt the update.
func tasksHaveCPUAffinity(targetPID int, taskIDs []int, desired unix.CPUSet, skipPmds bool) bool {
	for _, taskID := range taskIDs {
		if skipPmds && isPmdThread(taskID) {
			continue
		}
		var actual unix.CPUSet
		if err := unix.SchedGetaffinity(taskID, &actual); err != nil {
			klog.Warningf("Error while getting CPU affinity of task(%d) PID(%d): %v", taskID, targetPID, err)
			return false
		}
		if actual != desired {
			return false
		}
	}
	return true
}

// printCPUSet takes a unix.CPUSet and returns a string representation in canonical linux CPU list format.
// e.g. 0-5,8,10,12-3
//
// See http://man7.org/linux/man-pages/man7/cpuset.7.html#FORMATS
func printCPUSet(cpus unix.CPUSet) string {

	type rng struct {
		start int
		end   int
	}

	// Start with a fake range to avoid going out of range while looping
	ranges := []rng{{-2, -2}}

	// There is no public API to know the length of unix.CPUSet, so this counter is the
	// stopping condition for the loop
	remainingSetsCpus := cpus.Count()

	for i := 0; remainingSetsCpus > 0; i++ {
		if !cpus.IsSet(i) {
			continue
		}

		remainingSetsCpus--

		lastRange := ranges[len(ranges)-1]
		if lastRange.end == i-1 {
			ranges[len(ranges)-1].end++
		} else {
			ranges = append(ranges, rng{start: i, end: i})
		}
	}

	var result bytes.Buffer
	// discard the fake range with [1:]
	for _, r := range ranges[1:] {
		if r.start == r.end {
			result.WriteString(strconv.Itoa(r.start))
		} else {
			result.WriteString(fmt.Sprintf("%d-%d", r.start, r.end))
		}
		result.WriteString(",")
	}
	return strings.TrimRight(result.String(), ",")
}

func isPmdThread(tid int) bool {
	threadName, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", tid))
	if err != nil {
		klog.Errorf("Unable to get name of task ID %d", tid)
		return false
	}
	return strings.HasPrefix(string(threadName), PmdPrefix)
}

// getThreadsOfProcess returns the list of thread IDs of the given process
func getThreadsOfProcess(pid int) ([]int, error) {
	taskFolders, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, fmt.Errorf("unable to find %d tasks: %v", pid, err)
	}

	ret := []int{}
	for _, taskFolder := range taskFolders {
		taskID, err := strconv.Atoi(taskFolder.Name())
		if err != nil {
			return nil, fmt.Errorf("unable to get task ID of %d: %s, %v", pid, taskFolder.Name(), err)
		}

		ret = append(ret, taskID)
	}

	return ret, nil
}

func convertCPUSet(k8sSet *cpuset.CPUSet) unix.CPUSet {
	var uSet unix.CPUSet
	for _, cpu := range k8sSet.List() {
		uSet.Set(cpu)
	}
	return uSet
}

// getNonPinnedCPUs calculates and returns all allocatable CPUs on the node which are not
// exclusively pinned to any container. IOW it returns the CPUs that are dedicated for
// Burstable and BestEffort QoS containers
func getNonPinnedCPUs(ctx context.Context, podResCli podresourcesapi.PodResourcesListerClient) (cpuset.CPUSet, error) {
	// Get allocatable CPUs
	allocatableResp, err := podResCli.GetAllocatableResources(ctx, &podresourcesapi.AllocatableResourcesRequest{})
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("GetAllocatableResources failed: %w", err)
	}
	allocatableCPUs := cpuset.New(convertInt64ToInt(allocatableResp.CpuIds)...)

	// List pod resources and collect used CPUs
	listCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	listResp, err := podResCli.List(listCtx, &podresourcesapi.ListPodResourcesRequest{})
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("ListPodResources failed: %w", err)
	}

	usedCPUs := cpuset.New()
	for _, pod := range listResp.PodResources {
		for _, container := range pod.Containers {
			usedCPUs = usedCPUs.Union(cpuset.New(convertInt64ToInt(container.CpuIds)...))
		}
	}

	// Calculate the difference
	availableCPUs := allocatableCPUs.Difference(usedCPUs)
	return availableCPUs, nil
}

func convertInt64ToInt(int64s []int64) []int {
	ints := make([]int, len(int64s))
	for i, v := range int64s {
		ints[i] = int(v)
	}
	return ints
}

// getReservedCPUs reads a kubelet configuration file and extracts the ReservedSystemCPUs setting.
// It parses the kubelet config YAML/JSON file at the given path and returns the set of CPUs
// that are reserved for system use according to the kubelet configuration.
//
// Parameters:
//   - path: filesystem path to the kubelet configuration file
//
// Returns:
//   - cpuset.CPUSet: the set of CPUs reserved for system use
//   - error: any error encountered while reading or parsing the configuration
//
// Note: An empty ReservedSystemCPUs field in the config is not considered an error,
// it simply returns an empty CPU set.
func getReservedCPUs(path string) (cpuset.CPUSet, error) {
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)

	if err := kubeletconfigv1beta1.AddToScheme(scheme); err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("failed to add kubelet config scheme: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("failed to read file: %s: %w", path, err)
	}

	obj, _, err := codecs.UniversalDecoder(kubeletconfigv1beta1.SchemeGroupVersion).Decode(data, nil, nil)
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("failed to decode kubelet config: %w", err)
	}

	kc, ok := obj.(*kubeletconfigv1beta1.KubeletConfiguration)
	if !ok {
		return cpuset.CPUSet{}, fmt.Errorf("decoded object is not a KubeletConfiguration")
	}

	// kc.ReservedSystemCPUs could be empty. it's not a desired state, but not considered as an error either.
	cset, err := cpuset.Parse(kc.ReservedSystemCPUs)
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("failed to parse reservedSystemCPUs: %w", err)
	}

	return cset, nil
}

// getReservedCPUsFallback determines the set of reserved CPUs by calculating the difference
// between online CPUs and allocatable CPUs. This method serves as a fallback when the
// kubelet configuration file is not available or cannot be parsed.
//
// The logic is: Reserved CPUs = Online CPUs - Allocatable CPUs
// This works because reserved CPUs are those that are online but not available for
// pod allocation by the kubelet.
//
// Parameters:
//   - ctx: context for the operation
//   - podResCli: client for querying the kubelet's pod resources API
//
// Returns:
//   - cpuset.CPUSet: the set of CPUs reserved for system use
//   - error: any error encountered while querying CPU information
func getReservedCPUsFallback(ctx context.Context, podResCli podresourcesapi.PodResourcesListerClient) (cpuset.CPUSet, error) {
	onlineCPUs, err := getOnlineCPUs()
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("failed to get onlineCPUs CPUs %w", err)
	}
	allocatableCPUs, err := getAllocatableCPUs(ctx, podResCli)
	if err != nil {
		return cpuset.CPUSet{}, err
	}
	// online - allocatable is the reserved set
	return onlineCPUs.Difference(allocatableCPUs), nil

}

// getOnlineCPUs retrieves the set of CPUs that are currently online on the system.
// It reads from the Linux sysfs interface at /sys/devices/system/cpu/online which
// contains a comma-separated list or range of CPU IDs that are currently online
// and available for use by the kernel.
//
// Returns:
//   - cpuset.CPUSet: the set of CPUs that are currently online
//   - error: any error encountered while reading the sysfs file or parsing the CPU list
//
// Example sysfs content: "0-3,8-11" (CPUs 0,1,2,3,8,9,10,11 are online)
func getOnlineCPUs() (cpuset.CPUSet, error) {
	onlineCPUList, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return cpuset.CPUSet{}, err
	}
	return cpuset.Parse(strings.TrimSpace(string(onlineCPUList)))
}

func getAllocatableCPUs(ctx context.Context, podResCli podresourcesapi.PodResourcesListerClient) (cpuset.CPUSet, error) {
	getCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	allocatableResp, err := podResCli.GetAllocatableResources(getCtx, &podresourcesapi.AllocatableResourcesRequest{})
	if err != nil {
		return cpuset.CPUSet{}, fmt.Errorf("GetAllocatableResources failed: %w", err)
	}
	return cpuset.New(convertInt64ToInt(allocatableResp.CpuIds)...), nil
}

// getPmdCPUs returns the PMD CPU set and whether DPDK is enabled.
func getPmdCPUs(ovsClient libovsdbclient.Client) (cpuset.CPUSet, bool) {
	ovs, err := ovsops.GetOpenvSwitch(ovsClient)
	if err != nil {
		klog.V(5).Infof("Cannot read Open_vSwitch table for pmd-cpu-mask: %v", err)
		return cpuset.New(), false
	}
	if !ovs.DpdkInitialized {
		return cpuset.New(), false
	}

	mask, ok := ovs.OtherConfig["pmd-cpu-mask"]
	if !ok || mask == "" {
		return cpuset.New(), true
	}

	pmdCPUs, err := parsePmdCpuMask(mask)
	if err != nil {
		klog.Warningf("Failed to parse pmd-cpu-mask %q: %v", mask, err)
		return cpuset.New(), true
	}
	return pmdCPUs, true
}

// parsePmdCpuMask parses a hex bitmask string (e.g. "0xc", "C", "0x0006")
// into a cpuset.CPUSet.
func parsePmdCpuMask(mask string) (cpuset.CPUSet, error) {
	mask = strings.TrimSpace(mask)
	mask = strings.TrimPrefix(mask, "0x")
	mask = strings.TrimPrefix(mask, "0X")
	if mask == "" {
		return cpuset.New(), nil
	}

	v := new(big.Int)
	if _, ok := v.SetString(mask, 16); !ok {
		return cpuset.New(), fmt.Errorf("invalid hex mask: %q", mask)
	}

	var positions []int
	for i := 0; i < v.BitLen(); i++ {
		if v.Bit(i) == 1 {
			positions = append(positions, i)
		}
	}
	return cpuset.New(positions...), nil
}
