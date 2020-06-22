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

const (
	opInsert string = "insert"
	opMutate string = "mutate"
	opDelete string = "delete"
	opSelect string = "select"
	opUpdate string = "update"
)

const (
	DBNB string = "OVN_Northbound"
	DBSB string = "OVN_Southbound"
)

const (
	tableNBGlobal                 string = "NB_Global"
	tableLogicalSwitch            string = "Logical_Switch"
	tableLogicalSwitchPort        string = "Logical_Switch_Port"
	tableAddressSet               string = "Address_Set"
	tablePortGroup                string = "Port_Group"
	tableLoadBalancer             string = "Load_Balancer"
	tableACL                      string = "ACL"
	tableLogicalRouter            string = "Logical_Router"
	tableQoS                      string = "QoS"
	tableMeter                    string = "Meter"
	tableMeterBand                string = "Meter_Band"
	tableLogicalRouterPort        string = "Logical_Router_Port"
	tableLogicalRouterStaticRoute string = "Logical_Router_Static_Route"
	tableNAT                      string = "NAT"
	tableDHCPOptions              string = "DHCP_Options"
	tableConnection               string = "Connection"
	tableDNS                      string = "DNS"
	tableSSL                      string = "SSL"
	tableGatewayChassis           string = "Gateway_Chassis"
	tableChassis                  string = "Chassis"
	tableEncap                    string = "Encap"
	tableSBGlobal                 string = "SB_Global"
)

var tablesOrder = []string{
	tableNBGlobal,
	tableAddressSet,
	tableACL,
	tableDHCPOptions,
	tableLoadBalancer,
	tableQoS,
	tableMeter,
	tableMeterBand,
	tableLogicalRouterPort,
	tableLogicalRouterStaticRoute,
	tableLogicalSwitchPort,
	tableNAT,
	tableConnection,
	tableDNS,
	tableSSL,
	tableGatewayChassis,
	tablePortGroup,
	tableLogicalSwitch,
	tableLogicalRouter,
	tableChassis,
	tableEncap,
	tableSBGlobal,
}
