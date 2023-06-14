//go:build linux
// +build linux

package ovspinning

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

func TestAlignCPUAffinity(t *testing.T) {

	ovsDBPid, ovsDBStop := mockOvsdbProcess(t)
	defer ovsDBStop()

	ovsVSwitchdPid, ovsVSwitchdStop := mockOvsVSwitchdProcess(t)
	defer ovsVSwitchdStop()

	defer setTickDuration(20 * time.Millisecond)()
	defer mockFeatureEnableFile(t, "1")()

	var wg sync.WaitGroup
	stopCh := make(chan struct{})
	defer func() {
		close(stopCh)
		wg.Wait()
	}()

	wg.Add(1)
	go func() {
		// Be sure the system under test goroutine is finished before cleaning
		defer wg.Done()
		Run(stopCh)
	}()

	var initialCPUset unix.CPUSet
	err := unix.SchedGetaffinity(os.Getpid(), &initialCPUset)
	assert.NoError(t, err)

	defer func() {
		// Restore any previous CPU affinity value it was in place before the test
		err = unix.SchedSetaffinity(os.Getpid(), &initialCPUset)
		assert.NoError(t, err)
	}()

	assert.Greater(t, runtime.NumCPU(), 1)

	for i := 0; i < runtime.NumCPU(); i++ {
		var tmpCPUset unix.CPUSet
		tmpCPUset.Set(i)
		err = unix.SchedSetaffinity(os.Getpid(), &tmpCPUset)
		assert.NoError(t, err)

		klog.Infof("Test CPU Affinity %x", tmpCPUset)

		assertPIDHasSchedAffinity(t, ovsVSwitchdPid, tmpCPUset)
		assertPIDHasSchedAffinity(t, ovsDBPid, tmpCPUset)
	}

	// Disable the feature by making the enabler file empty
	ioutil.WriteFile(featureEnablerFile, []byte(""), 0)
	assert.NoError(t, err)

	var tmpCPUset unix.CPUSet
	tmpCPUset.Set(0)
	err = unix.SchedSetaffinity(os.Getpid(), &tmpCPUset)
	assert.NoError(t, err)

	assertNeverPIDHasSchedAffinity(t, ovsVSwitchdPid, tmpCPUset)
	assertNeverPIDHasSchedAffinity(t, ovsDBPid, tmpCPUset)
}

func TestIsFileNotEmpty(t *testing.T) {

	defer mockFeatureEnableFile(t, "")()

	result, err := isFileNotEmpty(featureEnablerFile)
	assert.NoError(t, err)
	assert.False(t, result)

	ioutil.WriteFile(featureEnablerFile, []byte("1"), 0)
	result, err = isFileNotEmpty(featureEnablerFile)
	assert.NoError(t, err)
	assert.True(t, result)

	os.Remove(featureEnablerFile)
	result, err = isFileNotEmpty(featureEnablerFile)
	assert.NoError(t, err)
	assert.False(t, result)
}

func TestPrintCPUSetAll(t *testing.T) {
	var x unix.CPUSet
	for i := 0; i < 16; i++ {
		x.Set(i)
	}

	assert.Equal(t,
		"0-15",
		printCPUSet(x),
	)

	assert.Equal(t,
		"",
		printCPUSet(unix.CPUSet{}),
	)
}

func TestPrintCPUSetRanges(t *testing.T) {
	var x unix.CPUSet

	x.Set(2)
	x.Set(3)
	x.Set(6)
	x.Set(7)
	x.Set(8)
	x.Set(14)

	assert.Equal(t,
		"2-3,6-8,14",
		printCPUSet(x),
	)
}

func mockOvsdbProcess(t *testing.T) (int, func()) {
	ctx, stopCmd := context.WithCancel(context.Background())
	defer stopCmd()

	cmd := exec.CommandContext(ctx, "sleep", "10")

	err := cmd.Start()
	assert.NoError(t, err)

	previousGetter := getOvsDBServerPIDFn
	getOvsDBServerPIDFn = func() (string, error) {
		return fmt.Sprintf("%d", cmd.Process.Pid), nil
	}

	return cmd.Process.Pid, func() {
		stopCmd()
		getOvsDBServerPIDFn = previousGetter
	}
}

func mockOvsVSwitchdProcess(t *testing.T) (int, func()) {
	ctx, stopCmd := context.WithCancel(context.Background())
	defer stopCmd()

	cmd := exec.CommandContext(ctx, "sleep", "10")

	err := cmd.Start()
	assert.NoError(t, err)

	previousGetter := getOvsVSwitchdPIDFn
	getOvsVSwitchdPIDFn = func() (string, error) {
		return fmt.Sprintf("%d", cmd.Process.Pid), nil
	}

	return cmd.Process.Pid, func() {
		stopCmd()
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

	f, err := ioutil.TempFile("", "enable_dynamic_cpu_affinity")
	assert.NoError(t, err)

	previousValue := featureEnablerFile
	featureEnablerFile = f.Name()

	ioutil.WriteFile(featureEnablerFile, []byte(data), 0)
	assert.NoError(t, err)

	return func() {
		featureEnablerFile = previousValue
		os.Remove(f.Name())
	}
}

func assertPIDHasSchedAffinity(t *testing.T, pid int, expectedCPUSet unix.CPUSet) {
	var actual unix.CPUSet
	assert.Eventually(t, func() bool {
		err := unix.SchedGetaffinity(pid, &actual)
		assert.NoError(t, err)

		return actual == expectedCPUSet
	}, time.Second, 10*time.Millisecond, "pid[%d] Expected CPUSet %0x != Actual CPUSet %0x", pid, expectedCPUSet, actual)
}

func assertNeverPIDHasSchedAffinity(t *testing.T, pid int, targetCPUSet unix.CPUSet) {
	var actual unix.CPUSet
	assert.Never(t, func() bool {
		err := unix.SchedGetaffinity(pid, &actual)
		assert.NoError(t, err)

		return actual == targetCPUSet
	}, time.Second, 10*time.Millisecond, "pid[%d]  == Actual CPUSet %0x expected to be different than %0x", pid, actual, targetCPUSet)
}
