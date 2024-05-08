package dnsnameresolver

import (
	"fmt"
	"net"
	"testing"
	"time"

	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set/mocks"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	util_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"github.com/miekg/dns"
	utilnet "k8s.io/utils/net"
)

const DefaultNetworkControllerName = "default-network-controller"

func TestNewEgressDNS(t *testing.T) {
	testCh := make(chan struct{})
	dbSetup := libovsdbtest.TestSetup{}

	libovsdbOvnNBClient, _, libovsdbCleanup, err := libovsdbtest.NewNBSBTestHarness(dbSetup)
	assert.Nil(t, err)
	t.Cleanup(libovsdbCleanup.Cleanup)

	testOvnAddFtry := addressset.NewOvnAddressSetFactory(libovsdbOvnNBClient, config.IPv4Mode, config.IPv6Mode)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	tests := []struct {
		desc             string
		errExp           bool
		dnsOpsMockHelper []ovntest.TestifyMockHelper
	}{
		{
			desc:   "fails to read the /etc/resolv.conf file",
			errExp: true,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{nil, fmt.Errorf("mock error")}, 0, 1},
			},
		},
		{
			desc: "positive tests case",
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{}, nil}, 0, 1},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			_, err := NewEgressDNS(testOvnAddFtry, DefaultNetworkControllerName, testCh, 0)
			//t.Log(res, err)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
			}
			mockDnsOps.AssertExpectations(t)
		})
	}
}

func generateRR(dnsName, ip, nextQueryTime string) dns.RR {
	var rr dns.RR
	if utilnet.IsIPv6(net.ParseIP(ip)) {
		rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      AAAA       " + ip)
	} else {
		rr, _ = dns.NewRR(dnsName + ".        " + nextQueryTime + "     IN      A       " + ip)
	}
	return rr
}

