package acl

import (
	"fmt"
	"testing"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
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
