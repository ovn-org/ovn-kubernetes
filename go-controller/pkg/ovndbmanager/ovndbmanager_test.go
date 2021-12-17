package ovndbmanager

import (
	"fmt"
	"strings"
	"testing"
)

type mockRes struct {
	res    string
	stderr string
	err    error
	called bool
}

const (
	status_template = `87f0
Name: %s
Cluster ID: f832 (f832bbff-e28c-4656-83f0-075e91a7ab8f)
Server ID: 87f0 (87f0d686-8a8d-4585-9513-45efac449101)
Address: %s
Status: cluster member
Role: %s
Term: 4
Leader: bbf6
Vote: unknown

Election timer: %s
Log: [19418, 26772]
Entries not yet committed: 0
Entries not yet applied: 0
Connections: ->bbf6 ->ad31 <-bbf6 <-ad31
Disconnections: 1
%s`

	serverAddress = "ssl:10.1.1.185:9643"

	servers = `Servers:
    87f0 (87f0 at ssl:10.1.1.185:9643) (self)
    bbf6 (bbf6 at ssl:10.1.1.218:9643) last msg 2757 ms ago
    ad31 (ad31 at ssl:10.1.1.211:9643) last msg 153868958 ms ago`
)

func TestEnsureElectionTimeout(t *testing.T) {
	var mockCalls map[string]*mockRes
	unexpectedKeys := make([]string, 0)
	mock := func(timeout int, args ...string) (string, string, error) {
		key := keyForArgs(args...)
		res, ok := mockCalls[key]
		if !ok {
			unexpectedKeys = append(unexpectedKeys, key)
			return "", "key not found", fmt.Errorf("key not found")
		}
		res.called = true
		return res.res, res.stderr, res.err
	}

	db := &dbProperties{
		appCtl: mock,
		dbName: "OVN_Northbound",
	}
	tests := []struct {
		desc         string
		mockCalls    map[string]*mockRes
		timeout      int
		role         string
		currentTimer string
		errorString  string
	}{
		{
			desc: "Test error: Unable to get cluster status",
			mockCalls: map[string]*mockRes{
				keyForArgs("cluster/status", "OVN_Northbound"): {
					res:    "",
					stderr: "failure",
					err:    fmt.Errorf("failure"),
				},
			},
			errorString: "unable to get cluster status for",
		},
		{
			desc:         "Test error: Failed to get current election timer",
			mockCalls:    map[string]*mockRes{},
			currentTimer: "a",
			role:         "leader",
			errorString:  "failed to get current election timer",
		},
		{
			desc:         "Follower, not trying to change",
			mockCalls:    map[string]*mockRes{},
			timeout:      1000,
			role:         "follower",
			currentTimer: "10000",
		},
		{
			desc:         "leader, timer doesn't change",
			mockCalls:    map[string]*mockRes{},
			timeout:      1000,
			role:         "leader",
			currentTimer: "1000",
		},
		{
			desc: "Test error: failed to change election timer when leader timer must change",
			mockCalls: map[string]*mockRes{
				keyForArgs("cluster/change-election-timer", "OVN_Northbound", "2000"): {
					res:    "",
					stderr: "failure",
					err:    fmt.Errorf("failure"),
				},
			},
			timeout:      2000,
			role:         "leader",
			currentTimer: "1500",
			errorString:  "failed to change election timer for",
		},
		{
			desc: "Test error: failed to change election timer when leader timer must change but desired is more than double",
			mockCalls: map[string]*mockRes{
				keyForArgs("cluster/change-election-timer", "OVN_Northbound", "3000"): {
					res:    "",
					stderr: "failure",
					err:    fmt.Errorf("failure"),
				},
			},
			timeout:      5000,
			role:         "leader",
			currentTimer: "1500",
			errorString:  "failed to change election timer for",
		},
		{
			desc: "leader, timer must change",
			mockCalls: map[string]*mockRes{
				keyForArgs("cluster/change-election-timer", "OVN_Northbound", "2000"): {
					res:    "change of election timer initiated",
					stderr: "",
					err:    nil,
				},
			},
			timeout:      2000,
			role:         "leader",
			currentTimer: "1500",
		},
		{
			desc: "leader, timer must change but desired is more than double",
			mockCalls: map[string]*mockRes{
				keyForArgs("cluster/change-election-timer", "OVN_Northbound", "3000"): {
					res:    "change of election timer initiated",
					stderr: "",
					err:    nil,
				},
			},
			timeout:      5000,
			role:         "leader",
			currentTimer: "1500",
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			// default mockCalls which may be supplemented or overwritten by
			// more specific tc mockCalls from the above maps
			mockCalls = map[string]*mockRes{
				keyForArgs("cluster/status", "OVN_Northbound"): {
					res: fmt.Sprintf(
						status_template,
						serverAddress,
						"OVN_Northbound",
						tc.role,
						tc.currentTimer,
						servers),
					stderr: "",
					err:    nil,
				},
			}
			for k, v := range tc.mockCalls {
				mockCalls[k] = v
			}

			db.electionTimer = tc.timeout
			err := ensureElectionTimeout(db)

			// fail either if an error is seen but not expected
			// or if an error is expected but when the subscring does not match the error
			failOnErrorMismatch(t, err, tc.errorString)

			for k, c := range tc.mockCalls {
				if !c.called {
					t.Errorf("Expecting call with args %s", k)
				}
			}
			if len(unexpectedKeys) > 0 {
				t.Errorf("Received unexpected calls %v", unexpectedKeys)
			}
		})
	}
}

func keyForArgs(args ...string) string {
	return strings.Join(args, "-")
}

// failOnErrorMismatch fails either if an error is seen but not expected
// or if an error is expected but when the substring does not match the error
func failOnErrorMismatch(t *testing.T, receivedErr error, expectedErrorString string) {
	if receivedErr != nil {
		if expectedErrorString == "" {
			t.Errorf("No error expected. However, received '%v' from method under test.", receivedErr)
		} else if !strings.Contains(receivedErr.Error(), expectedErrorString) {
			t.Errorf("Expected error string to contain '%s'. However, method under test threw error '%v'.", expectedErrorString, receivedErr)
		}
	} else if expectedErrorString != "" {
		t.Errorf("Error with error string '%s' expected. However, method under test completed without error.", expectedErrorString)
	}
}