func TestAdd(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	test1DNSName := "www.test.com"
	test1IPv4 := "2.2.2.2"
	test1IPv4Update := "3.3.3.3"
	test1IPv6 := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
	clusterSubnetStr := "10.128.0.0/14"
	_, clusterSubnet, _ := net.ParseCIDR(clusterSubnetStr)
	clusterSubnetIP := "10.128.0.1"
	tests := []struct {
		desc                       string
		errExp                     bool
		dnsName                    string
		configIPv4                 bool
		configIPv6                 bool
		testingUpdateOnQueryTime   bool
		syncTime                   time.Duration
		waitForSyncLoop            bool
		dnsOpsMockHelper           []ovntest.TestifyMockHelper
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:     "NewAddressSet returns error",
			errExp:   true,
			syncTime: 5 * time.Minute,
			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{
					Servers: []string{"1.1.1.1"},
					Port:    "1234"}, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"NewAddressSet", []string{"*ops.DbObjectIDs", "[]string"}, []interface{}{}, []interface{}{nil, fmt.Errorf("mock error")}, 0, 1},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) succeeds IPv4 only",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{
					Servers: []string{"1.1.1.1"},
					Port:    "1234"}, nil}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},

				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"NewAddressSet", []string{"*ops.DbObjectIDs", "[]string"}, []interface{}{}, []interface{}{mockAddressSetOps, nil}, 0, 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) ignores ips from clusterSubnet",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{
					Servers: []string{"1.1.1.1"},
					Port:    "1234"}, nil}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},

				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{},
					[]interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, clusterSubnetIP, "300")}}, 500 * time.Second, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"NewAddressSet", []string{"*ops.DbObjectIDs", "[]string"}, []interface{}{}, []interface{}{mockAddressSetOps, nil}, 0, 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:       "EgressFirewall Add(dnsName) ignores ips from clusterSubnet leaving other ips",
			errExp:     false,
			syncTime:   5 * time.Minute,
			dnsName:    test1DNSName,
			configIPv4: true,
			configIPv6: false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{
					Servers: []string{"1.1.1.1"},
					Port:    "1234"}, nil}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},

				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{},
					[]interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300"), generateRR(test1DNSName, clusterSubnetIP, "300")}}, 500 * time.Second, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"NewAddressSet", []string{"*ops.DbObjectIDs", "[]string"}, []interface{}{}, []interface{}{mockAddressSetOps, nil}, 0, 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:                     "EgressFirewall Add(dnsName) succeeds dual stack",
			errExp:                   false,
			syncTime:                 5 * time.Minute,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: false,
			configIPv4:               true,
			configIPv6:               true,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{
					Servers: []string{"1.1.1.1"},
					Port:    "1234"}, nil}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},
				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv6, "300")}}, 500 * time.Second, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"NewAddressSet", []string{"*ops.DbObjectIDs", "[]string"}, []interface{}{}, []interface{}{mockAddressSetOps, nil}, 0, 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4, net.ParseIP(test1IPv6).String()}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
		{
			desc:                     "EgressFirewall DNS Run Runs update after the ttl returned from the DNS server expires",
			errExp:                   false,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: true,
			syncTime:                 5 * time.Minute,
			configIPv4:               true,
			configIPv6:               false,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{
					Servers: []string{"1.1.1.1"},
					Port:    "1234"}, nil}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},

				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				// return a very low ttl so that the update based on ttl timeout occurs
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "4")}}, 1 * time.Second, nil}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},

				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4Update, "300")}}, 1 * time.Second, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"NewAddressSet", []string{"*ops.DbObjectIDs", "[]string"}, []interface{}{}, []interface{}{mockAddressSetOps, nil}, 0, 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4}},
					RetArgList:       []interface{}{nil},
				},
				{
					OnCallMethodName: "SetAddresses",
					OnCallMethodArgs: []interface{}{[]string{test1IPv4Update}},
					RetArgList:       []interface{}{nil},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			testCh := make(chan struct{})
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6
			config.Default.ClusterSubnets = []config.CIDRNetworkEntry{{CIDR: clusterSubnet}}

			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
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
			res, err := NewEgressDNS(mockAddressSetFactoryOps, DefaultNetworkControllerName, testCh, tc.syncTime)
			assert.NoError(t, err)

			res.Run()

			_, err = res.Add("addNamespace", test1DNSName)
			if tc.errExp {
				assert.Error(t, err)
			} else {
				assert.Nil(t, err)
				for stay, timeout := true, time.After(10*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						break
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout: it is taking too long for the goroutine to complete")
					default:
					}

				}
			}

			if tc.testingUpdateOnQueryTime {
				for stay, timeout := true, time.After(15*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						if len(dnsResolves) == 1 && dnsResolves[0].String() == test1IPv4Update {
							break
						}
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout waiting for update based on ttl to fire or process")
					default:
					}

				}
			}

			close(testCh)
			mockDnsOps.AssertExpectations(t)
			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockDnsOps.ExpectedCalls = nil
			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func TestDelete(t *testing.T) {
	mockAddressSetFactoryOps := new(mocks.AddressSetFactory)
	mockAddressSetOps := new(mocks.AddressSet)
	mockDnsOps := new(util_mocks.DNSOps)
	util.SetDNSLibOpsMockInst(mockDnsOps)
	test1DNSName := "www.test.com"
	test1IPv4 := "2.2.2.2"
	test1IPv6 := "2001:0db8:85a3:0000:0000:8a2e:0370:7334"
	tests := []struct {
		desc                       string
		errExp                     bool
		dnsName                    string
		configIPv4                 bool
		configIPv6                 bool
		testingUpdateOnQueryTime   bool
		syncTime                   time.Duration
		waitForSyncLoop            bool
		dnsOpsMockHelper           []ovntest.TestifyMockHelper
		addressSetFactoryOpsHelper []ovntest.TestifyMockHelper
		addressSetOpsHelper        []ovntest.TestifyMockHelper
	}{
		{
			desc:                     "EgressFirewall Delete functions",
			errExp:                   false,
			syncTime:                 5 * time.Minute,
			dnsName:                  test1DNSName,
			testingUpdateOnQueryTime: false,
			configIPv4:               true,
			configIPv6:               true,

			dnsOpsMockHelper: []ovntest.TestifyMockHelper{
				{"ClientConfigFromFile", []string{"string"}, []interface{}{}, []interface{}{&dns.ClientConfig{
					Servers: []string{"1.1.1.1"},
					Port:    "1234"}, nil}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},
				{"Fqdn", []string{"string"}, []interface{}{}, []interface{}{test1DNSName}, 0, 1},
				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"SetQuestion", []string{"*dns.Msg", "string", "uint16"}, []interface{}{}, []interface{}{&dns.Msg{}}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv4, "300")}}, 500 * time.Second, nil}, 0, 1},
				{"Exchange", []string{"*dns.Client", "*dns.Msg", "string"}, []interface{}{}, []interface{}{&dns.Msg{Answer: []dns.RR{generateRR(test1DNSName, test1IPv6, "300")}}, 500 * time.Second, nil}, 0, 1},
			},
			addressSetFactoryOpsHelper: []ovntest.TestifyMockHelper{
				{"NewAddressSet", []string{"*ops.DbObjectIDs", "[]string"}, []interface{}{}, []interface{}{mockAddressSetOps, nil}, 0, 1},
			},
			addressSetOpsHelper: []ovntest.TestifyMockHelper{
				{"SetAddresses", []string{"[]string"}, []interface{}{}, []interface{}{nil}, 0, 1},
				{"Destroy", []string{}, []interface{}{}, []interface{}{nil}, 0, 1},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			testCh := make(chan struct{})
			config.IPv4Mode = tc.configIPv4
			config.IPv6Mode = tc.configIPv6

			for _, item := range tc.dnsOpsMockHelper {
				call := mockDnsOps.On(item.OnCallMethodName)
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
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
				for _, arg := range item.OnCallMethodArgType {
					call.Arguments = append(call.Arguments, mock.AnythingOfType(arg))
				}
				for _, ret := range item.RetArgList {
					call.ReturnArguments = append(call.ReturnArguments, ret)
				}
				call.Once()
			}
			res, err := NewEgressDNS(mockAddressSetFactoryOps, DefaultNetworkControllerName, testCh, tc.syncTime)
			assert.NoError(t, err)

			res.Run()

			_, err = res.Add("addNamespace", test1DNSName)
			if tc.errExp {
				assert.Error(t, err)
			} else {

				assert.Nil(t, err)
				for stay, timeout := true, time.After(10*time.Second); stay; {
					_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
					if dnsResolves != nil {
						break
					}
					select {
					case <-timeout:
						stay = false
						t.Errorf("timeout: it is taking too long for the goroutine to complete")
					default:
					}

				}
			}
			_, dnsResolves, _ := res.getDNSEntry(tc.dnsName)
			res.Delete("addNamespace")
			for stay, timeout := true, time.After(10*time.Second); stay; {
				_, dnsResolves, _ = res.getDNSEntry(tc.dnsName)
				if dnsResolves == nil {
					break
				}
				select {
				case <-timeout:
					stay = false
					t.Errorf("timeout: dns is taking to long for the goroutine to update the dns object")
				default:
				}
			}

			assert.Nil(t, dnsResolves)

			close(testCh)
			mockDnsOps.AssertExpectations(t)
			mockAddressSetFactoryOps.AssertExpectations(t)
			mockAddressSetOps.AssertExpectations(t)

			mockDnsOps.ExpectedCalls = nil
			mockAddressSetFactoryOps.ExpectedCalls = nil
			mockAddressSetOps.ExpectedCalls = nil
		})
	}
}

func (e *EgressDNS) getDNSEntry(dnsName string) (map[string]struct{}, []net.IP, addressset.AddressSet) {
	e.lock.Lock()
	defer e.lock.Unlock()
	if dnsEntry, exists := e.dnsEntries[dnsName]; exists {
		return dnsEntry.namespaces, dnsEntry.dnsResolves, dnsEntry.dnsAddressSet
	}

	return nil, nil, nil
}
