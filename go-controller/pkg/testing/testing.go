package testing

import (
	"context"
	"strings"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	"github.com/sirupsen/logrus"
	kexec "k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"
)

func init() {
	// Gomega's default string diff behavior makes it impossible to figure
	// out what fake command is failing, so turn it off
	format.TruncatedDiff = false
}

// KCmd is a callback spec returning a k8s exec command
type KCmd func(cmd string, args ...string) kexec.Cmd

type pool struct {
	cmd  KCmd
	used bool
}

// FakeExec is a convenience struct that wraps testing.FakeExec
type FakeExec struct {
	// Activate this for a loose comparison of executed OVN commands.
	// We will in such a case ignore order when comparing all executed commands during the run of a test case.
	// This is important when defining test cases with multiple resources (or multiple resource watchers) of
	// the same type and not being able to rely on a deterministic order of incomming watch events.
	fakeexec.FakeExec
	looseCompare     bool
	commandPool      map[string]*pool
	expectedCommands []string
	executedCommands []string
}

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
		looseCompare: looseCompare,
		commandPool:  make(map[string]*pool),
		FakeExec: fakeexec.FakeExec{
			LookPathFunc: func(file string) (string, error) {
				return "/fake-bin/" + file, nil
			},
		},
	}
}

// LookPath is for finding the path of a file
func (f *FakeExec) LookPath(file string) (string, error) {
	return f.LookPathFunc(file)
}

// CommandContext wraps arguments into exec.Cmd
func (f *FakeExec) CommandContext(ctx context.Context, cmd string, args ...string) kexec.Cmd {
	return f.Command(cmd, args...)
}

func (f *FakeExec) PrintAllCmds() {
	for i := range f.expectedCommands {
		v := " "
		if f.looseCompare {
			c, ok := f.commandPool[f.expectedCommands[i]]
			if ok && c.used {
				v = "*"
			} else if !ok {
				v = "-"
			}
		}
		logrus.Infof("Expected commands were %v: %s %v", i, v, f.expectedCommands[i])
	}
	for i := range f.executedCommands {
		logrus.Infof("Executed commands were %v: %v", i, f.executedCommands[i])
	}
}

// CalledMatchesExpected returns true if the number of commands the code under
// test called matches the number of expected commands in the FakeExec's list
func (f *FakeExec) CalledMatchesExpected() bool {
	if len(f.executedCommands) != len(f.expectedCommands) {
		logrus.Infof("Command calls do not match!")
		f.PrintAllCmds()
		return false
	}
	if f.looseCompare {
		for k, cmd := range f.commandPool {
			if !cmd.used {
				logrus.Infof("Expected command unused: %s", k)
				return false
			}
		}
	}
	return true
}

// ExpectedCmd contains properties that the testcase expects a called command
// to have as well as the output that the fake command should return
type ExpectedCmd struct {
	// Cmd should be the command-line string of the executable name and all arguments it is expected to be called with
	Cmd string
	// Output is any stdout output which Cmd should produce
	Output string
	// Stderr is any stderr output which Cmd should produce
	Stderr string
	// Err is any error that should be returned for the invocation of Cmd
	Err error
	// Action is run when the fake command is "run"
	Action func() error
}

func getExecutedCommandline(cmd string, args ...string) string {
	return cmd + " " + strings.Join(args, " ")
}

func getExpectedCommandline(cmd string) (string, []string) {
	parts := strings.Split(cmd, " ")
	expectedCommandline := "/fake-bin/" + strings.Join(parts, " ")
	return expectedCommandline, parts
}

func (f *FakeExec) Command(cmd string, args ...string) kexec.Cmd {
	f.executedCommands = append(f.executedCommands, getExecutedCommandline(cmd, args...))
	if f.looseCompare {
		executedCommandline := getExecutedCommandline(cmd, args...)
		if c, ok := f.commandPool[executedCommandline]; ok {
			c.used = true
			return c.cmd(cmd, args...)
		}
		f.PrintAllCmds()
		gomega.Expect(executedCommandline).To(gomega.Equal("Did you forget to add this command?"), "Called command is not in the pool of expected fake commands")
	}
	return f.FakeExec.Command(cmd, args...)
}

// AddFakeCmd takes the ExpectedCmd and appends its runner function to
// a fake command action list of the FakeExec
func (f *FakeExec) AddFakeCmd(expected *ExpectedCmd) {
	kCmd := func(cmd string, args ...string) kexec.Cmd {
		expectedCommandline, parts := getExpectedCommandline(expected.Cmd)
		executedCommandline := getExecutedCommandline(cmd, args...)

		gomega.Expect(len(parts)).To(gomega.BeNumerically(">=", 2))

		// Expect the incoming 'args' to equal the fake/expected command 'parts'
		gomega.Expect(executedCommandline).To(gomega.Equal(expectedCommandline), "Called command doesn't match expected fake command")

		return &fakeexec.FakeCmd{
			Argv: parts[1:],
			CombinedOutputScript: []fakeexec.FakeCombinedOutputAction{
				func() ([]byte, error) {
					return []byte(expected.Output), expected.Err
				},
			},
			RunScript: []fakeexec.FakeRunAction{
				func() ([]byte, []byte, error) {
					if expected.Action != nil {
						err := expected.Action()
						gomega.Expect(err).NotTo(gomega.HaveOccurred())
					}
					return []byte(expected.Output), []byte(expected.Stderr), expected.Err
				},
			},
		}
	}
	expectedCommandline, _ := getExpectedCommandline(expected.Cmd)
	f.expectedCommands = append(f.expectedCommands, expectedCommandline)
	if f.looseCompare {
		f.commandPool[expectedCommandline] = &pool{kCmd, false}
	} else {
		f.CommandScript = append(f.CommandScript, kCmd)
	}
}

// AddFakeCmdsNoOutputNoError appends a list of commands to the expected
// command set. The command cannot return any output or error.
func (f *FakeExec) AddFakeCmdsNoOutputNoError(commands []string) {
	for _, cmd := range commands {
		f.AddFakeCmd(&ExpectedCmd{Cmd: cmd})
	}
}
