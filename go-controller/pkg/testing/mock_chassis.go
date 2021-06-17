package testing

import (
	"fmt"

	goovn "github.com/ebay/go-ovn"
	"github.com/mitchellh/copystructure"
	"k8s.io/klog/v2"
)

// Get logical switch port by name
func (mock *MockOVNClient) ChassisList() ([]*goovn.Chassis, error) {
	var chassisCache MockObjectCacheByName
	var ok bool

	chArray := []*goovn.Chassis{}
	if chassisCache, ok = mock.cache[ChassisType]; !ok {
		klog.V(5).Infof("Cache doesn't have any object of type %s", ChassisType)
		return nil, goovn.ErrorSchema
	}
	var ch interface{}
	for _, ch = range chassisCache {
		ch, err := copystructure.Copy(ch)
		if err != nil {
			panic(err) // should never happen
		}
		chassis, ok := ch.(*goovn.Chassis)
		if !ok {
			return nil, fmt.Errorf("invalid object type assertion for %s", ChassisType)
		}
		chArray = append(chArray, chassis)
	}
	return chArray, nil
}

// Delete chassis with given name
func (mock *MockOVNClient) ChassisDel(chName string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("Deleting chassis %s", chName)
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpDelete,
			table:   ChassisType,
			objName: chName,
		},
	}, nil
}

// Get chassis by hostname or name
func (mock *MockOVNClient) ChassisGet(name string) ([]*goovn.Chassis, error) {
	var chassisCache MockObjectCacheByName
	var ok bool
	if chassisCache, ok = mock.cache[ChassisType]; !ok {
		klog.V(5).Infof("Cache doesn't have any object of type %s", ChassisType)
		return nil, goovn.ErrorSchema
	}

	var ch interface{}
	chArray := make([]*goovn.Chassis, 0, len(chassisCache))
	for _, ch = range chassisCache {
		ch, err := copystructure.Copy(ch)
		if err != nil {
			panic(err) // should never happen
		}
		chassis, ok := ch.(*goovn.Chassis)
		if !ok {
			return nil, fmt.Errorf("invalid object type assertion for %s", ChassisType)
		}
		if chassis.Name == name || chassis.Hostname == name {
			chArray = append(chArray, chassis)
		}
	}
	return chArray, nil
}
