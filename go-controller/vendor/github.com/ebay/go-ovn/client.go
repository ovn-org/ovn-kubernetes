/**
 * Copyright (c) 2017 eBay Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 **/

package goovn

import (
	"fmt"
	"sync"

	"crypto/tls"
	"github.com/ebay/libovsdb"
	"log"
	"time"
)

// Client ovnnb/sb client
// Note: We can create different clients for ovn nb and sb each in future.
type Client interface {
	// Get logical switch by name
	LSGet(ls string) ([]*LogicalSwitch, error)
	// Create ls named SWITCH
	LSAdd(ls string) (*OvnCommand, error)
	// Del ls and all its ports
	LSDel(ls string) (*OvnCommand, error)
	// Get all logical switches
	LSList() ([]*LogicalSwitch, error)
	// Add external_ids to logical switch
	LSExtIdsAdd(ls string, external_ids map[string]string) (*OvnCommand, error)
	// Del external_ids from logical_switch
	LSExtIdsDel(ls string, external_ids map[string]string) (*OvnCommand, error)
	// Link logical switch to router
	LinkSwitchToRouter(lsw, lsp, lr, lrp, lrpMac string, networks []string, externalIds map[string]string) (*OvnCommand, error)

	// Get logical switch port by name
	LSPGet(lsp string) (*LogicalSwitchPort, error)
	// Add logical port PORT on SWITCH
	LSPAdd(ls string, lsp string) (*OvnCommand, error)
	// Delete PORT from its attached switch
	LSPDel(lsp string) (*OvnCommand, error)
	// Set addressset per lport
	LSPSetAddress(lsp string, addresses ...string) (*OvnCommand, error)
	// Set port security per lport
	LSPSetPortSecurity(lsp string, security ...string) (*OvnCommand, error)
	// Get all lport by lswitch
	LSPList(ls string) ([]*LogicalSwitchPort, error)

	// Add LB to LSW
	LSLBAdd(ls string, lb string) (*OvnCommand, error)
	// Delete LB from LSW
	LSLBDel(ls string, lb string) (*OvnCommand, error)
	// List Load balancers for a LSW
	LSLBList(ls string) ([]*LoadBalancer, error)

	// Add ACL
	ACLAdd(ls, direct, match, action string, priority int, external_ids map[string]string, logflag bool, meter string, severity string) (*OvnCommand, error)
	// Delete acl
	ACLDel(ls, direct, match string, priority int, external_ids map[string]string) (*OvnCommand, error)
	// Get all acl by lswitch
	ACLList(ls string) ([]*ACL, error)

	// Get AS
	ASGet(name string) (*AddressSet, error)
	// Update address set
	ASUpdate(name string, addrs []string, external_ids map[string]string) (*OvnCommand, error)
	// Add addressset
	ASAdd(name string, addrs []string, external_ids map[string]string) (*OvnCommand, error)
	// Delete addressset
	ASDel(name string) (*OvnCommand, error)
	// Get all AS
	ASList() ([]*AddressSet, error)

	// Get LR with given name
	LRGet(name string) ([]*LogicalRouter, error)
	// Add LR with given name
	LRAdd(name string, external_ids map[string]string) (*OvnCommand, error)
	// Delete LR with given name
	LRDel(name string) (*OvnCommand, error)
	// Get LRs
	LRList() ([]*LogicalRouter, error)

	// Add LRP with given name on given lr
	LRPAdd(lr string, lrp string, mac string, network []string, peer string, external_ids map[string]string) (*OvnCommand, error)
	// Delete LRP with given name on given lr
	LRPDel(lr string, lrp string) (*OvnCommand, error)
	// Get all lrp by lr
	LRPList(lr string) ([]*LogicalRouterPort, error)

	// Add LRSR with given ip_prefix on given lr
	LRSRAdd(lr string, ip_prefix string, nexthop string, output_port []string, policy []string, external_ids map[string]string) (*OvnCommand, error)
	// Delete LRSR with given ip_prefix, nexthop, policy and outputPort on given lr
	LRSRDel(lr string, prefix string, nexthop, policy, outputPort *string) (*OvnCommand, error)
	// Get all LRSRs by lr
	LRSRList(lr string) ([]*LogicalRouterStaticRoute, error)

	// Add LB to LR
	LRLBAdd(lr string, lb string) (*OvnCommand, error)
	// Delete LB from LR
	LRLBDel(lr string, lb string) (*OvnCommand, error)
	// List Load balancers for a LR
	LRLBList(lr string) ([]*LoadBalancer, error)

	// Get LB with given name
	LBGet(name string) ([]*LoadBalancer, error)
	// Add LB
	LBAdd(name string, vipPort string, protocol string, addrs []string) (*OvnCommand, error)
	// Delete LB with given name
	LBDel(name string) (*OvnCommand, error)
	// Update existing LB
	LBUpdate(name string, vipPort string, protocol string, addrs []string) (*OvnCommand, error)

	// Set dhcp4_options uuid on lsp
	LSPSetDHCPv4Options(lsp string, options string) (*OvnCommand, error)
	// Get dhcp4_options from lsp
	LSPGetDHCPv4Options(lsp string) (*DHCPOptions, error)
	// Set dhcp6_options uuid on lsp
	LSPSetDHCPv6Options(lsp string, options string) (*OvnCommand, error)
	// Get dhcp6_options from lsp
	LSPGetDHCPv6Options(lsp string) (*DHCPOptions, error)
	// Set options in LSP
	LSPSetOptions(lsp string, options map[string]string) (*OvnCommand, error)
	// Get options from LSP
	LSPGetOptions(lsp string) (map[string]string, error)
	// Set dynamic addresses in LSP
	LSPSetDynamicAddresses(lsp string, address string) (*OvnCommand, error)
	// Get dynamic addresses from LSP
	LSPGetDynamicAddresses(lsp string) (string, error)
	// Set external_ids for LSP
	LSPSetExternalIds(lsp string, external_ids map[string]string) (*OvnCommand, error)
	// Get external_ids from LSP
	LSPGetExternalIds(lsp string) (map[string]string, error)
	// Add dhcp options for cidr and provided external_ids
	DHCPOptionsAdd(cidr string, options map[string]string, external_ids map[string]string) (*OvnCommand, error)
	// Set dhcp options and set external_ids for specific uuid
	DHCPOptionsSet(uuid string, options map[string]string, external_ids map[string]string) (*OvnCommand, error)
	// Del dhcp options via provided external_ids
	DHCPOptionsDel(uuid string) (*OvnCommand, error)
	// Get single dhcp via provided uuid
	DHCPOptionsGet(uuid string) (*DHCPOptions, error)
	// List dhcp options
	DHCPOptionsList() ([]*DHCPOptions, error)

	// Add qos rule
	QoSAdd(ls string, direction string, priority int, match string, action map[string]int, bandwidth map[string]int, external_ids map[string]string) (*OvnCommand, error)
	// Del qos rule, to delete wildcard specify priority -1 and string options as ""
	QoSDel(ls string, direction string, priority int, match string) (*OvnCommand, error)
	// Get qos rules by logical switch
	QoSList(ls string) ([]*QoS, error)

	//Add NAT to Logical Router
	LRNATAdd(lr string, ntype string, externalIp string, logicalIp string, external_ids map[string]string, logicalPortAndExternalMac ...string) (*OvnCommand, error)
	//Del NAT from Logical Router
	LRNATDel(lr string, ntype string, ip ...string) (*OvnCommand, error)
	// Get NAT List by Logical Router
	LRNATList(lr string) ([]*NAT, error)
	// Add Meter with a Meter Band
	MeterAdd(name, action string, rate int, unit string, external_ids map[string]string, burst int) (*OvnCommand, error)
	// Deletes meters
	MeterDel(name ...string) (*OvnCommand, error)
	// List Meters
	MeterList() ([]*Meter, error)
	// List Meter Bands
	MeterBandsList() ([]*MeterBand, error)
	// Exec command, support mul-commands in one transaction.
	Execute(cmds ...*OvnCommand) error

	// Add chassis with given name
	ChassisAdd(name string, hostname string, etype []string, ip string, external_ids map[string]string,
		transport_zones []string, vtep_lswitches []string) (*OvnCommand, error)
	// Delete chassis with given name
	ChassisDel(chName string) (*OvnCommand, error)
	// Get chassis by hostname or name
	ChassisGet(chname string) ([]*Chassis, error)

	// Get encaps by chassis name
	EncapList(chname string) ([]*Encap, error)

	// Set NB_Global table options
	NBGlobalSetOptions(options map[string]string) (*OvnCommand, error)

	// Get NB_Global table options
	NBGlobalGetOptions() (map[string]string, error)

	// Set SB_Global table options
	SBGlobalSetOptions(options map[string]string) (*OvnCommand, error)

	// Get SB_Global table options
	SBGlobalGetOptions() (map[string]string, error)

	// Close connection to OVN
	Close() error
}

