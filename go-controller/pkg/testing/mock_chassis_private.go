package testing

import (
	goovn "github.com/ebay/go-ovn"
	"k8s.io/klog/v2"
)

// Delete chassis row from Chassis_Private table with given name
func (mock *MockOVNClient) ChassisPrivateDel(chName string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("Deleting chassis row from Chassis_Private table %s", chName)
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpDelete,
			table:   ChassisPrivateType,
			objName: chName,
		},
	}, nil
}
