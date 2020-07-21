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
	"github.com/ebay/libovsdb"
)

// OvnCommand ovnnb command
type OvnCommand struct {
	Operations []libovsdb.Operation
	Exe        Execution
	Results    [][]map[string]interface{}
}

// Execute sends command to ovnnb
func (ocmd *OvnCommand) Execute() error {
	return ocmd.Exe.Execute(ocmd)
}

// Execution executes multiple ovnnb commands
type Execution interface {
	//Excute multi-commands
	Execute(cmds ...*OvnCommand) error
}

// OVNDisconnectedCallback executed when ovn client disconnects
type OVNDisconnectedCallback func()

// OVNSignal notifies on changes to ovnnb
type OVNSignal interface {
	OnLogicalSwitchCreate(ls *LogicalSwitch)
	OnLogicalSwitchDelete(ls *LogicalSwitch)

	OnLogicalPortCreate(lp *LogicalSwitchPort)
	OnLogicalPortDelete(lp *LogicalSwitchPort)

	OnLogicalRouterCreate(lr *LogicalRouter)
	OnLogicalRouterDelete(lr *LogicalRouter)

	OnLogicalRouterPortCreate(lrp *LogicalRouterPort)
	OnLogicalRouterPortDelete(lrp *LogicalRouterPort)

	OnLogicalRouterStaticRouteCreate(lrsr *LogicalRouterStaticRoute)
	OnLogicalRouterStaticRouteDelete(lrsr *LogicalRouterStaticRoute)

	OnACLCreate(acl *ACL)
	OnACLDelete(acl *ACL)

	OnDHCPOptionsCreate(dhcp *DHCPOptions)
	OnDHCPOptionsDelete(dhcp *DHCPOptions)

	OnQoSCreate(qos *QoS)
	OnQoSDelete(qos *QoS)

	OnLoadBalancerCreate(ls *LoadBalancer)
	OnLoadBalancerDelete(ls *LoadBalancer)

	onMeterCreate(meter *Meter)
	onMeterDelete(meter *Meter)

	onMeterBandCreate(band *MeterBand)
	onMeterBandDelete(band *MeterBand)

	// Create/delete chassis from south bound db
	onChassisCreate(ch *Chassis)
	onChassisDelete(ch *Chassis)

	// Create/delete encap from south bound db
	onEncapCreate(ch *Encap)
	onEncapDelete(ch *Encap)
}

// OVNNotifier ovnnb and ovnsb notifier
type OVNNotifier interface {
	Update(context interface{}, tableUpdates libovsdb.TableUpdates)
	Locked([]interface{})
	Stolen([]interface{})
	Echo([]interface{})
	Disconnected(client *libovsdb.OvsdbClient)
}
