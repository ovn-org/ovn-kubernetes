package ovn

import (
	"fmt"
	"net"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
)

func TestResolverAdd(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	clusterSubnetStr := "10.128.0.0/14"
	_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
	dnsName := "www.example.com."

	tests := []struct {
		desc                       string
		addresses                  []string
		updateAddressSet           bool
		errExp                     bool
		configIPv4                 bool
		configIPv6                 bool
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:             "NewAddressSet returns error",
			addresses:        []string{},
			updateAddressSet: false,
			errExp:           true,
			configIPv4:       true,
			configIPv6:       false,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{nil, fmt.Errorf("mock error")},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Don't update IPs",
			addresses:        []string{},
			updateAddressSet: false,
			errExp:           false,
			configIPv4:       true,
			configIPv6:       false,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Add IPv4 addresses",
			addresses:        []string{"1.1.1.1", "2.2.2.2"},
			updateAddressSet: true,
			errExp:           false,
			configIPv4:       true,
			configIPv6:       false,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2")}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Add IPv6 addresses",
			addresses:        []string{"2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
			updateAddressSet: true,
			errExp:           false,
			configIPv4:       false,
			configIPv6:       true,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Dual stack support",
			addresses:        []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
			updateAddressSet: true,
			errExp:           false,
			configIPv4:       true,
			configIPv6:       true,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2"), net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Add only supported IPv4 addresses",
			addresses:        []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
			updateAddressSet: true,
			errExp:           false,
			configIPv4:       true,
			configIPv6:       false,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2")}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Add only supported IPv6 addresses",
			addresses:        []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
			updateAddressSet: true,
			errExp:           false,
			configIPv4:       false,
			configIPv6:       true,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Don't add IPs matching cluster subnet",
			addresses:        []string{"10.128.0.1"},
			updateAddressSet: true,
			errExp:           false,
			configIPv4:       true,
			configIPv6:       false,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{nil},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
		{
			desc:             "Don't add IPs matching cluster subnet, add other IPs",
			addresses:        []string{"10.128.0.1", "1.1.1.1", "2.2.2.2"},
			updateAddressSet: true,
			errExp:           false,
			configIPv4:       true,
			configIPv6:       false,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{nil},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2")}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

			for _, item := range tc.addressSetFactoryOpsHelper {
				call := mockAddressSetFactoryOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.addressSetOpsHelper {
				call := mockAddressSetOps.On(item.OnCallMethodName)
				// use exact arguments for AddressSet call to match ips
				call.Arguments = item.OnCallMethodArgs
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			res := NewResolver(mockAddressSetFactoryOps, DefaultNetworkControllerName)
			_, err := res.Add(dnsName, tc.addresses, tc.updateAddressSet)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
				netIPs, _ := res.getResolvedName(dnsName)
				if tc.updateAddressSet {
					ips := []net.IP{}
					for _, addr := range tc.addresses {
						ips = append(ips, net.ParseIP(addr))
					}
					cmpOpts := []cmp.Option{
						cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
					}
					if diff := cmp.Diff(netIPs, ips, cmpOpts...); diff != "" {
						t.Fatal(diff)
					}
				} else {
					if len(netIPs) > 0 {
						t.Fatalf("IPs should not be updated")
					}
				}
			}

			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func TestResolverDelete(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	dnsName := "www.example.com."

	tests := []struct {
		desc                       string
		addresses                  []string
		errExp                     bool
		configIPv4                 bool
		configIPv6                 bool
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:       "Delete added addresses",
			addresses:  []string{"1.1.1.1", "2.2.2.2", "2001:0db8:85a3:0000:0000:8a2e:0370:7334"},
			errExp:     false,
			configIPv4: true,
			configIPv6: true,
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "NewAddressSet",
					OnCallMethodArgType:                 []string{"*ops.DbObjectIDs", "[]net.IP"},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{mockAddressSetOps, nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName:                    "SetIPs",
					OnCallMethodArgType:                 []string{"[]net.IP"},
					OnCallMethodArgs:                    []interface{}{[]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2"), net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")}},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
				{
					OnCallMethodName:                    "Destroy",
					OnCallMethodArgType:                 []string{},
					OnCallMethodArgs:                    []interface{}{},
					RetArgList:                          []interface{}{nil},
					OnCallMethodsArgsStrTypeAppendCount: 0,
					CallTimes:                           1,
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6

			for _, item := range tc.addressSetFactoryOpsHelper {
				call := mockAddressSetFactoryOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			for _, item := range tc.addressSetOpsHelper {
				call := mockAddressSetOps.On(item.OnCallMethodName)
				// use exact arguments for AddressSet call to match ips
				call.Arguments = item.OnCallMethodArgs
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}

			res := NewResolver(mockAddressSetFactoryOps, DefaultNetworkControllerName)
			_, err := res.Add(dnsName, tc.addresses, true)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
				netIPs, _ := res.getResolvedName(dnsName)
				ips := []net.IP{}
				for _, addr := range tc.addresses {
					ips = append(ips, net.ParseIP(addr))
				}
				cmpOpts := []cmp.Option{
					cmpopts.SortSlices(func(ip1, ip2 net.IP) bool { return ip1.String() > ip2.String() }),
				}
				if diff := cmp.Diff(netIPs, ips, cmpOpts...); diff != "" {
					t.Fatal(diff)
				}
			}

			err = res.Delete(dnsName)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
				netIPs, _ := res.getResolvedName(dnsName)
				if netIPs != nil {
					t.Fatalf("IPs should be deleted")
				}
			}

			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func (res *Resolver) getResolvedName(dnsName string) ([]net.IP, addressset.AddressSet) {
	res.dnsLock.Lock()
	defer res.dnsLock.Unlock()
	if resolvedName, exists := res.dnsNames[dnsName]; exists {
		return resolvedName.ips, resolvedName.dnsAddressSet
	}

	return nil, nil
}
