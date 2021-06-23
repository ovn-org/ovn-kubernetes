package types

import (
	"github.com/containernetworking/cni/pkg/types"
)

// NetConf is CNI NetConf with DeviceID
type NetConf struct {
	types.NetConf
	// PciAddrs in case of using sriov
	DeviceID string `json:"deviceID,omitempty"`
	// Network Cidr
	NetCidr string `json:"netCidr,omitempty"`
	// Network MTU
	MTU int `json:"mtu,omitempty"`
	// set to localnet if it needs public interface
	TopoType string `json:"topology,omitempty"`
	// captures net-attach-def name in the form of namespace/name
	NadName string `json:"netAttachDefName,omitempty"`
	// set to true if it is a secondary networkattachmentdefintion
	IsSecondary bool `json:"isSecondary,omitempty"`

	// VlanID, valid in localnet topology network
	VlanId int `json:"vlanId,omitempty"`
	// list of IPs, expressed with prefix length, to be excluded from being allocated for Pod
	// valid for localnet network topology (L2 networks)
	ExcludeCIDRs []string `json:"excludeCidrs,omitempty"`

	// LogFile to log all the messages from cni shim binary to
	LogFile string `json:"logFile,omitempty"`
	// Level is the logging verbosity level
	LogLevel string `json:"logLevel,omitempty"`
	// LogFileMaxSize is the maximum size in bytes of the logfile
	// before it gets rolled.
	LogFileMaxSize int `json:"logfile-maxsize"`
	// LogFileMaxBackups represents the the maximum number of
	// old log files to retain
	LogFileMaxBackups int `json:"logfile-maxbackups"`
	// LogFileMaxAge represents the maximum number
	// of days to retain old log files
	LogFileMaxAge int `json:"logfile-maxage"`
}
