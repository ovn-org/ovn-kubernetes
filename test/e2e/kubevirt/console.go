/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2020 Red Hat, Inc.
 *
 */

package kubevirt

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"

	"google.golang.org/grpc/codes"

	expect "github.com/google/goexpect"
	"github.com/onsi/ginkgo"

	kvv1 "kubevirt.io/api/core/v1"
	v1 "kubevirt.io/api/core/v1"
	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
)

const (
	PromptExpression = `(\$ |\# )`
	CRLF             = "\r\n"
	UTFPosEscape     = "\u001b\\[[0-9]+;[0-9]+H"

	consoleConnectionTimeout = 30 * time.Second
	promptTimeout            = 40 * time.Second
	loginTimeout             = 2 * time.Minute
	configureConsoleTimeout  = 60 * time.Second
)

var (
	shellSuccess       = retValue("0")
	shellFail          = retValue("[1-9].*")
	shellSuccessRegexp = regexp.MustCompile(shellSuccess)
	shellFailRegexp    = regexp.MustCompile(shellFail)
)

// SafeExpectBatch runs the batch from `expected`, connecting to a VMI's console and
// waiting `wait` seconds for the batch to return.
// It validates that the commands arrive to the console.
// NOTE: This functions heritage limitations from `expectBatchWithValidatedSend` refer to it to check them.
func safeExpectBatch(virtClient kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance, expected []expect.Batcher, timeout time.Duration) error {
	_, err := safeExpectBatchWithResponse(virtClient, vmi, expected, timeout)
	return err
}

// safeExpectBatchWithResponse runs the batch from `expected`, connecting to a VMI's console and
// waiting `wait` seconds for the batch to return with a response.
// It validates that the commands arrive to the console.
// NOTE: This functions inherits limitations from `expectBatchWithValidatedSend`, refer to it for more information.
func safeExpectBatchWithResponse(virtClient kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance, expected []expect.Batcher, timeout time.Duration) ([]expect.BatchRes, error) {
	expecter, _, err := newExpecter(virtClient, vmi, consoleConnectionTimeout, expect.Verbose(true), expect.VerboseWriter(GinkgoWriter))
	if err != nil {
		return nil, err
	}
	defer expecter.Close()

	resp, err := expectBatchWithValidatedSend(expecter, expected, timeout)
	if err != nil {
		log.DefaultLogger().Object(vmi).Infof("%v", resp)
	}
	return resp, err
}

func RunCommand(virtClient kubecli.KubevirtClient, vmi *v1.VirtualMachineInstance, command string, timeout time.Duration) (string, error) {
	results, err := safeExpectBatchWithResponse(virtClient, vmi, []expect.Batcher{
		&expect.BSnd{S: "\n"},
		&expect.BExp{R: PromptExpression},
		&expect.BSnd{S: command + "\n"},
		&expect.BExp{R: PromptExpression},
		&expect.BSnd{S: "echo $?\n"},
		&expect.BCas{C: []expect.Caser{
			&expect.Case{
				R: shellSuccessRegexp,
				T: expect.OK(),
			},
			&expect.Case{
				R: shellFailRegexp,
				T: expect.Fail(expect.NewStatus(codes.Unavailable, command+" failed")),
			},
		}},
	}, timeout)
	if err != nil {
		return "", fmt.Errorf("failed to run [%s] at VMI %s, error: %v", command, vmi.Name, err)
	}
	outputLines := strings.Split(results[1].Output, "\n")
	output := strings.Join(outputLines[1:len(outputLines)-1], "\n")
	output = strings.ReplaceAll(output, "\x1b[?2004l", "")
	return output, err
}

func skipInput(scanner *bufio.Scanner) bool {
	return scanner.Scan()
}

