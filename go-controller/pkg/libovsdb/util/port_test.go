package util

import (
	"fmt"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/stretchr/testify/assert"
	utilpointer "k8s.io/utils/pointer"
)

const (
	hwAddr    string = "06:c6:d4:fb:fb:ba"
	badHWAddr string = "NotMAC"
	badIPAddr string = "NOTIP"
	ipAddr    string = "10.244.2.2"
	portName  string = "test-pod"
)

func TestExtractPortAddresses(t *testing.T) {
	tests := []struct {
		desc       string
		lsp        *nbdb.LogicalSwitchPort
		errMatch   error
		isNotFound bool
		hasNoIP    bool
	}{
		{
			desc: "test path where lsp.DynamicAddresses is a zero length string and len(addresses)==0",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             "test-pod",
				DynamicAddresses: utilpointer.String(hwAddr + " " + ipAddr),
			},
		},
		{
			desc: "test path where lsp.DynamicAddresses is non-zero length string and value of first address in addresses list is set to dynamic",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             portName,
				DynamicAddresses: utilpointer.String(hwAddr + " " + ipAddr),
				Addresses:        []string{"dynamic"},
			},
		},
		{
			desc: "test code path where port has MAC but no IPs",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             "test-pod",
				DynamicAddresses: utilpointer.String(hwAddr),
			},
			hasNoIP: true,
		},
		{
			desc: "test the code path where ParseMAC fails",
			lsp: &nbdb.LogicalSwitchPort{
				Name:             portName,
				DynamicAddresses: utilpointer.String(badHWAddr),
			},
			errMatch: fmt.Errorf("failed to parse logical switch port \"%s\" MAC \"%s\": address %s: invalid MAC address", portName, badHWAddr, badHWAddr),
		},
		{
			desc: "test code path where IP address parsing fails",
			lsp: &nbdb.LogicalSwitchPort{
				Name:      portName,
				Addresses: []string{fmt.Sprintf("%s %s", hwAddr, badIPAddr)},
			},
			errMatch: fmt.Errorf("failed to parse logical switch port \"%s\" IP \"%s\" is not a valid ip address", portName, badIPAddr),
		},
		{
			desc: "test success path with len(lsp.Addresses) > 0 and lsp.DynamicAddresses = nil",
			lsp: &nbdb.LogicalSwitchPort{
				Name:      portName,
				Addresses: []string{fmt.Sprintf("%s %s", hwAddr, ipAddr)},
			},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			hardwareAddr, ips, err := ExtractPortAddresses(tc.lsp)
			if tc.isNotFound {
				assert.Nil(t, hardwareAddr)
				assert.Nil(t, ips)
				assert.Nil(t, err)

			} else if tc.hasNoIP {
				assert.Equal(t, hardwareAddr.String(), hwAddr)
				assert.Nil(t, ips)
			} else if tc.errMatch != nil {
				assert.Equal(t, err, tc.errMatch)
			} else {
				assert.Equal(t, hardwareAddr.String(), hwAddr)
				assert.Equal(t, len(ips), 1)
				assert.Equal(t, ips[0].String(), ipAddr)
			}
		})
	}
}
