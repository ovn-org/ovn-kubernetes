package acl

import (
	"fmt"
	"reflect"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/core/v1"
)

func TestGetACLByName(t *testing.T) {
	tests := []struct {
		name    string
		aclName string
		ovnCmd  ovntest.ExpectedCmd
		want    string
		wantErr bool
	}{
		{
			name:    "existing acl",
			aclName: "my-acl",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=my-acl",
				Output: "a08ea426-2288-11eb-a30b-a8a1590cda29",
			},
			want:    "a08ea426-2288-11eb-a30b-a8a1590cda29",
			wantErr: false,
		},
		{
			name:    "non existing acl",
			aclName: "my-acl",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find acl name=my-acl",
				Output: "",
			},
			want:    "",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			got, err := GetACLByName(tt.aclName)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetACLByName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GetACLByName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRemoveACLFromNodeSwitches(t *testing.T) {
	tests := []struct {
		name     string
		switches []string
		aclUUID  string
		ovnCmd   ovntest.ExpectedCmd
		wantErr  bool
	}{
		{
			name:     "remove acl on two switches",
			switches: []string{"sw1", "sw2"},
			aclUUID:  "a08ea426-2288-11eb-a30b-a8a1590cda29",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 -- --if-exists remove logical_switch sw1 acl a08ea426-2288-11eb-a30b-a8a1590cda29 -- --if-exists remove logical_switch sw2 acl a08ea426-2288-11eb-a30b-a8a1590cda29",
				Output: "",
			},
			wantErr: false,
		},
		{
			name:     "remove acl on no switches",
			switches: []string{},
			aclUUID:  "a08ea426-2288-11eb-a30b-a8a1590cda29",
			ovnCmd:   ovntest.ExpectedCmd{},
			wantErr:  false,
		},
		{
			name:     "remove acl and OVN error on first switch",
			switches: []string{"sw1", "sw2"},
			aclUUID:  "a08ea426-2288-11eb-a30b-a8a1590cda29",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 -- --if-exists remove logical_switch sw1 acl a08ea426-2288-11eb-a30b-a8a1590cda29 -- --if-exists remove logical_switch sw2 acl a08ea426-2288-11eb-a30b-a8a1590cda29",
				Output: "",
				Err:    fmt.Errorf("error while removing ACL: sw1, from switches"),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			err = RemoveACLFromNodeSwitches(tt.switches, tt.aclUUID)
			if (err != nil) != tt.wantErr {
				t.Errorf("RemoveACLFromNodeSwitches() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}

func TestRemoveACLFromPortGroup(t *testing.T) {
	tests := []struct {
		name                 string
		clusterPortGroupUUID string
		aclUUID              string
		ovnCmd               ovntest.ExpectedCmd
		wantErr              bool
	}{
		{
			name:                 "remove acl from portgroup",
			clusterPortGroupUUID: "63c3e636-387d-11eb-ac78-a8a1590cda29",
			aclUUID:              "a08ea426-2288-11eb-a30b-a8a1590cda29",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 -- --if-exists remove port_group 63c3e636-387d-11eb-ac78-a8a1590cda29 acls a08ea426-2288-11eb-a30b-a8a1590cda29",
				Output: "",
			},
			wantErr: false,
		},

		// TODO: Add test cases for errors
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			err = RemoveACLFromPortGroup(tt.aclUUID, tt.clusterPortGroupUUID)
			if (err != nil) != tt.wantErr {
				t.Errorf("RemoveACLFromPortGroup() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}

func TestGetRejectACLs(t *testing.T) {
	tests := []struct {
		name    string
		ovnCmd  ovntest.ExpectedCmd
		want    map[string]string
		wantErr bool
	}{
		{
			name: "no reject ACLs in OVN",
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    "ovn-nbctl --timeout=15 --columns=name,_uuid --format=json find acl action=reject",
				Output: `{"data":[],"headings":["name","_uuid"]}`,
			},
			want:    make(map[string]string),
			wantErr: false,
		},

		// TODO: Add test cases with some data
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			got, err := GetRejectACLs()
			if (err != nil) != tt.wantErr {
				t.Errorf("GetRejectACLs() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetRejectACLs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAddRejectACLToLogicalSwitch(t *testing.T) {
	tests := []struct {
		name          string
		logicalSwitch string
		aclName       string
		sourceIP      string
		sourcePort    int
		proto         v1.Protocol
		ovnCmd        ovntest.ExpectedCmd
		want          string
		wantErr       bool
	}{
		{
			name:          "add ipv4 acl",
			logicalSwitch: "545dc436-387e-11eb-9f38-a8a1590cda29",
			aclName:       "myacl",
			sourceIP:      "192.168.2.2",
			sourcePort:    80,
			proto:         v1.ProtocolTCP,
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip4.dst==192.168.2.2 && tcp && tcp.dst==80" action=reject name=myacl -- add logical_switch 545dc436-387e-11eb-9f38-a8a1590cda29 acls @reject-acl`,
				Output: "97347886-387e-11eb-9fdf-a8a1590cda29",
			},
			want:    "97347886-387e-11eb-9fdf-a8a1590cda29",
			wantErr: false,
		},
		{
			name:          "add ipv6 acl",
			logicalSwitch: "545dc436-387e-11eb-9f38-a8a1590cda29",
			aclName:       "myacl",
			sourceIP:      "2001:db2:1:2::23",
			sourcePort:    80,
			proto:         v1.ProtocolTCP,
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip6.dst==2001:db2:1:2::23 && tcp && tcp.dst==80" action=reject name=myacl -- add logical_switch 545dc436-387e-11eb-9f38-a8a1590cda29 acls @reject-acl`,
				Output: "97347886-387e-11eb-9fdf-a8a1590cda29",
			},
			want:    "97347886-387e-11eb-9fdf-a8a1590cda29",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			got, err := AddRejectACLToLogicalSwitch(tt.logicalSwitch, tt.aclName, tt.sourceIP, tt.sourcePort, tt.proto)
			if (err != nil) != tt.wantErr {
				t.Errorf("AddRejectACLToLogicalSwitch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("AddRejectACLToLogicalSwitch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAddRejectACLToPortGroup(t *testing.T) {
	tests := []struct {
		name       string
		portGroup  string
		aclName    string
		sourceIP   string
		sourcePort int
		proto      v1.Protocol
		ovnCmd     ovntest.ExpectedCmd
		want       string
		wantErr    bool
	}{
		{
			name:       "add ipv4 acl",
			portGroup:  "545dc436-387e-11eb-9f38-a8a1590cda29",
			aclName:    "myacl",
			sourceIP:   "192.168.2.2",
			sourcePort: 80,
			proto:      v1.ProtocolTCP,
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip4.dst==192.168.2.2 && tcp && tcp.dst==80" action=reject name=myacl -- add port_group 545dc436-387e-11eb-9f38-a8a1590cda29 acls @reject-acl`,
				Output: "97347886-387e-11eb-9fdf-a8a1590cda29",
			},
			want:    "97347886-387e-11eb-9fdf-a8a1590cda29",
			wantErr: false,
		},
		{
			name:       "add ipv6 acl",
			portGroup:  "545dc436-387e-11eb-9f38-a8a1590cda29",
			aclName:    "myacl",
			sourceIP:   "2001:db2:1:2::23",
			sourcePort: 80,
			proto:      v1.ProtocolTCP,
			ovnCmd: ovntest.ExpectedCmd{
				Cmd:    `ovn-nbctl --timeout=15 --id=@reject-acl create acl direction=from-lport priority=1000 match="ip6.dst==2001:db2:1:2::23 && tcp && tcp.dst==80" action=reject name=myacl -- add port_group 545dc436-387e-11eb-9f38-a8a1590cda29 acls @reject-acl`,
				Output: "97347886-387e-11eb-9fdf-a8a1590cda29",
			},
			want:    "97347886-387e-11eb-9fdf-a8a1590cda29",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fexec := ovntest.NewLooseCompareFakeExec()
			fexec.AddFakeCmd(&tt.ovnCmd)
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}

			got, err := AddRejectACLToPortGroup(tt.portGroup, tt.aclName, tt.sourceIP, tt.sourcePort, tt.proto)
			if (err != nil) != tt.wantErr {
				t.Errorf("AddRejectACLToPortGroup() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("AddRejectACLToPortGroup() = %v, want %v", got, tt.want)
			}
		})
	}
}