// newExpecter will connect to an already logged in VMI console and return the generated expecter it will wait `timeout` for the connection.
func newExpecter(
	virtCli kubecli.KubevirtClient,
	vmi *v1.VirtualMachineInstance,
	timeout time.Duration,
	opts ...expect.Option) (expect.Expecter, <-chan error, error) {
	vmiReader, vmiWriter := io.Pipe()
	expecterReader, expecterWriter := io.Pipe()
	resCh := make(chan error)

	startTime := time.Now()
	serialConsoleOptions := &kubecli.SerialConsoleOptions{ConnectionTimeout: timeout}
	con, err := virtCli.VirtualMachineInstance(vmi.Namespace).SerialConsole(vmi.Name, serialConsoleOptions)
	if err != nil {
		return nil, nil, err
	}
	serialConsoleCreateDuration := time.Since(startTime)
	if timeout-serialConsoleCreateDuration <= 0 {
		return nil, nil,
			fmt.Errorf(
				"creation of SerialConsole took %s - longer than given expecter timeout %s",
				serialConsoleCreateDuration.String(),
				timeout.String(),
			)
	}
	timeout -= serialConsoleCreateDuration

	go func() {
		resCh <- con.Stream(kubecli.StreamOptions{
			In:  vmiReader,
			Out: expecterWriter,
		})
	}()

	opts = append(opts, expect.SendTimeout(timeout), expect.Verbose(true), expect.VerboseWriter(ginkgo.GinkgoWriter))
	return expect.SpawnGeneric(&expect.GenOptions{
		In:  vmiWriter,
		Out: expecterReader,
		Wait: func() error {
			return <-resCh
		},
		Close: func() error {
			expecterWriter.Close()
			vmiReader.Close()
			return nil
		},
		Check: func() bool { return true },
	}, timeout, expect.Verbose(true), expect.VerboseWriter(GinkgoWriter))
}

// expectBatchWithValidatedSend adds the expect.BSnd command to the exect.BExp expression.
// It is done to make sure the match was found in the result of the expect.BSnd
// command and not in a leftover that wasn't removed from the buffer.
// NOTE: the method contains the following limitations:
//   - Use of `BatchSwitchCase`
//   - Multiline commands
//   - No more than one sequential send or receive
func expectBatchWithValidatedSend(expecter expect.Expecter, batch []expect.Batcher, timeout time.Duration) ([]expect.BatchRes, error) {
	sendFlag := false
	expectFlag := false
	previousSend := ""

	const minimumRequiredBatches = 2
	if len(batch) < minimumRequiredBatches {
		return nil, fmt.Errorf("expectBatchWithValidatedSend requires at least 2 batchers, supplied %v", batch)
	}

	for i, batcher := range batch {
		switch batcher.Cmd() {
		case expect.BatchExpect:
			if expectFlag {
				return nil, fmt.Errorf("two sequential expect.BExp are not allowed")
			}
			expectFlag = true
			sendFlag = false
			if _, ok := batch[i].(*expect.BExp); !ok {
				return nil, fmt.Errorf("expectBatchWithValidatedSend support only expect of type BExp")
			}
			bExp, _ := batch[i].(*expect.BExp)
			previousSend = regexp.QuoteMeta(previousSend)

			// Remove the \n since it is translated by the console to \r\n.
			previousSend = strings.TrimSuffix(previousSend, "\n")
			bExp.R = fmt.Sprintf("%s%s%s", previousSend, "((?s).*)", bExp.R)
		case expect.BatchSend:
			if sendFlag {
				return nil, fmt.Errorf("two sequential expect.BSend are not allowed")
			}
			sendFlag = true
			expectFlag = false
			previousSend = batcher.Arg()
		case expect.BatchSwitchCase:
			if bCast, ok := batch[i].(*expect.BCas); ok {
				for _, c := range bCast.C {
					cas, _ := c.(*expect.Case)
					p := regexp.QuoteMeta(previousSend)

					// Remove the \n since it is translated by the console to \r\n.
					p = strings.TrimSuffix(p, "\n")
					cas.R = regexp.MustCompile(fmt.Sprintf("%s%s%s", p, "((?s).*)", cas.R.String()))
				}
			}
		default:
			return nil, fmt.Errorf("unknown command: expectBatchWithValidatedSend supports only BatchExpect and BatchSend")
		}
	}

	res, err := expecter.ExpectBatch(batch, timeout)
	return res, err
}