var _ Client = &ovndb{}

type ovndb struct {
	client       *libovsdb.OvsdbClient
	cache        map[string]map[string]libovsdb.Row
	cachemutex   sync.RWMutex
	tranmutex    sync.Mutex
	signalCB     OVNSignal
	disconnectCB OVNDisconnectedCallback
	db           string
	addr         string
	tlsConfig    *tls.Config
	reconn       bool
}

func connect(c *ovndb) error {
	ovsdb, err := libovsdb.Connect(c.addr, c.tlsConfig)
	if err != nil {
		return err
	}
	c.client = ovsdb
	initial, err := ovsdb.MonitorAll(c.db, "")
	if err != nil {
		return err
	}
	c.populateCache(*initial)
	notifier := ovnNotifier{c}
	ovsdb.Register(notifier)
	return nil
}

func NewClient(cfg *Config) (Client, error) {
	db := cfg.Db
	// db string should strictly be OVN_Northbound or OVN_Southbound
	switch db {
	case DBNB, DBSB:
		break
	case "":
		db = DBNB
	default:
		return nil, fmt.Errorf("Valid db names are: %s and %s", DBNB, DBSB)
	}

	ovndb := &ovndb{
		cache:        make(map[string]map[string]libovsdb.Row),
		signalCB:     cfg.SignalCB,
		disconnectCB: cfg.DisconnectCB,
		db:           db,
		addr:         cfg.Addr,
		tlsConfig:    cfg.TLSConfig,
		reconn:       cfg.Reconnect,
	}

	err := connect(ovndb)
	if err != nil {
		return nil, err
	}
	return ovndb, err
}

