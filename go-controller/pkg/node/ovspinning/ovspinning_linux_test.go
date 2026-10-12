// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux
// +build linux

package ovspinning

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"k8s.io/klog/v2"
	kubeletpodresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
	"k8s.io/utils/cpuset"

	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	mocks "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/kubelet/pkg/apis/podresources/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestAlignCPUAffinity(t *testing.T) {
	testCases := []struct {
		name            string
		allocatableCPUs []int64
		reservedCPUs    []int
		usedCPUs        [][]int64
	}{
		{
			name:            "Simple split with some used",
			allocatableCPUs: []int64{0, 1},
			reservedCPUs:    []int{2, 3},
			usedCPUs:        [][]int64{{1}},
		},
		{
			name:            "All allocatable used",
			allocatableCPUs: []int64{0, 1},
			reservedCPUs:    []int{2, 3},
			usedCPUs:        [][]int64{{0}, {1}},
		},
		{
			name:            "No used CPUs",
			allocatableCPUs: []int64{0, 1},
			reservedCPUs:    []int{2, 3},
			usedCPUs:        [][]int64{},
		},
		{
			name:            "Partial usage with multiple containers",
			allocatableCPUs: []int64{0, 1, 2},
			reservedCPUs:    []int{3},
			usedCPUs:        [][]int64{{0}, {2}},
		},
		{
			name:            "low cpu capacity: Simple split with some used",
			allocatableCPUs: []int64{1, 2, 3},
			reservedCPUs:    []int{0},
			usedCPUs:        [][]int64{{3}},
		},
		{
			name:            "Empty. should use self affinity",
			allocatableCPUs: []int64{},
			reservedCPUs:    []int{},
			usedCPUs:        [][]int64{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			expectedCPUs := calculateExpectedCPUs(tc.allocatableCPUs, tc.reservedCPUs, tc.usedCPUs)
			// check that we can run this test on the tested machine
			numCPUs := runtime.NumCPU()
			if !expectedCPUsValid(expectedCPUs.List(), numCPUs) {
				t.Skipf("Skipping test case %q: CPU ID out of range for this machine (have %d CPUs)", tc.name, numCPUs)
			}

			ovsDBPid, ovsDBStop := mockOvsdbProcess(t)
			defer ovsDBStop()

			ovsVSwitchdPid, ovsVSwitchdStop := mockOvsVSwitchdProcess(t)
			defer ovsVSwitchdStop()

			defer setTickDuration(20 * time.Millisecond)()
			defer mockFeatureEnableFile(t, "1")()
			defer mockKubeletConfigFile(t, cpuset.New(tc.reservedCPUs...))()

			ovsClient, ovsTestCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: []libovsdbtest.TestData{
					&vswitchd.OpenvSwitch{UUID: "root-ovs"},
				},
			})
			require.NoError(t, err)
			defer ovsTestCtx.Cleanup()

			var wg sync.WaitGroup
			stopCh := make(chan struct{})
			defer func() {
				close(stopCh)
				wg.Wait()
			}()

			featureStates := make(chan bool, 1)
			waitForFeatureState := func(enabled bool) {
				t.Helper()
				select {
				case actual := <-featureStates:
					require.Equal(t, enabled, actual)
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out waiting for pinning enabled=%v", enabled)
				}
			}
			mockClient := mocks.NewPodResourcesListerClient(t)
			mockClient.On("GetAllocatableResources", mock.Anything, mock.Anything).Return(
				&kubeletpodresourcesv1.AllocatableResourcesResponse{CpuIds: tc.allocatableCPUs}, nil)
			mockClient.On("List", mock.Anything, mock.Anything).Return(
				buildListPodResourcesResponse(tc.usedCPUs), nil)
			wg.Add(1)
			go func() {
				// Be sure the system under test goroutine is finished before cleaning
				defer wg.Done()
				run(context.Background(), stopCh, mockClient, ovsClient, func(enabled bool) {
					select {
					case featureStates <- enabled:
					case <-stopCh:
					}
				})
			}()

			// Affinity may already match before Run starts; wait for its watcher to be ready.
			waitForFeatureState(true)

			expectedUnixCPUSet := convertCPUSet(&expectedCPUs)
			if expectedCPUs.IsEmpty() {
				klog.Info("expectedCPUs is empty, using running process's self-affinity")
				// use self-affinity
				var pidSelfCPUs unix.CPUSet
				err := unix.SchedGetaffinity(os.Getpid(), &pidSelfCPUs)
				require.NoError(t, err)

				expectedCPUs, err = convertUnixCPUSetToK8sCPUSet(pidSelfCPUs)
				assert.NoError(t, err)
				expectedUnixCPUSet = pidSelfCPUs
			}
			klog.Infof("Test CPU Affinity %s", expectedCPUs)

			assertPIDHasSchedAffinity(t, ovsVSwitchdPid, expectedUnixCPUSet)
			assertPIDHasSchedAffinity(t, ovsDBPid, expectedUnixCPUSet)

			// Disable the feature by making the enabler file empty
			err = os.WriteFile(featureEnablerFile, []byte(""), 0)
			require.NoError(t, err)

			// Wait until the loop has processed disabling and finished any prior update.
			waitForFeatureState(false)

			var tmpCPUset unix.CPUSet
			tmpCPUset.Set(0)
			assert.NoError(t, unix.SchedSetaffinity(ovsVSwitchdPid, &tmpCPUset))
			assert.NoError(t, unix.SchedSetaffinity(ovsDBPid, &tmpCPUset))

			// Should not set the affinity back
			assertNeverPIDHasSchedAffinity(t, ovsVSwitchdPid, expectedUnixCPUSet)
			assertNeverPIDHasSchedAffinity(t, ovsDBPid, expectedUnixCPUSet)

			// Enable the feature back by putting contents in the enabler file
			err = os.WriteFile(featureEnablerFile, []byte("1"), 0)
			require.NoError(t, err)
			waitForFeatureState(true)

			assertPIDHasSchedAffinity(t, ovsVSwitchdPid, expectedUnixCPUSet)
			assertPIDHasSchedAffinity(t, ovsDBPid, expectedUnixCPUSet)

			// Disable the feature by deleting the enabler file
			klog.Infof("Remove the enabler file to disable the feature")
			err = os.Remove(featureEnablerFile)
			require.NoError(t, err)

			// Wait until the loop has processed disabling and finished any prior update.
			waitForFeatureState(false)

			tmpCPUset.Set(1)
			assert.NoError(t, unix.SchedSetaffinity(ovsVSwitchdPid, &tmpCPUset))
			assert.NoError(t, unix.SchedSetaffinity(ovsDBPid, &tmpCPUset))

			// Should not set the affinity back
			assertNeverPIDHasSchedAffinity(t, ovsVSwitchdPid, expectedUnixCPUSet)
			assertNeverPIDHasSchedAffinity(t, ovsDBPid, expectedUnixCPUSet)

			// Re-enable the feature back by recreating the enabler file
			klog.Infof("Re-enable the feature")
			err = os.WriteFile(featureEnablerFile, []byte("1"), 0)
			require.NoError(t, err)
			waitForFeatureState(true)

			assertPIDHasSchedAffinity(t, ovsVSwitchdPid, expectedUnixCPUSet)
			assertPIDHasSchedAffinity(t, ovsDBPid, expectedUnixCPUSet)
		})
	}
}