// LoginToFedora performs a console login to a Fedora base VM
func LoginToFedora(virtClient kubecli.KubevirtClient, vmi *kvv1.VirtualMachineInstance, user, password string) error {
	expecter, _, err := newExpecter(virtClient, vmi, consoleConnectionTimeout, expect.Verbose(true), expect.VerboseWriter(GinkgoWriter))
	if err != nil {
		return err
	}
	defer expecter.Close()

	err = expecter.Send("\n")
	if err != nil {
		return err
	}

	// Do not login, if we already logged in
	loggedInPromptRegex := fmt.Sprintf(
		`(\[%s@%s ~\]\$ |\[root@%s %s\]\# )`, user, vmi.Name, vmi.Name, user,
	)
	b := []expect.Batcher{
		&expect.BSnd{S: "\n"},
		&expect.BExp{R: loggedInPromptRegex},
	}
	_, err = expecter.ExpectBatch(b, promptTimeout)
	if err == nil {
		return nil
	}

	b = []expect.Batcher{
		&expect.BSnd{S: "\n"},
		&expect.BSnd{S: "\n"},
		&expect.BCas{C: []expect.Caser{
			&expect.Case{
				// Using only "login: " would match things like "Last failed login: Tue Jun  9 22:25:30 UTC 2020 on ttyS0"
				// and in case the VM's did not get hostname form DHCP server try the default hostname
				R:  regexp.MustCompile(fmt.Sprintf(`%s login: `, vmi.Name)),
				S:  user + "\n",
				T:  expect.Next(),
				Rt: 10,
			},
			&expect.Case{
				R:  regexp.MustCompile(`Password:`),
				S:  password + "\n",
				T:  expect.Next(),
				Rt: 10,
			},
			&expect.Case{
				R:  regexp.MustCompile(`Login incorrect`),
				T:  expect.LogContinue("Failed to log in", expect.NewStatus(codes.PermissionDenied, "login failed")),
				Rt: 10,
			},
			&expect.Case{
				R: regexp.MustCompile(loggedInPromptRegex),
				T: expect.OK(),
			},
		}},
		&expect.BSnd{S: "sudo su\n"},
		&expect.BExp{R: PromptExpression},
	}
	res, err := expecter.ExpectBatch(b, loginTimeout)
	if err != nil {
		log.DefaultLogger().Object(vmi).Reason(err).Errorf("Login attempt failed: %+v", res)
		// Try once more since sometimes the login prompt is ripped apart by asynchronous daemon updates
		if retryRes, retryErr := expecter.ExpectBatch(b, 1*time.Minute); retryErr != nil {
			log.DefaultLogger().Object(vmi).Reason(retryErr).Errorf("Retried login attempt after two minutes failed: %+v", retryRes)
			return retryErr
		}
	}

	err = configureConsole(expecter, false)
	if err != nil {
		return err
	}
	return nil
}

func configureConsole(expecter expect.Expecter, shouldSudo bool) error {
	sudoString := ""
	if shouldSudo {
		sudoString = "sudo "
	}
	batch := []expect.Batcher{
		&expect.BSnd{S: "stty cols 500 rows 500\n"},
		&expect.BExp{R: PromptExpression},
		&expect.BSnd{S: "echo $?\n"},
		&expect.BExp{R: retValue("0")},
		&expect.BSnd{S: fmt.Sprintf("%sdmesg -n 1\n", sudoString)},
		&expect.BExp{R: PromptExpression},
		&expect.BSnd{S: "echo $?\n"},
		&expect.BExp{R: retValue("0")},
	}
	resp, err := expecter.ExpectBatch(batch, configureConsoleTimeout)
	if err != nil {
		log.DefaultLogger().Infof("%v", resp)
	}
	return err
}
func retValue(retcode string) string {
	return retcode + CRLF + ".*" + PromptExpression
}
