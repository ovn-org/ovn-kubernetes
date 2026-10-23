// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	mock_k8s_io_utils_exec "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/utils/exec"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/mocks"
)

func TestRunningPlatform(t *testing.T) {
	// Below is defined in ovs.go file
	AppFs = afero.NewMemMapFs()
	if err := AppFs.MkdirAll("/etc", 0o755); err != nil {
		t.Fatalf("failed to AppFs.MkdirAll: %v", err)
	}
	tests := []struct {
		desc            string
		fileContent     []byte
		filePermissions os.FileMode
		expOut          string
		expErr          error
	}{
		{
			desc:   "ReadFile returns error",
			expErr: fmt.Errorf("failed to parse file"),
		},
		{
			desc:            "failed to find platform name",
			expErr:          fmt.Errorf("failed to find the platform name"),
			fileContent:     []byte("NAME="),
			filePermissions: 0o755,
		},
		{
			desc:            "platform name returned is RHEL",
			expOut:          "RHEL",
			fileContent:     []byte("NAME=\"CentOS Linux\""),
			filePermissions: 0o755,
		},
		{
			desc:            "platform name returned is Ubuntu",
			expOut:          "Ubuntu",
			fileContent:     []byte("NAME=\"Debian\""),
			filePermissions: 0o755,
		},
		{
			desc:            "platform name returned is Photon",
			expOut:          "Photon",
			fileContent:     []byte("NAME=\"VMware\""),
			filePermissions: 0o755,
		},
		{
			desc:            "unknown platform",
			expErr:          fmt.Errorf("unknown platform"),
			fileContent:     []byte("NAME=\"blah\""),
			filePermissions: 0o755,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.fileContent != nil && tc.filePermissions != 0 {
				err := afero.WriteFile(AppFs, "/etc/os-release", tc.fileContent, tc.filePermissions)
				if err != nil {
					t.Fatalf("failed to afero.WriteFile: %v", err)
				}
				defer func() {
					if err = AppFs.Remove("/etc/os-release"); err != nil {
						t.Fatalf("failed to AppFs.Remove: %v", err)
					}
				}()
			}
			res, err := runningPlatform()
			t.Log(res, err)
			if tc.expErr != nil {
				assert.Contains(t, err.Error(), tc.expErr.Error())
			} else {
				assert.Equal(t, tc.expOut, res)
			}
		})
	}
}

