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
	TableNBGlobal                 string = "NB_Global"
	TableLogicalSwitch            string = "Logical_Switch"
	TableLogicalSwitchPort        string = "Logical_Switch_Port"
	TableAddressSet               string = "Address_Set"
	TablePortGroup                string = "Port_Group"
	TableLoadBalancer             string = "Load_Balancer"
	TableACL                      string = "ACL"
	TableLogicalRouter            string = "Logical_Router"
	TableQoS                      string = "QoS"
	TableMeter                    string = "Meter"
	TableMeterBand                string = "Meter_Band"
	TableLogicalRouterPort        string = "Logical_Router_Port"
	TableLogicalRouterStaticRoute string = "Logical_Router_Static_Route"
	TableNAT                      string = "NAT"
	TableDHCPOptions              string = "DHCP_Options"
	TableConnection               string = "Connection"
	TableDNS                      string = "DNS"
	TableSSL                      string = "SSL"
	TableGatewayChassis           string = "Gateway_Chassis"
	TableChassis                  string = "Chassis"
	TableEncap                    string = "Encap"
	TableSBGlobal                 string = "SB_Global"
	TableChassisPrivate           string = "Chassis_Private"
)

var NBTablesOrder = []string{
	TableNBGlobal,
	TableAddressSet,
	TableACL,
	TableDHCPOptions,
	TableLoadBalancer,
	TableQoS,
	TableMeter,
	TableMeterBand,
	TableLogicalRouterPort,
	TableLogicalRouterStaticRoute,
	TableLogicalSwitchPort,
	TableNAT,
	TableConnection,
	TableDNS,
	TableSSL,
	TableGatewayChassis,
	TablePortGroup,
	TableLogicalSwitch,
	TableLogicalRouter,
}

var SBTablesOrder = []string{
	TableChassis,
	TableChassisPrivate,
	TableEncap,
	TableSBGlobal,
}