func (c *ovndb) reconnect() {
	ticker := time.NewTicker(500 * time.Millisecond)
	go func() {
		log.Printf("%s disconnected. Reconnecting ... \n", c.addr)
		retry := 0
		for range ticker.C {
			if err := connect(c); err != nil {
				if retry < 10 {
					log.Printf("%s reconnect failed (%v). Retry...\n",
						c.addr, err)
				} else if retry == 10 {
					log.Printf("%s reconnect failed (%v). Continue retrying but log will be supressed.\n",
						c.addr, err)
				}
				retry++
				continue
			}
			log.Printf("%s reconnected.\n", c.addr)
			return
		}
	}()
}

// TODO return proper error
func (c *ovndb) Close() error {
	c.client.Disconnect()
	return nil
}

func (c *ovndb) EncapList(chname string) ([]*Encap, error) {
	return c.encapListImp(chname)
}

func (c *ovndb) ChassisGet(name string) ([]*Chassis, error) {
	return c.chassisGetImp(name)
}

func (c *ovndb) ChassisAdd(name string, hostname string, etype []string, ip string,
	external_ids map[string]string, transport_zones []string, vtep_lswitches []string) (*OvnCommand, error) {
	return c.chassisAddImp(name, hostname, etype, ip, external_ids, transport_zones, vtep_lswitches)
}

func (c *ovndb) ChassisDel(name string) (*OvnCommand, error) {
	return c.chassisDelImp(name)
}