func TestIsFileNotEmpty(t *testing.T) {
	defer mockFeatureEnableFile(t, "")()

	result, err := isFileNotEmpty(featureEnablerFile)
	require.NoError(t, err)
	assert.False(t, result)

	err = os.WriteFile(featureEnablerFile, []byte("1"), 0)
	require.NoError(t, err)
	result, err = isFileNotEmpty(featureEnablerFile)
	require.NoError(t, err)
	assert.True(t, result)

	os.Remove(featureEnablerFile)
	result, err = isFileNotEmpty(featureEnablerFile)
	require.NoError(t, err)
	assert.False(t, result)
}

func TestPrintCPUSetAll(t *testing.T) {
	var x unix.CPUSet
	for i := 0; i < 4; i++ {
		x.Set(i)
	}

	assert.Equal(t,
		"0-3",
		printCPUSet(x),
	)

	assert.Empty(t,
		printCPUSet(unix.CPUSet{}),
	)
}

func TestPrintCPUSetRanges(t *testing.T) {
	var x unix.CPUSet

	x.Set(0)
	x.Set(2)
	x.Set(3)

	assert.Equal(t,
		"0,2-3",
		printCPUSet(x),
	)
}

func TestGetReservedCPUs(t *testing.T) {
	tests := []struct {
		name            string
		yamlContent     string
		expectError     bool
		expectedCPUSet  string
		expectedIsEmpty bool
	}{
		{
			name: "valid config",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
reservedSystemCPUs: "0-1,3"
`,
			expectError:    false,
			expectedCPUSet: "0-1,3",
		},
		{
			name: "empty reservedSystemCPUs",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
`,
			expectError:     false,
			expectedIsEmpty: true,
		},
		{
			name: "invalid cpuset string",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
reservedSystemCPUs: "not-a-valid-range"
`,
			expectError: true,
		},
		{
			name: "invalid YAML format",
			yamlContent: `
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
reservedSystemCPUs: [0,1
`,
			expectError: true,
		},
		{
			name:        "file not found",
			yamlContent: "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.name == "file not found" {
				path = "/nonexistent/path.yaml"
			} else {
				path = writeTempFile(t, tt.yamlContent)
			}

			cset, err := getReservedCPUs(path)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.expectedIsEmpty && !cset.IsEmpty() {
				t.Errorf("expected empty cpuset, got %s", cset.String())
			} else if tt.expectedCPUSet != "" {
				expected, _ := cpuset.Parse(tt.expectedCPUSet)
				if !cset.Equals(expected) {
					t.Errorf("expected cpuset %s, got %s", expected.String(), cset.String())
				}
			}
		})
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "kubelet-config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	return path
}

func mockOvsdbProcess(t *testing.T) (int, func()) {
	t.Helper()
	ctx, stopCmd := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "sleep", "60")

	err := cmd.Start()
	assert.NoError(t, err)

	previousGetter := getOvsDBServerPIDFn
	getOvsDBServerPIDFn = func() (string, error) {
		return fmt.Sprintf("%d", cmd.Process.Pid), nil
	}

	return cmd.Process.Pid, func() {
		stopCmd()
		_ = cmd.Wait()
		getOvsDBServerPIDFn = previousGetter
	}
}

func mockOvsVSwitchdProcess(t *testing.T) (int, func()) {
	t.Helper()
	pid, _, _ := startAffinityTestProcess(t)
	previousGetter := getOvsVSwitchdPIDFn
	getOvsVSwitchdPIDFn = func() (string, error) {
		return fmt.Sprintf("%d", pid), nil
	}
	return pid, func() {
		getOvsVSwitchdPIDFn = previousGetter
	}
}

func setTickDuration(d time.Duration) func() {
	previousValue := tickDuration
	tickDuration = d

	return func() {
		tickDuration = previousValue
	}
}

func mockFeatureEnableFile(t *testing.T, data string) func() {
	t.Helper()
	f, err := os.CreateTemp("", "enable_dynamic_cpu_affinity")
	require.NoError(t, err)

	previousValue := featureEnablerFile
	featureEnablerFile = f.Name()

	err = os.WriteFile(featureEnablerFile, []byte(data), 0)
	assert.NoError(t, err)

	return func() {
		featureEnablerFile = previousValue
		_ = os.Remove(f.Name())
	}
}

func mockKubeletConfigFile(t *testing.T, reservedCPUs cpuset.CPUSet) func() {
	t.Helper()
	f, err := os.CreateTemp("", "kubelet.conf")
	require.NoError(t, err)

	previousValue := kubeletConfigFilePath
	kubeletConfigFilePath = f.Name()

	data := fmt.Sprintf(`
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
reservedSystemCPUs: %q
`, reservedCPUs)
	err = os.WriteFile(kubeletConfigFilePath, []byte(data), 0)
	assert.NoError(t, err)

	return func() {
		kubeletConfigFilePath = previousValue
		_ = os.Remove(f.Name())
	}
}

func assertPIDHasSchedAffinity(t *testing.T, pid int, expectedCPUSet unix.CPUSet) {
	t.Helper()
	var actual unix.CPUSet
	assert.Eventually(t, func() bool {
		err := unix.SchedGetaffinity(pid, &actual)
		assert.NoError(t, err)

		return actual == expectedCPUSet
	}, 2*time.Second, 10*time.Millisecond, "pid[%d] Expected CPUSet %#x != Actual CPUSet %#x", pid, expectedCPUSet, actual)

	tasks, err := getThreadsOfProcess(pid)
	require.NoError(t, err)

	for _, task := range tasks {
		assert.Eventually(t, func() bool {
			err := unix.SchedGetaffinity(task, &actual)
			assert.NoError(t, err)

			return actual == expectedCPUSet
		}, time.Second, 10*time.Millisecond, "task[%d] of process[%d]  Expected CPUSet %0x != Actual CPUSet %0x", task, pid, expectedCPUSet, actual)
	}
}

func assertNeverPIDHasSchedAffinity(t *testing.T, pid int, targetCPUSet unix.CPUSet) {
	t.Helper()
	var actual unix.CPUSet
	assert.Never(t, func() bool {
		err := unix.SchedGetaffinity(pid, &actual)
		assert.NoError(t, err)
		return actual == targetCPUSet
	}, 1*time.Second, 10*time.Millisecond, "pid[%d]  == Actual CPUSet %#x expected to be different than %#x", pid, actual, targetCPUSet)
}

// convertUnixCPUSetToK8sCPUSet converts a unix.CPUSet to a k8s.io/utils/cpuset.CPUSet
func convertUnixCPUSetToK8sCPUSet(unixSet unix.CPUSet) (cpuset.CPUSet, error) {
	var cpus []int
	const maxCPUs = 1024 // Maximum CPUs supported by unix.CPUSet (CPU_SETSIZE on Linux)
	for i := 0; i < maxCPUs; i++ {
		if unixSet.IsSet(i) {
			cpus = append(cpus, i)
		}
	}
	if len(cpus) == 0 {
		return cpuset.CPUSet{}, fmt.Errorf("no CPUs found in unix.CPUSet")
	}
	return cpuset.New(cpus...), nil
}

// calculateExpectedCPUs computes (allocatable ∪ reserved) - used
func calculateExpectedCPUs(allocatableCPUs []int64, reservedCPUs []int, usedCPUs [][]int64) cpuset.CPUSet {
	// Convert slices to CPUSet
	allocSet := cpuset.New(convertInt64ToInt(allocatableCPUs)...)
	reservedSet := cpuset.New(reservedCPUs...)

	unionSet := allocSet.Union(reservedSet)

	// Flatten usedCPUs and build a CPUSet
	var flatUsed []int
	for _, grp := range usedCPUs {
		flatUsed = append(flatUsed, convertInt64ToInt(grp)...)
	}
	usedSet := cpuset.New(flatUsed...)

	// Final result: (alloc ∪ reserved) - used
	return unionSet.Difference(usedSet)
}

func expectedCPUsValid(cpus []int, max int) bool {
	for _, cpu := range cpus {
		if cpu >= max {
			return false
		}
	}
	return true
}

// buildListPodResourcesResponse builds a ListPodResourcesResponse from usedCPUs test data.
// usedCPUs is a slice of CPU ID slices, one per container.
func buildListPodResourcesResponse(usedCPUs [][]int64) *kubeletpodresourcesv1.ListPodResourcesResponse {
	var podResources []*kubeletpodresourcesv1.PodResources

	if len(usedCPUs) > 0 {
		var containers []*kubeletpodresourcesv1.ContainerResources
		for i, containerCPUs := range usedCPUs {
			if len(containerCPUs) > 0 {
				containers = append(containers, &kubeletpodresourcesv1.ContainerResources{
					Name:   fmt.Sprintf("container-%d", i),
					CpuIds: containerCPUs,
				})
			}
		}

		if len(containers) > 0 {
			podResources = append(podResources, &kubeletpodresourcesv1.PodResources{
				Name:       "test-pod",
				Namespace:  "default",
				Containers: containers,
			})
		}
	}

	return &kubeletpodresourcesv1.ListPodResourcesResponse{
		PodResources: podResources,
	}
}

func TestGetPmdCPUs(t *testing.T) {
	tests := []struct {
		name            string
		ovsData         []libovsdbtest.TestData
		expectedCPUs    cpuset.CPUSet
		expectedEnabled bool
	}{
		{
			name:            "empty OVS table returns empty set, not enabled",
			ovsData:         []libovsdbtest.TestData{},
			expectedCPUs:    cpuset.New(),
			expectedEnabled: false,
		},
		{
			name: "dpdk not initialized returns empty set, not enabled",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", DpdkInitialized: false,
					OtherConfig: map[string]string{"pmd-cpu-mask": "0xc"}},
			},
			expectedCPUs:    cpuset.New(),
			expectedEnabled: false,
		},
		{
			name: "dpdk initialized, no pmd-cpu-mask: enabled but empty CPUs",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", DpdkInitialized: true,
					OtherConfig: map[string]string{}},
			},
			expectedCPUs:    cpuset.New(),
			expectedEnabled: true,
		},
		{
			name: "dpdk initialized, empty pmd-cpu-mask: enabled but empty CPUs",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", DpdkInitialized: true,
					OtherConfig: map[string]string{"pmd-cpu-mask": ""}},
			},
			expectedCPUs:    cpuset.New(),
			expectedEnabled: true,
		},
		{
			name: "dpdk initialized, valid mask 0xc: enabled, CPUs 2 and 3",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", DpdkInitialized: true,
					OtherConfig: map[string]string{"pmd-cpu-mask": "0xc"}},
			},
			expectedCPUs:    cpuset.New(2, 3),
			expectedEnabled: true,
		},
		{
			name: "dpdk initialized, invalid mask: enabled but empty CPUs",
			ovsData: []libovsdbtest.TestData{
				&vswitchd.OpenvSwitch{UUID: "root-ovs", DpdkInitialized: true,
					OtherConfig: map[string]string{"pmd-cpu-mask": "xyz"}},
			},
			expectedCPUs:    cpuset.New(),
			expectedEnabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ovsClient, testCtx, err := libovsdbtest.NewOVSTestHarness(libovsdbtest.TestSetup{
				OVSData: tt.ovsData,
			})
			require.NoError(t, err)
			defer testCtx.Cleanup()

			cpus, enabled := getPmdCPUs(ovsClient)
			assert.Equal(t, tt.expectedCPUs, cpus)
			assert.Equal(t, tt.expectedEnabled, enabled)
		})
	}
}

func TestParsePmdCpuMask(t *testing.T) {
	tests := []struct {
		name        string
		mask        string
		expected    []int
		expectError bool
	}{
		{name: "hex with 0x prefix", mask: "0xc", expected: []int{2, 3}},
		{name: "hex with 0X prefix", mask: "0XC", expected: []int{2, 3}},
		{name: "bare hex lowercase", mask: "c", expected: []int{2, 3}},
		{name: "bare hex uppercase", mask: "C", expected: []int{2, 3}},
		{name: "single bit", mask: "0x1", expected: []int{0}},
		{name: "multiple bits", mask: "0x6", expected: []int{1, 2}},
		{name: "all low byte", mask: "0xff", expected: []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{name: "zero", mask: "0x0", expected: []int{}},
		{name: "leading zeros", mask: "0x0000000000000006", expected: []int{1, 2}},
		{name: "empty string", mask: "", expected: []int{}},
		{name: "whitespace only", mask: "  ", expected: []int{}},
		{name: "with whitespace", mask: " 0xc ", expected: []int{2, 3}},
		{name: "invalid", mask: "xyz", expectError: true},
		{name: "CPU 64 only", mask: "0x10000000000000000", expected: []int{64}},
		{name: "CPUs spanning 64-bit boundary", mask: "0x10000000000000003", expected: []int{0, 1, 64}},
		{name: "CPU 128 only", mask: "0x100000000000000000000000000000000", expected: []int{128}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parsePmdCpuMask(tt.mask)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, cpuset.New(tt.expected...), result)
		})
	}
}
