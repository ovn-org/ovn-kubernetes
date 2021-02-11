package testing

import (
	goovn "github.com/ebay/go-ovn"
	"k8s.io/klog/v2"
)

// Add ACL
func (mock *MockOVNClient) ACLAdd(ls, direct, match, action string, priority int, external_ids map[string]string, logflag bool, meter string, severity string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("Adding ACL %s to switch %s", ls, ls)

	ext_ids := make(map[interface{}]interface{})
	for k, v := range external_ids {
		ext_ids[k] = v
	}

	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpAdd,
			table:   ACLType,
			objName: ls,
			obj: &goovn.ACL{
				UUID:       FakeUUID,
				Action:     action,
				Direction:  direct,
				Match:      match,
				Priority:   priority,
				Log:        logflag,
				Meter:      []string{meter},
				Severity:   severity,
				ExternalID: ext_ids,
			},
		},
	}, nil
}