func (c *ovndb) LSAdd(ls string) (*OvnCommand, error) {
	return c.lsAddImp(ls)
}

func (c *ovndb) LSDel(ls string) (*OvnCommand, error) {
	return c.lsDelImp(ls)
}

func (c *ovndb) LSList() ([]*LogicalSwitch, error) {
	return c.lsListImp()
}

func (c *ovndb) LSExtIdsAdd(ls string, external_ids map[string]string) (*OvnCommand, error) {
	return c.lsExtIdsAddImp(ls, external_ids)
}

func (c *ovndb) LSExtIdsDel(ls string, external_ids map[string]string) (*OvnCommand, error) {
	return c.lsExtIdsDelImp(ls, external_ids)
}

func (c *ovndb) LSPGet(lsp string) (*LogicalSwitchPort, error) {
	return c.lspGetImp(lsp)
}

func (c *ovndb) LSPAdd(ls string, lsp string) (*OvnCommand, error) {
	return c.lspAddImp(ls, lsp)
}

func (c *ovndb) LinkSwitchToRouter(lsw, lsp, lr, lrp, lrpMac string, networks []string, externalIds map[string]string) (*OvnCommand, error) {
	return c.linkSwitchToRouterImp(lsw, lsp, lr, lrp, lrpMac, networks, externalIds)
}

func (c *ovndb) LSPDel(lsp string) (*OvnCommand, error) {
	return c.lspDelImp(lsp)
}

func (c *ovndb) LSPSetAddress(lsp string, addresses ...string) (*OvnCommand, error) {
	return c.lspSetAddressImp(lsp, addresses...)
}

func (c *ovndb) LSPSetPortSecurity(lsp string, security ...string) (*OvnCommand, error) {
	return c.lspSetPortSecurityImp(lsp, security...)
}

func (c *ovndb) LSPSetDHCPv4Options(lsp string, options string) (*OvnCommand, error) {
	return c.lspSetDHCPv4OptionsImp(lsp, options)
}

func (c *ovndb) LSPGetDHCPv4Options(lsp string) (*DHCPOptions, error) {
	return c.lspGetDHCPv4OptionsImp(lsp)
}

func (c *ovndb) LSPSetDHCPv6Options(lsp string, options string) (*OvnCommand, error) {
	return c.lspSetDHCPv6OptionsImp(lsp, options)
}

func (c *ovndb) LSPGetDHCPv6Options(lsp string) (*DHCPOptions, error) {
	return c.lspGetDHCPv6OptionsImp(lsp)
}

func (c *ovndb) LSPSetOptions(lsp string, options map[string]string) (*OvnCommand, error) {
	return c.lspSetOptionsImp(lsp, options)
}

func (c *ovndb) LSPGetOptions(lsp string) (map[string]string, error) {
	return c.lspGetOptionsImp(lsp)
}

func (c *ovndb) LSPSetDynamicAddresses(lsp string, address string) (*OvnCommand, error) {
	return c.lspSetDynamicAddressesImp(lsp, address)
}

func (c *ovndb) LSPGetDynamicAddresses(lsp string) (string, error) {
	return c.lspGetDynamicAddressesImp(lsp)
}

func (c *ovndb) LSPSetExternalIds(lsp string, external_ids map[string]string) (*OvnCommand, error) {
	return c.lspSetExternalIdsImp(lsp, external_ids)
}

func (c *ovndb) LSPGetExternalIds(lsp string) (map[string]string, error) {
	return c.lspGetExternalIdsImp(lsp)
}

func (c *ovndb) LSLBAdd(ls string, lb string) (*OvnCommand, error) {
	return c.lslbAddImp(ls, lb)
}

func (c *ovndb) LSLBDel(ls string, lb string) (*OvnCommand, error) {
	return c.lslbDelImp(ls, lb)
}

func (c *ovndb) LSLBList(ls string) ([]*LoadBalancer, error) {
	return c.lslbListImp(ls)
}

func (c *ovndb) LRAdd(name string, external_ids map[string]string) (*OvnCommand, error) {
	return c.lrAddImp(name, external_ids)
}

