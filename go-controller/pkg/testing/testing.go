package testing

import (
	"strings"

	kexec "k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
)

func init() {
	// Gomega's default string diff behavior makes it impossible to figure
	// out what fake command is failing, so turn it off
	format.TruncatedDiff = false
}

// FakeExec is a convenience struct that wraps testing.FakeExec
type FakeExec struct {
	fakeexec.FakeExec
}

// NewFakeExec returns a new FakeExec with a default LookPathFunc
func NewFakeExec() *FakeExec {
	return &FakeExec{
		fakeexec.FakeExec{
			LookPathFunc: func(file string) (string, error) {
				return "/fake-bin/" + file, nil
			},
		},
	}
}

// CalledMatchesExpected returns true if the number of commands the code under
// test called matches the number of expected commands in the FakeExec's list
func (f *FakeExec) CalledMatchesExpected() bool {
	return f.CommandCalls == len(f.CommandScript)
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

// AddFakeCmd takes the ExpectedCmd and appends its runner function to
// a fake command action list of the FakeExec
func (f *FakeExec) AddFakeCmd(expected *ExpectedCmd) {
	f.CommandScript = append(f.CommandScript, func(cmd string, args ...string) kexec.Cmd {
		parts := strings.Split(expected.Cmd, " ")
		gomega.Expect(len(parts)).To(gomega.BeNumerically(">=", 2))

		executedCommandline := cmd + " " + strings.Join(args, " ")
		expectedCommandline := "/fake-bin/" + strings.Join(parts, " ")
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
	})
}

// AddFakeCmdsNoOutputNoError appends a list of commands to the expected
// command set. The command cannot return any output or error.
func (f *FakeExec) AddFakeCmdsNoOutputNoError(commands []string) {
	for _, cmd := range commands {
		f.AddFakeCmd(&ExpectedCmd{Cmd: cmd})
	}
}
