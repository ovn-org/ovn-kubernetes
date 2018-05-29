package testing

import (
	"strings"

	kexec "k8s.io/utils/exec"
	fakeexec "k8s.io/utils/exec/testing"

	"github.com/onsi/gomega"
)

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
}

// AddFakeCmd takes the ExpectedCmd and appends its runner function to
// a fake command action list
func AddFakeCmd(fakeCmds []fakeexec.FakeCommandAction, expected *ExpectedCmd) []fakeexec.FakeCommandAction {
	return append(fakeCmds, func(cmd string, args ...string) kexec.Cmd {
		parts := strings.Split(expected.Cmd, " ")
		gomega.Expect(cmd).To(gomega.Equal("/fake-bin/" + parts[0]))
		gomega.Expect(strings.Join(args, " ")).To(gomega.Equal(strings.Join(parts[1:], " ")))
		return &fakeexec.FakeCmd{
			Argv: parts[1:],
			CombinedOutputScript: []fakeexec.FakeCombinedOutputAction{
				func() ([]byte, error) {
					return []byte(expected.Output), expected.Err
				},
			},
			RunScript: []fakeexec.FakeRunAction{
				func() ([]byte, []byte, error) {
					return []byte(expected.Output), []byte(expected.Stderr), expected.Err
				},
			},
		}
	})
}
