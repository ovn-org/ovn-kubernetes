package testing

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"

	"k8s.io/klog/v2"
	kexec "k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"
)

// KCmd is a callback spec returning a k8s exec command
type KCmd func(cmd string, args ...string) kexec.Cmd

// FakeExec is a convenience struct that wraps testing.FakeExec
type FakeExec struct {
	// Activate this for a loose comparison of executed OVN commands.
	// We will in such a case ignore order when comparing all executed commands during the run of a test case.
	// This is important when defining test cases with multiple resources (or multiple resource watchers) of
	// the same type and not being able to rely on a deterministic order of incomming watch events.
	looseCompare       bool
	expectedCommands   []*ExpectedCmd
	executedCommands   []string
	mu                 sync.Mutex
	receivedUnexpected bool
	unexpectedCommand  string
}

var _ kexec.Interface = &FakeExec{}

// NewFakeExec returns a new FakeExec with a strict order compare
func NewFakeExec() *FakeExec {
	return newFakeExec(false)
}

// NewFakeExec returns a new FakeExec with a strict order compare
func NewLooseCompareFakeExec() *FakeExec {
	return newFakeExec(true)
}

// newFakeExec returns a new FakeExec with a default LookPathFunc
func newFakeExec(looseCompare bool) *FakeExec {
	return &FakeExec{
		looseCompare:     looseCompare,
		expectedCommands: make([]*ExpectedCmd, 0),
	}
}

const fakeBinPrefix string = "/fake-bin/"

// LookPath is for finding the path of a file
func (f *FakeExec) LookPath(file string) (string, error) {
	return fakeBinPrefix + file, nil
}

// CommandContext wraps arguments into exec.Cmd
func (f *FakeExec) CommandContext(ctx context.Context, cmd string, args ...string) kexec.Cmd {
	return f.Command(cmd, args...)
}

func (f *FakeExec) ErrorDesc() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.executedCommands) == len(f.expectedCommands) {
		return ""
	}
	return f.internalErrorDesc()
}

func (f *FakeExec) internalErrorDesc() string {
	desc := "Executed commands do not match expected commands!\n"
	if f.receivedUnexpected {
		desc += fmt.Sprintf("Executed unexpected command %s\n\n", f.unexpectedCommand)
	}

	if f.looseCompare {
		// For loose compare, mark expected commands that were not
		// executed with a !
		for i, exp := range f.expectedCommands {
			called := " "
			if !exp.called {
				called = "!"
			}
			desc += fmt.Sprintf("[%02d] %s %s\n", i, called, exp.Cmd)
		}
	} else {
		// For strict compare:
		// 1) show all expected commands that were executed
		// 2) mark executed commands that were not matched with +
		// 3) mark expected commands that were not matched with -
		max := len(f.expectedCommands)
		min := max
		executedLen := len(f.executedCommands)
		if max < executedLen {
			max = executedLen
		}
		if min > executedLen {
			min = executedLen
		}
		for i := 0; i < max; i++ {
			if i < min && f.expectedCommands[i].compare(f.executedCommands[i]) {
				desc += fmt.Sprintf("[%02d]   %v\n", i, f.expectedCommands[i].Cmd)
				continue
			}
			if i < len(f.executedCommands) {
				desc += fmt.Sprintf("[%02d] + %v\n", i, f.executedCommands[i])
			}
			if i < len(f.expectedCommands) {
				desc += fmt.Sprintf("[%02d] - %v\n", i, f.expectedCommands[i].Cmd)
			}
		}
	}
	return desc
}

// CalledMatchesExpected returns true if the number of commands the code under
// test called matches the number of expected commands in the FakeExec's list
func (f *FakeExec) CalledMatchesExpected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.receivedUnexpected {
		return false
	}
	return len(f.executedCommands) == len(f.expectedCommands)
}

// ExpectedCmd contains properties that the testcase expects a called command
// to have as well as the output that the fake command should return
type ExpectedCmd struct {
	// Cmd should be the command-line string of the executable name and all arguments it is expected to be called with
	Cmd string
	// LooseBatchCompare should be set if the command-line string is a batched
	// ovn-nbctl where the ordering does not matter
	LooseBatchCompare bool
	// Output is any stdout output which Cmd should produce
	Output string
	// Stderr is any stderr output which Cmd should produce
	Stderr string
	// Err is any error that should be returned for the invocation of Cmd
	Err error
	// Action is run when the fake command is "run"
	Action func() error
	// called is set to true when the command is called
	called bool
}

