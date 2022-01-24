package client

import (
	"encoding/json"
	"testing"

	"github.com/ovn-org/libovsdb/cache"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"
	"github.com/stretchr/testify/assert"
)

var apiTestSchema = []byte(`{
    "name": "OVN_Northbound",
    "version": "5.31.0",
    "cksum": "2352750632 28701",
    "tables": {
        "Logical_Switch": {
            "columns": {
                "name": {"type": "string"},
                "ports": {"type": {"key": {"type": "uuid",
                                           "refTable": "Logical_Switch_Port",
                                           "refType": "strong"},
                                   "min": 0,
                                   "max": "unlimited"}},
                "acls": {"type": {"key": {"type": "uuid",
                                          "refTable": "ACL",
                                          "refType": "strong"},
                                  "min": 0,
                                  "max": "unlimited"}},
                "qos_rules": {"type": {"key": {"type": "uuid",
                                          "refTable": "QoS",
                                          "refType": "strong"},
                                  "min": 0,
                                  "max": "unlimited"}},
                "load_balancer": {"type": {"key": {"type": "uuid",
                                                  "refTable": "Load_Balancer",
                                                  "refType": "weak"},
                                           "min": 0,
                                           "max": "unlimited"}},
                "dns_records": {"type": {"key": {"type": "uuid",
                                         "refTable": "DNS",
                                         "refType": "weak"},
                                  "min": 0,
                                  "max": "unlimited"}},
                "other_config": {
                    "type": {"key": "string", "value": "string",
                             "min": 0, "max": "unlimited"}},
                "external_ids": {
                    "type": {"key": "string", "value": "string",
                             "min": 0, "max": "unlimited"}},
               "forwarding_groups": {
                    "type": {"key": {"type": "uuid",
                                     "refTable": "Forwarding_Group",
                                     "refType": "strong"},
                                     "min": 0, "max": "unlimited"}}},
            "isRoot": true},
        "Logical_Switch_Port": {
            "columns": {
                "name": {"type": "string"},
                "type": {"type": "string"},
                "options": {
                     "type": {"key": "string",
                              "value": "string",
                              "min": 0,
                              "max": "unlimited"}},
                "parent_name": {"type": {"key": "string", "min": 0, "max": 1}},
                "tag_request": {
                     "type": {"key": {"type": "integer",
                                      "minInteger": 0,
                                      "maxInteger": 4095},
                              "min": 0, "max": 1}},
                "tag": {
                     "type": {"key": {"type": "integer",
                                      "minInteger": 1,
                                      "maxInteger": 4095},
                              "min": 0, "max": 1}},
                "addresses": {"type": {"key": "string",
                                       "min": 0,
                                       "max": "unlimited"}},
                "dynamic_addresses": {"type": {"key": "string",
                                       "min": 0,
                                       "max": 1}},
                "port_security": {"type": {"key": "string",
                                           "min": 0,
                                           "max": "unlimited"}},
                "up": {"type": {"key": "boolean", "min": 0, "max": 1}},
                "enabled": {"type": {"key": "boolean", "min": 0, "max": 1}},
                "dhcpv4_options": {"type": {"key": {"type": "uuid",
                                            "refTable": "DHCP_Options",
                                            "refType": "weak"},
                                 "min": 0,
                                 "max": 1}},
                "dhcpv6_options": {"type": {"key": {"type": "uuid",
                                            "refTable": "DHCP_Options",
                                            "refType": "weak"},
                                 "min": 0,
                                 "max": 1}},
                "ha_chassis_group": {
                    "type": {"key": {"type": "uuid",
                                     "refTable": "HA_Chassis_Group",
                                     "refType": "strong"},
                             "min": 0,
                             "max": 1}},
                "external_ids": {
                    "type": {"key": "string", "value": "string",
                             "min": 0, "max": "unlimited"}}},
            "indexes": [["name"]],
            "isRoot": false}
	}
    }`)

type testLogicalSwitch struct {
	UUID             string            `ovsdb:"_uuid"`
	Ports            []string          `ovsdb:"ports"`
	ExternalIds      map[string]string `ovsdb:"external_ids"`
	Name             string            `ovsdb:"name"`
	QosRules         []string          `ovsdb:"qos_rules"`
	LoadBalancer     []string          `ovsdb:"load_balancer"`
	DNSRecords       []string          `ovsdb:"dns_records"`
	OtherConfig      map[string]string `ovsdb:"other_config"`
	ForwardingGroups []string          `ovsdb:"forwarding_groups"`
	Acls             []string          `ovsdb:"acls"`
}

// Table returns the table name. It's part of the Model interface
func (*testLogicalSwitch) Table() string {
	return "Logical_Switch"
}

//LogicalSwitchPort struct defines an object in Logical_Switch_Port table
type testLogicalSwitchPort struct {
	UUID             string            `ovsdb:"_uuid"`
	Up               *bool             `ovsdb:"up"`
	Dhcpv4Options    *string           `ovsdb:"dhcpv4_options"`
	Name             string            `ovsdb:"name"`
	DynamicAddresses *string           `ovsdb:"dynamic_addresses"`
	HaChassisGroup   *string           `ovsdb:"ha_chassis_group"`
	Options          map[string]string `ovsdb:"options"`
	Enabled          *bool             `ovsdb:"enabled"`
	Addresses        []string          `ovsdb:"addresses"`
	Dhcpv6Options    *string           `ovsdb:"dhcpv6_options"`
	TagRequest       *int              `ovsdb:"tag_request"`
	Tag              *int              `ovsdb:"tag"`
	PortSecurity     []string          `ovsdb:"port_security"`
	ExternalIds      map[string]string `ovsdb:"external_ids"`
	Type             string            `ovsdb:"type"`
	ParentName       *string           `ovsdb:"parent_name"`
}

// Table returns the table name. It's part of the Model interface
func (*testLogicalSwitchPort) Table() string {
	return "Logical_Switch_Port"
}

func apiTestCache(t testing.TB, data map[string]map[string]model.Model) *cache.TableCache {
	var schema ovsdb.DatabaseSchema
	err := json.Unmarshal(apiTestSchema, &schema)
	assert.Nil(t, err)
	db, err := model.NewClientDBModel("OVN_Northbound", map[string]model.Model{"Logical_Switch": &testLogicalSwitch{}, "Logical_Switch_Port": &testLogicalSwitchPort{}})
	assert.Nil(t, err)
	dbModel, errs := model.NewDatabaseModel(schema, db)
	assert.Empty(t, errs)
	cache, err := cache.NewTableCache(dbModel, data, nil)
	assert.Nil(t, err)
	return cache
}
