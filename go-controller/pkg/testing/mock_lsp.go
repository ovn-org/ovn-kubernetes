package testing

import (
	"fmt"

	goovn "github.com/ebay/go-ovn"
	"k8s.io/klog"
)

const (
	LogicalSwitchPortAddresses        string = "LSPAddressesField"
	LogicalSwitchPortOptions          string = "LSPOptionsField"
	LogicalSwitchPortDynamicAddresses string = "LSPDynamicAddressesField"
	LogicalSwitchPortExternalId       string = "LSPExternalIdsField"
	LogicalSwitchPortPortSecurity     string = "LSPPortSecurityField"
	FakeUUID                                 = "8a86f6d8-7972-4253-b0bd-ddbef66e9303"
)

// Get logical switch port by name
func (mock *MockOVNClient) LSPGet(lsp string) (*goovn.LogicalSwitchPort, error) {
	var lspCache MockObjectCacheByName
	var ok bool
	if lspCache, ok = mock.cache[LogicalSwitchPortType]; !ok {
		klog.V(5).Infof("Cache doesn't have any object of type %s", LogicalSwitchPortType)
		return nil, goovn.ErrorSchema
	}
	var port interface{}
	if port, ok = lspCache[lsp]; !ok {
		return nil, goovn.ErrorNotFound
	}
	if lspRet, ok := port.(*goovn.LogicalSwitchPort); ok {
		return lspRet, nil
	}
	return nil, fmt.Errorf("invalid object type assertion for %s", LogicalSwitchPortType)
}

// Add logical port PORT on SWITCH
func (mock *MockOVNClient) LSPAdd(ls string, lsp string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("Adding lsp %s to switch %s", lsp, ls)
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpAdd,
			table:   LogicalSwitchPortType,
			objName: lsp,
			obj:     &goovn.LogicalSwitchPort{Name: lsp, UUID: FakeUUID},
		},
	}, nil
}

// Delete PORT from its attached switch
func (mock *MockOVNClient) LSPDel(lsp string) (*goovn.OvnCommand, error) {
	klog.V(5).Infof("Deleting lsp %s", lsp)
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpDelete,
			table:   LogicalSwitchPortType,
			objName: lsp,
		},
	}, nil
}

// Set addresses per lport
func (mock *MockOVNClient) LSPSetAddress(lsp string, addresses ...string) (*goovn.OvnCommand, error) {
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpUpdate,
			table:   LogicalSwitchPortType,
			objName: lsp,
			objUpdate: UpdateCache{
				FieldType:  LogicalSwitchPortAddresses,
				FieldValue: addresses,
			},
		},
	}, nil
}

// Set port security per lport
func (mock *MockOVNClient) LSPSetPortSecurity(lsp string, security ...string) (*goovn.OvnCommand, error) {
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpUpdate,
			table:   LogicalSwitchPortType,
			objName: lsp,
			objUpdate: UpdateCache{
				FieldType:  LogicalSwitchPortPortSecurity,
				FieldValue: security,
			},
		},
	}, nil
}

// Get all lport by lswitch
func (mock *MockOVNClient) LSPList(ls string) ([]*goovn.LogicalSwitchPort, error) {
	return nil, fmt.Errorf("method %s is not implemented yet", functionName())
}

// Set dhcp4_options uuid on lsp
func (mock *MockOVNClient) LSPSetDHCPv4Options(lsp string, options string) (*goovn.OvnCommand, error) {
	return nil, fmt.Errorf("method %s is not implemented yet", functionName())
}

// Get dhcp4_options from lsp
func (mock *MockOVNClient) LSPGetDHCPv4Options(lsp string) (*goovn.DHCPOptions, error) {
	return nil, fmt.Errorf("method %s is not implemented yet", functionName())
}

// Set dhcp6_options uuid on lsp
func (mock *MockOVNClient) LSPSetDHCPv6Options(lsp string, options string) (*goovn.OvnCommand, error) {
	return nil, fmt.Errorf("method %s is not implemented yet", functionName())
}

// Get dhcp6_options from lsp
func (mock *MockOVNClient) LSPGetDHCPv6Options(lsp string) (*goovn.DHCPOptions, error) {
	return nil, fmt.Errorf("method %s is not implemented yet", functionName())
}

// Set options in LSP
func (mock *MockOVNClient) LSPSetOptions(lsp string, options map[string]string) (*goovn.OvnCommand, error) {
	return nil, fmt.Errorf("method %s is not implemented yet", functionName())
}