// compare compares an executed command and an ExpectedCommand, optionally
// breaking batched commands into individual pieces to loosely compare batches
// that have no internal ordering constraints
func (e *ExpectedCmd) compare(executed string) bool {
	if !e.LooseBatchCompare {
		return executed == e.Cmd
	}

	// Order of the batch doesn't matter; break apart the commands and
	// make sure all executed commands are present in the candidate
	eSplit := strings.SplitN(executed, " ", 2)
	cSplit := strings.SplitN(e.Cmd, " ", 2)
	if eSplit[0] != cSplit[0] {
		return false
	}
	if len(eSplit) != len(cSplit) {
		return false
	}
	if len(eSplit) < 2 {
		// Already compared [0]
		return true
	}

	// Check for --timeout before we compare the actual args
	eAllArgs := eSplit[1]
	cAllArgs := cSplit[1]
	if strings.HasPrefix(eSplit[1], "--timeout=") {
		etSplit := strings.SplitN(eSplit[1], " ", 2)
		ctSplit := strings.SplitN(cSplit[1], " ", 2)
		if etSplit[0] != ctSplit[0] {
			return false
		}
		eAllArgs = etSplit[1]
		cAllArgs = ctSplit[1]
	}

	// Now compare each element of the command batch
	eSplitArgs := strings.Split(eAllArgs, " -- ")
	cSplitArgs := strings.Split(cAllArgs, " -- ")
	if len(eSplitArgs) != len(cSplitArgs) {
		return false
	}

	for _, eArg := range eSplitArgs {
		found := false
		for _, cArg := range cSplitArgs {
			if cArg == eArg {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

func getExecutedCommandline(cmd string, args ...string) string {
	return cmd + " " + strings.Join(args, " ")
}

func (f *FakeExec) Command(cmd string, args ...string) kexec.Cmd {
	dummyCommand := &fakeexec.FakeCmd{
		Argv: []string{},
		CombinedOutputScript: []fakeexec.FakeAction{
			func() ([]byte, []byte, error) {
				return []byte{}, []byte{}, fmt.Errorf("fake exec error")
			},
		},
		RunScript: []fakeexec.FakeAction{
			func() ([]byte, []byte, error) {
				return []byte{}, []byte{}, fmt.Errorf("fake exec error")
			},
		},
	}
	if f.receivedUnexpected {
		// If receivedUnexpected we return a fake error. It doesn't matter
		// because we'll check the first error anyway
		return dummyCommand
	}

	executed := getExecutedCommandline(cmd, args...)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executedCommands = append(f.executedCommands, executed)

	var expected *ExpectedCmd
	for _, candidate := range f.expectedCommands {
		if !candidate.called {
			if candidate.compare(executed) {
				expected = candidate
				expected.called = true
				break
			}
			if !f.looseCompare {
				// Fail if the first unused expected command doesn't
				// match the one that is being executed
				if !candidate.compare(executed) {
					f.receivedUnexpected = true
					f.unexpectedCommand = executed
					klog.Warning(f.internalErrorDesc())
					debug.PrintStack()
					break
				}
			}
		}
	}
	// Fail if the command being executed could not be found in the
	// expected command list, or if the expected command list has been
	// completely used and we are executing more commands
	if expected == nil {
		f.receivedUnexpected = true
		f.unexpectedCommand = executed
		klog.Warningf("Unexpected command: %s\n\n%s", executed, f.internalErrorDesc())
		debug.PrintStack()
		// there's not point in returning the fake cmd as expected is nil
		return dummyCommand
	}

	return &fakeexec.FakeCmd{
		Argv: strings.Split(expected.Cmd, " ")[1:],
		CombinedOutputScript: []fakeexec.FakeAction{
			func() ([]byte, []byte, error) {
				return []byte(expected.Output), []byte(expected.Stderr), expected.Err
			},
		},
		RunScript: []fakeexec.FakeAction{
			func() ([]byte, []byte, error) {
				if expected.Action != nil {
					err := expected.Action()
					if err != nil {
						klog.Fatalf("Unexpected error running command %q: %v", expected.Cmd, err)
					}
				}
				return []byte(expected.Output), []byte(expected.Stderr), expected.Err
			},
		},
	}
}

// AddFakeCmd takes the ExpectedCmd and appends its runner function to
// a fake command action list of the FakeExec
func (f *FakeExec) AddFakeCmd(expected *ExpectedCmd) {
	expected.Cmd = fakeBinPrefix + expected.Cmd
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expectedCommands = append(f.expectedCommands, expected)
}

// AddFakeCmdsNoOutputNoError appends a list of commands to the expected
// command set. The command cannot return any output or error.
func (f *FakeExec) AddFakeCmdsNoOutputNoError(commands []string) {
	for _, cmd := range commands {
		f.AddFakeCmd(&ExpectedCmd{Cmd: cmd})
	}
}
