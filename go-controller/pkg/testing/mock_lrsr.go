package testing

import (
	"encoding/base64"
	"fmt"

	goovn "github.com/ebay/go-ovn"
	"github.com/mitchellh/copystructure"
	"k8s.io/klog/v2"
)

func makeUUID(params string) string {
	return base64.StdEncoding.EncodeToString([]byte(params))

}

// Add LRSR with given ip_prefix on given lr
func (mock *MockOVNClient) LRSRAdd(lr string, ip_prefix string, nexthop string, output_port *string, policy *string, external_ids map[string]string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("Adding  static route to router %s", lr)
	extIdsMap := make(map[interface{}]interface{})
	for k, v := range external_ids {
		extIdsMap[k] = v
	}
	uuidParamStr := lr + ip_prefix + nexthop + *output_port + *policy
	fakeUUID := makeUUID(uuidParamStr)
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpAdd,
			table:   LogicalRouterStaticRouteType,
			objName: fakeUUID,
			obj:     &goovn.LogicalRouterStaticRoute{UUID: fakeUUID, IPPrefix: ip_prefix, Nexthop: nexthop, Policy: policy, ExternalID: extIdsMap, OutputPort: output_port},
		},
	}, nil

}

// Delete LRSR with given ip_prefix, nexthop, outputPort and policy on given lr
func (mock *MockOVNClient) LRSRDel(lr string, prefix string, nexthop, outputPort, policy *string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("LRSRDel called for lr: %s and prefix: %s nexthop: %s outputPort: %s policy: %s", lr, prefix, *nexthop, *outputPort, *policy)
	uuidParamStr := lr + prefix + *nexthop + *outputPort + *policy
	fakeUUID := makeUUID(uuidParamStr)
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpDelete,
			table:   LogicalRouterStaticRouteType,
			objName: fakeUUID,
		},
	}, nil
}

// Delete LRSR by uuid given lr
func (mock *MockOVNClient) LRSRDelByUUID(lr, uuid string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("LRSRDelByUUID called for lr: %s and UUID: %s", lr, uuid)
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpDelete,
			table:   LogicalRouterStaticRouteType,
			objName: uuid,
		},
	}, nil
}

// Get all LRSRs by lr
func (mock *MockOVNClient) LRSRList(lr string) ([]*goovn.LogicalRouterStaticRoute, error) {
	klog.V(5).Infof("LRSRList called for lr: %s", lr)
	var lrsrCache MockObjectCacheByName
	var ok bool

	lrsrArray := []*goovn.LogicalRouterStaticRoute{}
	if lrsrCache, ok = mock.cache[LogicalRouterStaticRouteType]; !ok {
		klog.V(5).Infof("Cache doesn't have any object of type %s", LogicalRouterStaticRouteType)
		return nil, goovn.ErrorSchema
	}
	var lrsrEntry interface{}
	for _, lrsrEntry = range lrsrCache {
		lrsrEntry, err := copystructure.Copy(lrsrEntry)
		if err != nil {
			panic(err) // should never happen
		}

		lrsr, ok := lrsrEntry.(*goovn.LogicalRouterStaticRoute)
		if !ok {
			return nil, fmt.Errorf("invalid object type assertion for %s", LogicalRouterStaticRouteType)
		}
		lrsrArray = append(lrsrArray, lrsr)
	}
	return lrsrArray, nil
}