func (c *ovndb) LRDel(name string) (*OvnCommand, error) {
	return c.lrDelImp(name)
}

func (c *ovndb) LRList() ([]*LogicalRouter, error) {
	return c.lrListImp()
}

func (c *ovndb) LRPAdd(lr string, lrp string, mac string, network []string, peer string, external_ids map[string]string) (*OvnCommand, error) {
	return c.lrpAddImp(lr, lrp, mac, network, peer, external_ids)
}

func (c *ovndb) LRPDel(lr string, lrp string) (*OvnCommand, error) {
	return c.lrpDelImp(lr, lrp)
}

func (c *ovndb) LRPList(lr string) ([]*LogicalRouterPort, error) {
	return c.lrpListImp(lr)
}

func (c *ovndb) LRSRAdd(lr string, ip_prefix string, nexthop string, output_port []string, policy []string, external_ids map[string]string) (*OvnCommand, error) {
	return c.lrsrAddImp(lr, ip_prefix, nexthop, output_port, policy, external_ids)
}

func (c *ovndb) LRSRDel(lr string, prefix string, nexthop, policy, outputPort *string) (*OvnCommand, error) {
	return c.lrsrDelImp(lr, prefix, nexthop, policy, outputPort)
}

func (c *ovndb) LRSRList(lr string) ([]*LogicalRouterStaticRoute, error) {
	return c.lrsrListImp(lr)
}

func (c *ovndb) LRLBAdd(lr string, lb string) (*OvnCommand, error) {
	return c.lrlbAddImp(lr, lb)
}

func (c *ovndb) LRLBDel(lr string, lb string) (*OvnCommand, error) {
	return c.lrlbDelImp(lr, lb)
}

func (c *ovndb) LRLBList(lr string) ([]*LoadBalancer, error) {
	return c.lrlbListImp(lr)
}

func (c *ovndb) LBAdd(name string, vipPort string, protocol string, addrs []string) (*OvnCommand, error) {
	return c.lbAddImp(name, vipPort, protocol, addrs)
}

func (c *ovndb) LBUpdate(name string, vipPort string, protocol string, addrs []string) (*OvnCommand, error) {
	return c.lbUpdateImp(name, vipPort, protocol, addrs)
}

func (c *ovndb) LBDel(name string) (*OvnCommand, error) {
	return c.lbDelImp(name)
}

func (c *ovndb) ACLAdd(ls, direct, match, action string, priority int, external_ids map[string]string, logflag bool, meter string, severity string) (*OvnCommand, error) {
	return c.aclAddImp(ls, direct, match, action, priority, external_ids, logflag, meter, severity)
}

func (c *ovndb) ACLDel(ls, direct, match string, priority int, external_ids map[string]string) (*OvnCommand, error) {
	return c.aclDelImp(ls, direct, match, priority, external_ids)
}

func (c *ovndb) ASAdd(name string, addrs []string, external_ids map[string]string) (*OvnCommand, error) {
	return c.asAddImp(name, addrs, external_ids)
}

func (c *ovndb) ASDel(name string) (*OvnCommand, error) {
	return c.asDelImp(name)
}

func (c *ovndb) ASUpdate(name string, addrs []string, external_ids map[string]string) (*OvnCommand, error) {
	return c.asUpdateImp(name, addrs, external_ids)
}

func (c *ovndb) QoSAdd(ls string, direction string, priority int, match string, action map[string]int, bandwidth map[string]int, external_ids map[string]string) (*OvnCommand, error) {
	return c.qosAddImp(ls, direction, priority, match, action, bandwidth, external_ids)
}

func (c *ovndb) QoSDel(ls string, direction string, priority int, match string) (*OvnCommand, error) {
	return c.qosDelImp(ls, direction, priority, match)
}

func (c *ovndb) QoSList(ls string) ([]*QoS, error) {
	return c.qosListImp(ls)
}

func (c *ovndb) Execute(cmds ...*OvnCommand) error {
	return c.execute(cmds...)
}

func (c *ovndb) LSGet(ls string) ([]*LogicalSwitch, error) {
	return c.lsGetImp(ls)
}

