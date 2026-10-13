// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package ovspinning

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"k8s.io/utils/cpuset"
)

// Reserve the child's main thread before testing starts its test goroutines.
// Otherwise a named worker could run on the process leader's thread.
func init() {
	if os.Getenv("OVNK_TEST_AFFINITY_HELPER") == "1" {
		runtime.LockOSThread()
	}
}

// TestAffinityHelperProcess runs in a child so tests never change the affinity
// of the test runner. The named threads stay alive until the parent kills it.
func TestAffinityHelperProcess(t *testing.T) {
	if os.Getenv("OVNK_TEST_AFFINITY_HELPER") != "1" {
		return
	}
	tids := make([]int, 0, 2)
	for _, name := range []string{"ovs-test", "pmd-test"} {
		ready := make(chan int)
		go func(name string) {
			runtime.LockOSThread()
			tid := unix.Gettid()
			if err := os.WriteFile(fmt.Sprintf("/proc/self/task/%d/comm", tid), []byte(name), 0600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			ready <- tid
			time.Sleep(time.Hour)
		}(name)
		tids = append(tids, <-ready)
	}
	require.NoError(t, json.NewEncoder(os.Stdout).Encode(tids))
	time.Sleep(time.Hour)
}

func startAffinityTestProcess(t *testing.T) (pid, workerTID, pmdTID int) {
	t.Helper()
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestAffinityHelperProcess$")
	cmd.Env = append(os.Environ(), "OVNK_TEST_AFFINITY_HELPER=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	var tids []int
	require.NoError(t, json.NewDecoder(stdout).Decode(&tids))
	require.Len(t, tids, 2)
	return cmd.Process.Pid, tids[0], tids[1]
}

func TestReconcileThreadCPUAffinity(t *testing.T) {
	var allowed unix.CPUSet
	require.NoError(t, unix.SchedGetaffinity(os.Getpid(), &allowed))
	available, err := convertUnixCPUSetToK8sCPUSet(allowed)
	require.NoError(t, err)
	if available.Size() < 2 {
		t.Skip("requires two allowed CPUs")
	}
	desired := cpuset.New(available.List()[0])
	desiredMask := convertCPUSet(&desired)
	different := cpuset.New(available.List()[1])
	differentMask := convertCPUSet(&different)

	for _, skipPmds := range []bool{false, true} {
		t.Run(fmt.Sprintf("skipPMDs=%v", skipPmds), func(t *testing.T) {
			pid, worker, pmd := startAffinityTestProcess(t)
			// Initialize every thread, then diverge only the two stable worker threads.
			require.NoError(t, setProcessCPUAffinity(strconv.Itoa(pid), &desired, false))
			require.NoError(t, unix.SchedSetaffinity(worker, &differentMask))
			require.NoError(t, unix.SchedSetaffinity(pmd, &differentMask))
			var actual unix.CPUSet
			require.NoError(t, unix.SchedGetaffinity(pid, &actual))
			require.Equal(t, desiredMask, actual, "leader must match before reconciliation")

			require.NoError(t, setProcessCPUAffinity(strconv.Itoa(pid), &desired, skipPmds))
			require.NoError(t, unix.SchedGetaffinity(worker, &actual))
			assert.Equal(t, desiredMask, actual, "a matching leader must not hide a stale worker")
			require.NoError(t, unix.SchedGetaffinity(pmd, &actual))
			if skipPmds {
				assert.Equal(t, differentMask, actual, "DPDK PMD affinity must be preserved")
			} else {
				assert.Equal(t, desiredMask, actual, "without DPDK, PMD-named threads are managed too")
			}
			assert.True(t, tasksHaveCPUAffinity(pid, []int{pid, worker, pmd}, desiredMask, skipPmds),
				"an excluded PMD must not prevent the no-op path")
		})
	}
}

func TestTasksHaveCPUAffinityReadError(t *testing.T) {
	var desired unix.CPUSet
	require.NoError(t, unix.SchedGetaffinity(os.Getpid(), &desired))
	assert.False(t, tasksHaveCPUAffinity(os.Getpid(), []int{os.Getpid(), -1}, desired, false),
		"an unreadable task must not count as matching")
}