// Get Options for LSP
func (mock *MockOVNClient) LSPGetOptions(lsp string) (map[string]string, error) {
	return nil, fmt.Errorf("method %s is not implemented yet", functionName())
}

// Set dynamic addresses in LSP
func (mock *MockOVNClient) LSPSetDynamicAddresses(lsp string, address string) (*goovn.OvnCommand, error) {
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpUpdate,
			table:   LogicalSwitchPortType,
			objName: lsp,
			objUpdate: UpdateCache{
				FieldType:  LogicalSwitchPortDynamicAddresses,
				FieldValue: address,
			},
		},
	}, nil

}

// Get dynamic addresses from LSP
func (mock *MockOVNClient) LSPGetDynamicAddresses(lsp string) (string, error) {
	lspRet, err := mock.LSPGet(lsp)
	if err != nil {
		return "", err
	}
	if lspRet != nil {
		return "", fmt.Errorf("no lsp found with name: %s", lsp)
	}
	return lspRet.DynamicAddresses, nil
}

// Set external_ids for LSP
func (mock *MockOVNClient) LSPSetExternalIds(lsp string, external_ids map[string]string) (*goovn.OvnCommand, error) {
	return &goovn.OvnCommand{
		Exe: &MockExecution{
			handler: mock,
			op:      OpUpdate,
			table:   LogicalSwitchPortType,
			objName: lsp,
			objUpdate: UpdateCache{
				FieldType:  LogicalSwitchPortExternalId,
				FieldValue: external_ids,
			},
		},
	}, nil
}

// Get external_ids from LSP
func (mock *MockOVNClient) LSPGetExternalIds(lsp string) (map[string]string, error) {
	lspRet, err := mock.LSPGet(lsp)
	if err != nil {
		return nil, err
	}
	if lspRet != nil {
		return nil, fmt.Errorf("no lsp found with name: %s", lsp)
	}
	extIds := make(map[string]string)
	for k, v := range lspRet.ExternalID {
		key, keyOk := k.(string)
		value, valueOk := v.(string)
		if !keyOk || !valueOk {
			continue
		}
		extIds[key] = value
	}
	return extIds, nil
}

// helper function that applies field updates for a given lsp to the mock object cache
func (mock *MockOVNClient) updateLSPCache(lspName string, update UpdateCache, mockCache MockObjectCacheByName) error {
	var entry interface{}
	var lsp *goovn.LogicalSwitchPort
	var ok bool

	if entry, ok = mockCache[lspName]; !ok {
		return fmt.Errorf("error updating LSP with name %s, LSP doesn't exist", lspName)
	}

	if lsp, ok = entry.(*goovn.LogicalSwitchPort); !ok {
		panic("type assertion failed for LSP cache entry")
	}

	switch update.FieldType {
	case LogicalSwitchPortAddresses:
		klog.V(5).Infof("Setting addresses for LSP %s", lspName)
		if addresses, ok := update.FieldValue.([]string); ok {
			lsp.Addresses = addresses
		} else {
			return fmt.Errorf("type assertion failed for LSP field: %s", update.FieldType)
		}
	case LogicalSwitchPortDynamicAddresses:
		if dynaddr, ok := update.FieldValue.(string); ok {
			klog.V(5).Infof("Setting dynamic address for LSP %s to %s", lspName, dynaddr)
			lsp.DynamicAddresses = dynaddr
		} else {
			return fmt.Errorf("type assertion failed for LSP field: %s", update.FieldType)
		}
	case LogicalSwitchPortPortSecurity:
		klog.V(5).Infof("Setting port security for LSP %s", lsp)
		if addresses, ok := update.FieldValue.([]string); ok {
			lsp.PortSecurity = addresses
		} else {
			return fmt.Errorf("type assertion failed for LSP field: %s", update.FieldType)
		}
	case LogicalSwitchPortExternalId:
		klog.V(5).Infof("Setting external id for LSP %s", lspName)
		if extIds, ok := update.FieldValue.(map[string]string); ok {
			extMap := make(map[interface{}]interface{})
			for k, v := range extIds {
				extMap[k] = v
			}
			lsp.ExternalID = extMap
		} else {
			return fmt.Errorf("type assertion failed for LSP field: %s", update.FieldType)
		}
	default:
		return fmt.Errorf("unrecognized field type: %s", update.FieldType)
	}

	return nil
}