func (c *ovndb) LSPList(ls string) ([]*LogicalSwitchPort, error) {
	return c.lspListImp(ls)
}

func (c *ovndb) ACLList(ls string) ([]*ACL, error) {
	return c.aclListImp(ls)
}

func (c *ovndb) ASList() ([]*AddressSet, error) {
	return c.asListImp()
}

func (c *ovndb) ASGet(name string) (*AddressSet, error) {
	return c.asGetImp(name)
}

func (c *ovndb) LRGet(name string) ([]*LogicalRouter, error) {
	return c.lrGetImp(name)
}

func (c *ovndb) LBGet(name string) ([]*LoadBalancer, error) {
	return c.lbGetImp(name)
}

func (c *ovndb) DHCPOptionsAdd(cidr string, options map[string]string, external_ids map[string]string) (*OvnCommand, error) {
	return c.dhcpOptionsAddImp(cidr, options, external_ids)
}

func (c *ovndb) DHCPOptionsSet(uuid string, options map[string]string, external_ids map[string]string) (*OvnCommand, error) {
	return c.dhcpOptionsSetImp(uuid, options, external_ids)
}

func (c *ovndb) DHCPOptionsDel(uuid string) (*OvnCommand, error) {
	return c.dhcpOptionsDelImp(uuid)
}

func (c *ovndb) DHCPOptionsGet(uuid string) (*DHCPOptions, error) {
	return c.dhcpOptionsGetImp(uuid)
}

func (c *ovndb) DHCPOptionsList() ([]*DHCPOptions, error) {
	return c.dhcpOptionsListImp()
}

func (c *ovndb) LRNATAdd(lr string, ntype string, externalIp string, logicalIp string, external_ids map[string]string, logicalPortAndExternalMac ...string) (*OvnCommand, error) {
	return c.lrNatAddImp(lr, ntype, externalIp, logicalIp, external_ids, logicalPortAndExternalMac...)
}

func (c *ovndb) LRNATDel(lr string, ntype string, ip ...string) (*OvnCommand, error) {
	return c.lrNatDelImp(lr, ntype, ip...)
}

func (c *ovndb) LRNATList(lr string) ([]*NAT, error) {
	return c.lrNatListImp(lr)
}

func (c *ovndb) MeterAdd(name, action string, rate int, unit string, external_ids map[string]string, burst int) (*OvnCommand, error) {
	return c.meterAddImp(name, action, rate, unit, external_ids, burst)
}

func (c *ovndb) MeterDel(name ...string) (*OvnCommand, error) {
	return c.meterDelImp(name...)
}

func (c *ovndb) MeterList() ([]*Meter, error) {
	return c.meterListImp()
}

func (c *ovndb) MeterBandsList() ([]*MeterBand, error) {
	return c.meterBandsListImp()
}

func (c *ovndb) NBGlobalSetOptions(options map[string]string) (*OvnCommand, error) {
	return c.nbGlobalSetOptionsImp(options)
}

func (c *ovndb) NBGlobalGetOptions() (map[string]string, error) {
	return c.nbGlobalGetOptionsImp()
}

func (c *ovndb) SBGlobalSetOptions(options map[string]string) (*OvnCommand, error) {
	return c.sbGlobalSetOptionsImp(options)
}

func (c *ovndb) SBGlobalGetOptions() (map[string]string, error) {
	return c.sbGlobalGetOptionsImp()
}

// these functions are helpers for unit-tests, but not part of the API

func (c *ovndb) nbGlobalAdd(options map[string]string) (*OvnCommand, error) {
	return c.nbGlobalAddImp(options)
}

func (c *ovndb) nbGlobalDel() (*OvnCommand, error) {
	return c.nbGlobalDelImp()
}

func (c *ovndb) sbGlobalAdd(options map[string]string) (*OvnCommand, error) {
	return c.sbGlobalAddImp(options)
}

func (c *ovndb) sbGlobalDel() (*OvnCommand, error) {
	return c.sbGlobalDelImp()
}