func TestRunOVNretry(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// Variables below are defined in ovs.go
	ovnCmdRetryCount = 0
	ovnCmdRetryInterval = 1 * time.Millisecond
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	// Used for "test path when PID changes" test case
	extraArgsFuncAlreadyCalled := false

	tests := []struct {
		desc                    string
		retryCountOverride      int
		inpCmdPath              string
		inpEnvVars              []string
		inpExtraArgsFunc        func() ([]string, error)
		errMatch                error
		onRetArgsExecUtilsIface []*ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "test path when runWithEnvVars returns no error",
			inpCmdPath:              runner.ovnctlPath,
			inpEnvVars:              []string{},
			onRetArgsExecUtilsIface: []*ovntest.TestifyMockHelper{{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{nil, nil, nil}}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "test path when runWithEnvVars returns  \"Connection refused\" error",
			inpCmdPath:              runner.ovnctlPath,
			inpEnvVars:              []string{},
			errMatch:                fmt.Errorf("connection refused"),
			onRetArgsExecUtilsIface: []*ovntest.TestifyMockHelper{{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{nil, bytes.NewBuffer([]byte("Connection refused")), fmt.Errorf("connection refused")}}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "test path when runWithEnvVars returns an error OTHER THAN \"Connection refused\" ",
			inpCmdPath:              runner.ovnctlPath,
			inpEnvVars:              []string{},
			errMatch:                fmt.Errorf("OVN command"),
			onRetArgsExecUtilsIface: []*ovntest.TestifyMockHelper{{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("mock error")}}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:               "test path when PID changes",
			retryCountOverride: 1,
			inpCmdPath:         runner.ovnctlPath,
			inpEnvVars:         []string{},
			inpExtraArgsFunc: func() ([]string, error) {
				if !extraArgsFuncAlreadyCalled {
					extraArgsFuncAlreadyCalled = true
					return []string{"-t", "ovn-northd.1.ctl"}, nil
				}
				return []string{"-t", "ovn-northd.2.ctl"}, nil
			},
			onRetArgsExecUtilsIface: []*ovntest.TestifyMockHelper{
				{OnCallMethodName: "RunCmd", OnCallMethodArgs: []interface{}{mock.Anything, runner.ovnctlPath, []string{}, "-t", "ovn-northd.1.ctl"}, RetArgList: []interface{}{nil, bytes.NewBuffer([]byte("Connection refused")), fmt.Errorf("connection refused")}},
				{OnCallMethodName: "RunCmd", OnCallMethodArgs: []interface{}{mock.Anything, runner.ovnctlPath, []string{}, "-t", "ovn-northd.2.ctl"}, RetArgList: []interface{}{nil, nil, nil}},
			},
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}, CallTimes: 2},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.onRetArgsExecUtilsIface {
				ovntest.ProcessMockFn(&mockExecRunner.Mock, *item)
			}
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovnCmdRetryCount = tc.retryCountOverride

			_, _, e := runOVNretry(tc.inpCmdPath, tc.inpEnvVars, tc.inpExtraArgsFunc)

			if tc.errMatch != nil {
				assert.Contains(t, e.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestGetNbctlArgsAndEnv(t *testing.T) {
	cmdArgs, envVars := getNbctlArgsAndEnv(15)
	assert.Equal(t, []string{"--timeout=15"}, cmdArgs)
	assert.Equal(t, []string{}, envVars)
}

func TestRunOVNNorthAppCtl(t *testing.T) {
	// Below is defined in ovs.go file
	AppFs = afero.NewMemMapFs()
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	// note runner.ovndir is defined in ovs.go file and so is ovnRunDir var with an initial value
	runner.ovnRunDir = ovnRunDir

	tests := []struct {
		desc                    string
		inpVarArgs              string
		errMatch                error
		dirFileMocks            []ovntest.AferoDirMockHelper
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:     "test path when ReadFile returns error",
			errMatch: fmt.Errorf("failed to run the command since failed to get ovn-northd's pid:"),
		},
		{
			desc: "test path when runOVNretry succeeds",
			dirFileMocks: []ovntest.AferoDirMockHelper{
				{
					DirName:     "/var/run/ovn/",
					Permissions: 0o755,
					Files: []ovntest.AferoFileMockHelper{
						{FileName: "/var/run/ovn/ovn-northd.pid", Permissions: 0o755, Content: []byte("pid")},
					},
				},
			},
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsExecUtilsIface != nil {
				ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			}
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			}

			if len(tc.dirFileMocks) > 0 {
				for _, item := range tc.dirFileMocks {
					if err := AppFs.MkdirAll(item.DirName, item.Permissions); err != nil {
						t.Fatalf("failed to AppFs.MkdirAll: %v", err)
					}
					defer func() {
						if err := AppFs.Remove(item.DirName); err != nil {
							t.Fatalf("failed to AppFs.Remove: %v", err)
						}
					}()
					if len(item.Files) != 0 {
						for _, f := range item.Files {
							err := afero.WriteFile(AppFs, f.FileName, f.Content, f.Permissions)
							if err != nil {
								t.Fatalf("failed to afero.WriteFile: %v", err)
							}
						}
					}
				}
			}
			_, _, err := RunOVNNorthAppCtl()
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNControllerAppCtl(t *testing.T) {
	// Below is defined in ovs.go file
	AppFs = afero.NewMemMapFs()
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	// note runner.ovndir is defined in ovs.go file and so is ovnRunDir var with an initial value
	runner.ovnRunDir = ovnRunDir

	tests := []struct {
		desc                    string
		inpVarArgs              string
		errMatch                error
		dirFileMocks            []ovntest.AferoDirMockHelper
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:     "test path when ReadFile returns error",
			errMatch: fmt.Errorf("failed to get ovn-controller pid"),
		},
		{
			desc: "test path when runOVNretry succeeds",
			dirFileMocks: []ovntest.AferoDirMockHelper{
				{
					DirName:     "/var/run/ovn/",
					Permissions: 0o755,
					Files: []ovntest.AferoFileMockHelper{
						{FileName: "/var/run/ovn/ovn-controller.pid", Permissions: 0o755, Content: []byte("pid")},
					},
				},
			},
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsExecUtilsIface != nil {
				ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			}
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			}

			if len(tc.dirFileMocks) > 0 {
				for _, item := range tc.dirFileMocks {
					if err := AppFs.MkdirAll(item.DirName, item.Permissions); err != nil {
						t.Fatalf("failed to AppFs.MkdirAlle: %v", err)
					}
					defer func() {
						if err := AppFs.Remove(item.DirName); err != nil {
							t.Fatalf("failed to AppFs.Remove: %v", err)
						}
					}()
					if len(item.Files) != 0 {
						for _, f := range item.Files {
							err := afero.WriteFile(AppFs, f.FileName, f.Content, f.Permissions)
							if err != nil {
								t.Fatalf("failed to afero.WriteFile: %v", err)
							}
						}
					}
				}
			}
			_, _, err := RunOVNControllerAppCtl()
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOvsVswitchdAppCtl(t *testing.T) {
	// Below is defined in ovs.go file
	AppFs = afero.NewMemMapFs()
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc                    string
		inpVarArgs              string
		errMatch                error
		dirFileMocks            []ovntest.AferoDirMockHelper
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:     "test path when ReadFile returns error",
			errMatch: fmt.Errorf("failed to get ovs-vswitch pid"),
		},
		{
			desc: "test path when runOVNretry succeeds",
			dirFileMocks: []ovntest.AferoDirMockHelper{
				{
					DirName:     "/var/run/openvswitch/",
					Permissions: 0o755,
					Files: []ovntest.AferoFileMockHelper{
						{FileName: "/var/run/openvswitch/ovs-vswitchd.pid", Permissions: 0o755, Content: []byte("pid")},
					},
				},
			},
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgsExecUtilsIface != nil {
				ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			}
			if tc.onRetArgsKexecIface != nil {
				ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			}

			if len(tc.dirFileMocks) > 0 {
				for _, item := range tc.dirFileMocks {
					if err := AppFs.MkdirAll(item.DirName, item.Permissions); err != nil {
						t.Fatalf("failed to AppFs.MkdirAlle: %v", err)
					}
					defer func() {
						if err := AppFs.Remove(item.DirName); err != nil {
							t.Fatalf("failed to AppFs.Remove: %v", err)
						}
					}()
					if len(item.Files) != 0 {
						for _, f := range item.Files {
							err := afero.WriteFile(AppFs, f.FileName, f.Content, f.Permissions)
							if err != nil {
								t.Fatalf("failed to afero.WriteFile: %v", err)
							}
						}
					}
				}
			}
			_, _, err := RunOvsVswitchdAppCtl()
			t.Log(err)
			if tc.errMatch != nil {
				assert.Contains(t, err.Error(), tc.errMatch.Error())
			} else {
				require.NoError(t, err)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestDefaultExecRunner_RunCmd(t *testing.T) {
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// tests in other files in the package would set runCmdExecRunner to mocks.ExecRunner,
	// for this test we want to ensure the non-mock instance is used
	RunCmdExecRunner = &defaultExecRunner{}

	tests := []struct {
		desc             string
		expectedErr      error
		cmd              kexec.Cmd
		cmdPath          string
		cmdArg           string
		envVars          []string
		onRetArgsCmdList []ovntest.TestifyMockHelper
	}{
		{
			desc:        "negative: set cmd parameter to be nil",
			expectedErr: fmt.Errorf("cmd object cannot be nil"),
			cmd:         nil,
		},
		{
			desc:        "cmd.Run returns error test",
			expectedErr: fmt.Errorf("mock error"),
			cmd:         mockCmd,
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{fmt.Errorf("mock error")}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.cmd != nil {
				ovntest.ProcessMockFnList(&tc.cmd.(*mock_k8s_io_utils_exec.Cmd).Mock, tc.onRetArgsCmdList)
			}
			_, _, e := RunCmdExecRunner.RunCmd(tc.cmd, tc.cmdPath, tc.envVars, tc.cmdArg)

			assert.Equal(t, tc.expectedErr, e)
			mockCmd.AssertExpectations(t)
		})
	}
}

func TestSetExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc         string
		expectedErr  error
		onRetArgs    *ovntest.TestifyMockHelper
		setRunnerNil bool
	}{
		{
			desc:         "positive, test when 'runner' is nil",
			expectedErr:  nil,
			onRetArgs:    &ovntest.TestifyMockHelper{OnCallMethodName: "LookPath", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ip", nil}, CallTimes: 9},
			setRunnerNil: true,
		},
		{
			desc:         "positive, test when 'runner' is not nil",
			expectedErr:  nil,
			onRetArgs:    &ovntest.TestifyMockHelper{OnCallMethodName: "LookPath", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", nil}, CallTimes: 9},
			setRunnerNil: false,
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgs)
			if tc.setRunnerNil == false {
				// note runner is defined in ovs.go file
				runner = &execHelper{exec: mockKexecIface}
			}
			e := SetExec(mockKexecIface)
			assert.Equal(t, tc.expectedErr, e)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSetExecWithoutOVS(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		onRetArgs   *ovntest.TestifyMockHelper
	}{
		{
			desc:        "positive, ip path found",
			expectedErr: nil,
			onRetArgs:   &ovntest.TestifyMockHelper{OnCallMethodName: "LookPath", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ip", nil}},
		},
		{
			desc:        "negative, ip path not found",
			expectedErr: fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`),
			onRetArgs:   &ovntest.TestifyMockHelper{OnCallMethodName: "LookPath", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", fmt.Errorf(`exec: \"ip:\" executable file not found in $PATH`)}},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgs)

			e := SetExecWithoutOVS(mockKexecIface)
			assert.Equal(t, tc.expectedErr, e)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestSetSpecificExec(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	tests := []struct {
		desc        string
		expectedErr error
		fnArg       string
		onRetArgs   *ovntest.TestifyMockHelper
	}{
		{
			desc:        "positive: ovs-vsctl path found",
			expectedErr: nil,
			fnArg:       "ovs-vsctl",
			onRetArgs:   &ovntest.TestifyMockHelper{OnCallMethodName: "LookPath", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"ovs-vsctl", nil}},
		},
		{
			desc:        "negative: ovs-vsctl path not found",
			expectedErr: fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`),
			fnArg:       "ovs-vsctl",
			onRetArgs:   &ovntest.TestifyMockHelper{OnCallMethodName: "LookPath", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{"", fmt.Errorf(`exec: \"ovs-vsctl:\" executable file not found in $PATH`)}},
		},
		{
			desc:        "negative: unknown command",
			expectedErr: fmt.Errorf(`unknown command: "ovs-appctl"`),
			fnArg:       "ovs-appctl",
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			if tc.onRetArgs != nil {
				ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgs)
			}

			e := SetSpecificExec(mockKexecIface, tc.fnArg)
			assert.Equal(t, tc.expectedErr, e)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunCmd(t *testing.T) {
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)

	tests := []struct {
		desc             string
		expectedErr      error
		cmdPath          string
		cmdArg           string
		onRetArgsCmdList []ovntest.TestifyMockHelper
	}{
		{
			desc:        "positive: run `ip addr` command",
			expectedErr: nil,
			cmdPath:     "ip",
			cmdArg:      "a",
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
		{
			desc:        "negative: run `ip addr` command",
			expectedErr: fmt.Errorf("executable file not found in $PATH"),
			cmdPath:     "ips",
			cmdArg:      "addr",
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)

			_, _, e := runCmd(mockCmd, tc.cmdPath, tc.cmdArg)

			assert.Equal(t, tc.expectedErr, e)
			mockCmd.AssertExpectations(t)
		})
	}
}

func TestRun(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		cmdPath          string
		cmdArg           string
		onRetArgsIface   *ovntest.TestifyMockHelper
		onRetArgsCmdList []ovntest.TestifyMockHelper
	}{
		{
			desc:           "negative: run `ip addr` command",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			cmdPath:        "ips",
			cmdArg:         "addr",
			onRetArgsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
		{
			desc:           "positive: run `ip addr`",
			expectedErr:    nil,
			cmdPath:        "ip",
			cmdArg:         "a",
			onRetArgsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsIface)

			/*for _, item := range tc.onRetArgsCmdList {
				cmdCall := mockCmd.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					cmdCall.Arguments = append(cmdCall.Arguments, mock.AnythingOfType(arg))
				}

				for _, e := range item.RetArgList {
					cmdCall.ReturnArguments = append(cmdCall.ReturnArguments, e)
				}
				cmdCall.Once()
			}

			ifaceCall := mockKexecIface.On(tc.onRetArgsIface.OnCallMethodName, mock.Anything)
			for _, arg := range tc.onRetArgsIface.OnCallMethodArgType {
				ifaceCall.Arguments = append(ifaceCall.Arguments, mock.AnythingOfType(arg))
			}
			for _, item := range tc.onRetArgsIface.RetArgList {
				ifaceCall.ReturnArguments = append(ifaceCall.ReturnArguments, item)
			}
			ifaceCall.Once()*/

			_, _, e := run(tc.cmdPath, tc.cmdArg)

			assert.Equal(t, tc.expectedErr, e)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunWithEnvVars(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}

	tests := []struct {
		desc             string
		expectedErr      error
		cmdPath          string
		cmdArg           string
		envArgs          []string
		onRetArgsIface   *ovntest.TestifyMockHelper
		onRetArgsCmdList []ovntest.TestifyMockHelper
	}{
		{
			desc:           "positive: run `ip addr` command with empty envVars",
			expectedErr:    nil,
			cmdPath:        "ip",
			cmdArg:         "a",
			envArgs:        []string{},
			onRetArgsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
		{
			desc:           "positive: run `ip addr` command",
			expectedErr:    nil,
			cmdPath:        "ip",
			cmdArg:         "a",
			envArgs:        []string{},
			onRetArgsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{nil}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
		{
			desc:           "negative: run `ip addr` command",
			expectedErr:    fmt.Errorf("executable file not found in $PATH"),
			cmdPath:        "ips",
			cmdArg:         "addr",
			envArgs:        nil,
			onRetArgsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList: []ovntest.TestifyMockHelper{
				{OnCallMethodName: "Run", OnCallMethodArgType: []string{}, RetArgList: []interface{}{fmt.Errorf("executable file not found in $PATH")}},
				{OnCallMethodName: "SetStdout", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
				{OnCallMethodName: "SetStderr", OnCallMethodArgType: []string{"*bytes.Buffer"}, RetArgList: nil},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFnList(&mockCmd.Mock, tc.onRetArgsCmdList)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsIface)

			_, _, e := runWithEnvVars(tc.cmdPath, tc.envArgs, tc.cmdArg)

			assert.Equal(t, tc.expectedErr, e)
			mockCmd.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSOfctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovs-ofctl` command",
			expectedErr:             fmt.Errorf("executable file not found in $PATH"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-ofctl` ",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVSOfctl()

			assert.Equal(t, tc.expectedErr, e)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestGetOpenFlowPorts(t *testing.T) {
	// ovs-ofctl show breth0 mock output
	ofctlOutput := `
OFPT_FEATURES_REPLY (xid=0x2): dpid:00000242ac120002
n_tables:254, n_buffers:0
capabilities: FLOW_STATS TABLE_STATS PORT_STATS QUEUE_STATS ARP_MATCH_IP
actions: output enqueue set_vlan_vid set_vlan_pcp strip_vlan mod_dl_src mod_dl_dst mod_nw_src mod_nw_dst mod_nw_tos mod_tp_src mod_tp_dst
 1(eth0): addr:02:42:ac:12:00:02
     config:     0
     state:      0
     current:    10GB-FD COPPER
     speed: 10000 Mbps now, 0 Mbps max
 2(patch-breth0_ov): addr:8e:8d:f4:cd:4f:76
     config:     0
     state:      0
     speed: 0 Mbps now, 0 Mbps max
 LOCAL(breth0): addr:02:42:ac:12:00:02
     config:     0
     state:      0
     speed: 0 Mbps now, 0 Mbps max
OFPT_GET_CONFIG_REPLY (xid=0x4): frags=normal miss_send_len=0
`
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		expectedOut             []string
		portNumbers             bool
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:        "Get port numbers",
			expectedErr: nil,
			expectedOut: []string{"1", "2", "LOCAL"},
			portNumbers: false,
			// match arguments [ "mocks.Cmd", "string", "[]string{}", "show", "breth0" ] -> "string", "string", "string[]", "string", "string"
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{
				OnCallMethodName:    "RunCmd",
				OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"},
				// ovs-ofctl show breth0 will return stdout=ofctlOutput, stderr="", err=nil
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ofctlOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// match arguments [ "", "show", "breth0" ] -> "string", "string", "string"
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{
				OnCallMethodName:    "Command",
				OnCallMethodArgType: []string{"string", "string", "string"},
				RetArgList:          []interface{}{mockCmd},
			},
		},
		{
			desc:        "Get port names",
			expectedErr: nil,
			expectedOut: []string{"eth0", "patch-breth0_ov", "breth0"},
			portNumbers: true,
			// match arguments [ "mocks.Cmd", "string", "[]string{}", "show", "breth0" ] -> "string", "string", "string[]", "string", "string"
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{
				OnCallMethodName:    "RunCmd",
				OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"},
				// ovs-ofctl show breth0 will return stdout=ofctlOutput, stderr="", err=nil
				RetArgList: []interface{}{bytes.NewBuffer([]byte(ofctlOutput)), bytes.NewBuffer([]byte("")), nil},
			},
			// match arguments [ "", "show", "breth0" ] -> "string", "string", "string"
			onRetArgsKexecIface: &ovntest.TestifyMockHelper{
				OnCallMethodName:    "Command",
				OnCallMethodArgType: []string{"string", "string", "string"},
				RetArgList:          []interface{}{mockCmd},
			},
		},
	}

	// loop through all tests and make sure that there is no error and that the port list is correct
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			ports, err := GetOpenFlowPorts("breth0", tc.portNumbers)

			// make sure that there's no error
			assert.Equal(t, tc.expectedErr, err)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)

			// make sure that we get the correct list of ports
			assert.ElementsMatch(t, ports, tc.expectedOut)
		})
	}
}

func TestRunOVSVsctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovs-vsctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-vsctl command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-vsctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-vsctl` ",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVSVsctl()

			assert.Equal(t, tc.expectedErr, e)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSAppctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovs-appctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovs-appctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-appctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovs-appctl` command with timeout",
			expectedErr:             nil,
			timeout:                 5,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVSAppctlWithTimeout(tc.timeout)

			assert.Equal(t, tc.expectedErr, e)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNAppctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovn-appctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovn-appctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-appctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-appctl` command with timeout",
			expectedErr:             nil,
			timeout:                 15,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVNAppctlWithTimeout(tc.timeout)

			assert.Equal(t, tc.expectedErr, e)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovn-nbctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovn-nbctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-nbctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-nbctl` command with timeout",
			expectedErr:             nil,
			timeout:                 15,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVNNbctlWithTimeout(tc.timeout)

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNbctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovn-nbctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-nbctl command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-nbctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-nbctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVNNbctl()

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctlWithTimeout(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		timeout                 int
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovn-sbctl` command with timeout",
			expectedErr:             fmt.Errorf("failed to execute ovn-sbctl command"),
			timeout:                 5,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-sbctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-sbctl` command with timeout",
			expectedErr:             nil,
			timeout:                 15,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVNSbctlWithTimeout(tc.timeout)

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSbctl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovn-sbctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-sbctl command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-sbctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-sbctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVNSbctl()

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSDBClient(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovsdb-client` command",
			expectedErr:             fmt.Errorf("failed to execute ovsdb-client command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovsdb-client command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovsdb-client` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVSDBClient()

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVSDBTool(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go.
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovsdb-tool` command",
			expectedErr:             fmt.Errorf("failed to execute ovsdb-tool command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovsdb-tool command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovsdb-tool` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVSDBTool()

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNNBAppCtl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovn-appctl -t nbdbCtlFileName` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-appctl -t nbdbCtlFileName command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-appctl -t nbdbCtlFileName command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-appctl -t nbdbCtlFileName` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVNNBAppCtl()

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunOVNSBAppCtl(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovn-appctl -t sbdbCtlFileName` command",
			expectedErr:             fmt.Errorf("failed to execute ovn-appctl -t sbdbCtlFileName command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovn-appctl -t sbdbCtlFileName command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
		{
			desc:                    "positive: run `ovn-appctl -t sbdbCtlFileName` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunOVNSBAppCtl()

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestRunIP(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockExecRunner := new(mocks.ExecRunner)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "positive: run IP ",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string"}, RetArgList: []interface{}{mockCmd}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)

			_, _, e := RunIP()

			assert.Equal(t, tc.expectedErr, e)
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestAddOFFlowWithSpecificAction(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	mockExecRunner := new(mocks.ExecRunner)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
		onRetArgsCmdList        *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovs-ofctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-ofctl command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-ofctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:        &ovntest.TestifyMockHelper{OnCallMethodName: "SetStdin", OnCallMethodArgType: []string{"*bytes.Buffer"}},
		},
		{
			desc:                    "positive: run `ovs-ofctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), fmt.Errorf("executable file not found in $PATH")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:        &ovntest.TestifyMockHelper{OnCallMethodName: "SetStdin", OnCallMethodArgType: []string{"*bytes.Buffer"}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)

			_, _, e := AddOFFlowWithSpecificAction("somename", "someaction")

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestReplaceOFFlows(t *testing.T) {
	mockKexecIface := new(mock_k8s_io_utils_exec.Interface)
	mockCmd := new(mock_k8s_io_utils_exec.Cmd)
	mockExecRunner := new(mocks.ExecRunner)
	// below is defined in ovs.go
	RunCmdExecRunner = mockExecRunner
	// note runner is defined in ovs.go file
	runner = &execHelper{exec: mockKexecIface}
	tests := []struct {
		desc                    string
		expectedErr             error
		onRetArgsExecUtilsIface *ovntest.TestifyMockHelper
		onRetArgsKexecIface     *ovntest.TestifyMockHelper
		onRetArgsCmdList        *ovntest.TestifyMockHelper
	}{
		{
			desc:                    "negative: run `ovs-ofctl` command",
			expectedErr:             fmt.Errorf("failed to execute ovs-ofctl command"),
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{nil, nil, fmt.Errorf("failed to execute ovs-ofctl command")}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:        &ovntest.TestifyMockHelper{OnCallMethodName: "SetStdin", OnCallMethodArgType: []string{"*util.openFlowStdinReader"}},
		},
		{
			desc:                    "positive: run `ovs-ofctl` command",
			expectedErr:             nil,
			onRetArgsExecUtilsIface: &ovntest.TestifyMockHelper{OnCallMethodName: "RunCmd", OnCallMethodArgType: []string{"*mocks.Cmd", "string", "[]string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{bytes.NewBuffer([]byte("testblah")), bytes.NewBuffer([]byte("")), nil}},
			onRetArgsKexecIface:     &ovntest.TestifyMockHelper{OnCallMethodName: "Command", OnCallMethodArgType: []string{"string", "string", "string", "string", "string", "string", "string"}, RetArgList: []interface{}{mockCmd}},
			onRetArgsCmdList:        &ovntest.TestifyMockHelper{OnCallMethodName: "SetStdin", OnCallMethodArgType: []string{"*util.openFlowStdinReader"}},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			ovntest.ProcessMockFn(&mockExecRunner.Mock, *tc.onRetArgsExecUtilsIface)
			ovntest.ProcessMockFn(&mockKexecIface.Mock, *tc.onRetArgsKexecIface)
			ovntest.ProcessMockFn(&mockCmd.Mock, *tc.onRetArgsCmdList)

			_, _, e := ReplaceOFFlows("somename", []string{})

			if tc.expectedErr != nil {
				require.Error(t, e)
			}
			mockExecRunner.AssertExpectations(t)
			mockKexecIface.AssertExpectations(t)
		})
	}
}

func TestOpenFlowStdinReader(t *testing.T) {
	tests := []struct {
		desc  string
		flows []string
	}{
		{
			desc:  "empty flow list",
			flows: []string{},
		},
		{
			desc:  "single flow",
			flows: []string{"table=0,priority=0,actions=NORMAL"},
		},
		{
			desc:  "multiple flows",
			flows: []string{"a", "b", "c"},
		},
		{
			desc:  "includes empty flow",
			flows: []string{"a", "", "c"},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			r := &openFlowStdinReader{flows: tc.flows}
			out, err := io.ReadAll(r)
			require.NoError(t, err)
			assert.Equal(t, strings.Join(tc.flows, "\n"), string(out))

			buf := make([]byte, 1)
			n, eof := r.Read(buf)
			assert.Equal(t, 0, n)
			assert.Equal(t, io.EOF, eof)
		})
	}
}

func TestPathVariableInitialization(t *testing.T) {
	tests := []struct {
		name     string
		testFunc func(t *testing.T)
	}{
		{
			name: "path variables initialized from config",
			testFunc: func(t *testing.T) {
				t.Helper()
				if err := config.PrepareTestConfig(); err != nil {
					t.Fatalf("failed to PrepareTestConfig: %v", err)
				}

				// Test that path variables are properly initialized from config
				assert.Equal(t, config.OvsPaths.RunDir, ovsRunDir)
				assert.Equal(t, config.OvnNorth.RunDir, ovnRunDir)
				assert.Equal(t, config.OvnSouth.RunDir, ovnRunDir)
				assert.Equal(t, "/etc/ovn/ovnnb_db.db", config.OvnNorth.DbLocation)
				assert.Equal(t, "/etc/ovn/ovnsb_db.db", config.OvnSouth.DbLocation)
			},
		},
		{
			name: "PrepareTestConfig restores saved values",
			testFunc: func(t *testing.T) {
				t.Helper()
				// Save original values
				originalOvsRunDir := ovsRunDir
				originalOvnRunDir := ovnRunDir
				originalOvnNorth := config.OvnNorth
				originalOvnSouth := config.OvnSouth

				// Modify the variables
				ovsRunDir = "/modified/ovs/"
				ovnRunDir = "/modified/ovn/"

				// Call PrepareTestConfig
				PrepareTestConfig()

				// Verify values are restored
				assert.Equal(t, originalOvsRunDir, ovsRunDir)
				assert.Equal(t, originalOvnRunDir, ovnRunDir)
				// Restore config values
				config.OvnNorth = originalOvnNorth
				config.OvnSouth = originalOvnSouth
			},
		},
		{
			name: "default path values are correct",
			testFunc: func(t *testing.T) {
				t.Helper()
				// Ensure PrepareTestConfig is called to reset to defaults
				PrepareTestConfig()

				// Test default values match config defaults
				assert.Equal(t, "/var/run/openvswitch/", ovsRunDir)
				assert.Equal(t, "/var/run/ovn/", ovnRunDir)
				assert.Equal(t, "/etc/ovn/ovnnb_db.db", config.OvnNorth.DbLocation)
				assert.Equal(t, "/etc/ovn/ovnsb_db.db", config.OvnSouth.DbLocation)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.testFunc)
	}
}
