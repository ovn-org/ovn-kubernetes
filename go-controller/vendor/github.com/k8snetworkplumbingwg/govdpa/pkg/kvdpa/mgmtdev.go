package kvdpa

import (
	"strings"
	"syscall"

	"github.com/vishvananda/netlink/nl"
)

// MgmtDev represents a Vdpa Management Device
type MgmtDev interface {
	BusName() string // Optional
	DevName() string //
	Name() string    // The MgmtDevName is BusName/DevName
}

type mgmtDev struct {
	busName string
	devName string
}

// BusName returns the MgmtDev's bus name
func (m *mgmtDev) BusName() string {
	return m.busName
}

// BusName returns the MgmtDev's device name
func (m *mgmtDev) DevName() string {
	return m.devName
}

// BusName returns the MgmtDev's name: [BusName/]DeviceName
func (m *mgmtDev) Name() string {
	if m.busName != "" {
		return strings.Join([]string{m.busName, m.devName}, "/")
	}
	return m.devName
}

// parseAttributes parses the netlink attributes and populates the fields accordingly
func (m *mgmtDev) parseAttributes(attrs []syscall.NetlinkRouteAttr) error {
	for _, a := range attrs {
		switch a.Attr.Type {
		case VdpaAttrMgmtDevBusName:
			m.busName = string(a.Value[:len(a.Value)-1])
		case VdpaAttrMgmtDevDevName:
			m.devName = string(a.Value[:len(a.Value)-1])
		}
	}
	return nil
}

// ListVdpaMgmtDevices returns the list of all available MgmtDevs
func ListVdpaMgmtDevices() ([]MgmtDev, error) {
	msgs, err := GetNetlinkOps().RunVdpaNetlinkCmd(VdpaCmdMgmtDevGet, syscall.NLM_F_DUMP, nil)
	if err != nil {
		return nil, err
	}

	mgtmDevs, err := parseDevLinkVdpaMgmtDevList(msgs)
	if err != nil {
		return nil, err
	}
	return mgtmDevs, nil
}

// GetVdpaMgmtDevices returns a MgmtDev based on a busName and deviceName
func GetVdpaMgmtDevices(busName, devName string) (MgmtDev, error) {
	data := []*nl.RtAttr{}
	if busName != "" {
		bus, err := GetNetlinkOps().NewAttribute(VdpaAttrMgmtDevBusName, busName)
		if err != nil {
			return nil, err
		}
		data = append(data, bus)
	}

	dev, err := GetNetlinkOps().NewAttribute(VdpaAttrMgmtDevDevName, devName)
	if err != nil {
		return nil, err
	}
	data = append(data, dev)

	msgs, err := GetNetlinkOps().RunVdpaNetlinkCmd(VdpaCmdMgmtDevGet, 0, data)
	if err != nil {
		return nil, err
	}

	mgtmDevs, err := parseDevLinkVdpaMgmtDevList(msgs)
	if err != nil {
		return nil, err
	}
	return mgtmDevs[0], nil
}

func parseDevLinkVdpaMgmtDevList(msgs [][]byte) ([]MgmtDev, error) {
	devices := make([]MgmtDev, 0, len(msgs))

	for _, m := range msgs {
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofGenlmsg:])
		if err != nil {
			return nil, err
		}
		dev := &mgmtDev{}
		if err = dev.parseAttributes(attrs); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, nil
}
