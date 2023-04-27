package sriovnet

import (
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/vishvananda/netlink"

	utilfs "github.com/k8snetworkplumbingwg/sriovnet/pkg/utils/filesystem"
	"github.com/k8snetworkplumbingwg/sriovnet/pkg/utils/netlinkops"
)

const (
	// Used locally
	etherEncapType = "ether"
	ibEncapType    = "infiniband"
)

var (
	virtFnRe          = regexp.MustCompile(`virtfn(\d+)`)
	pciAddressRe      = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[01][0-9a-f].[0-7]$`)
	auxiliaryDeviceRe = regexp.MustCompile(`^(\S+\.){2}\d+$`)
)

type VfObj struct {
	Index      int
	PciAddress string
	Bound      bool
	Allocated  bool
}

type PfNetdevHandle struct {
	PfNetdevName string
	pfLinkHandle netlink.Link

	List []*VfObj
}

func SetPFLinkUp(pfNetdevName string) error {
	handle, err := netlinkops.GetNetlinkOps().LinkByName(pfNetdevName)
	if err != nil {
		return err
	}

	return netlinkops.GetNetlinkOps().LinkSetUp(handle)
}

func IsVfPciVfioBound(pciAddr string) bool {
	driverLink := filepath.Join(PciSysDir, pciAddr, "driver")
	driverPath, err := utilfs.Fs.Readlink(driverLink)
	if err != nil {
		return false
	}
	driverName := filepath.Base(driverPath)
	return driverName == "vfio-pci"
}

func IsSriovSupported(netdevName string) bool {
	maxvfs, err := getMaxVfCount(netdevName)
	if maxvfs == 0 || err != nil {
		return false
	}
	return true
}

func IsSriovEnabled(netdevName string) bool {
	curvfs, err := getCurrentVfCount(netdevName)
	if curvfs == 0 || err != nil {
		return false
	}
	return true
}

func EnableSriov(pfNetdevName string) error {
	var maxVfCount int
	var err error

	devDirName := netDevDeviceDir(pfNetdevName)

	devExist := dirExists(devDirName)
	if !devExist {
		return fmt.Errorf("device %s not found", pfNetdevName)
	}

	maxVfCount, err = getMaxVfCount(pfNetdevName)
	if err != nil {
		log.Println("Fail to read max vf count of PF", pfNetdevName)
		return err
	}

	if maxVfCount == 0 {
		return fmt.Errorf("sriov unsupported for device: %s", pfNetdevName)
	}

	curVfCount, err2 := getCurrentVfCount(pfNetdevName)
	if err2 != nil {
		log.Println("Fail to read current vf count of PF", pfNetdevName)
		return err
	}
	if curVfCount == 0 {
		return setMaxVfCount(pfNetdevName, maxVfCount)
	}
	return nil
}

func DisableSriov(pfNetdevName string) error {
	devDirName := netDevDeviceDir(pfNetdevName)

	devExist := dirExists(devDirName)
	if !devExist {
		return fmt.Errorf("device %s not found", pfNetdevName)
	}

	return setMaxVfCount(pfNetdevName, 0)
}

func GetPfNetdevHandle(pfNetdevName string) (*PfNetdevHandle, error) {
	pfLinkHandle, err := netlinkops.GetNetlinkOps().LinkByName(pfNetdevName)
	if err != nil {
		return nil, err
	}

	handle := PfNetdevHandle{
		PfNetdevName: pfNetdevName,
		pfLinkHandle: pfLinkHandle,
	}

	list, err := GetVfPciDevList(pfNetdevName)
	if err != nil {
		return nil, err
	}

	for _, vfDir := range list {
		vfIndexStr := strings.TrimPrefix(vfDir, netDevVfDevicePrefix)
		vfIndex, _ := strconv.Atoi(vfIndexStr)
		vfNetdevName := vfNetdevNameFromParent(pfNetdevName, vfIndex)
		pciAddress, err := vfPCIDevNameFromVfIndex(pfNetdevName, vfIndex)
		if err != nil {
			log.Printf("Failed to read PCI Address for VF %v from PF %v: %v\n",
				vfNetdevName, pfNetdevName, err)
			continue
		}
		vfObj := VfObj{
			Index:      vfIndex,
			PciAddress: pciAddress,
		}
		if vfNetdevName != "" {
			vfObj.Bound = true
		} else {
			vfObj.Bound = false
		}
		vfObj.Allocated = false
		handle.List = append(handle.List, &vfObj)
	}
	return &handle, nil
}

func UnbindVf(handle *PfNetdevHandle, vf *VfObj) error {
	cmdFile := filepath.Join(NetSysDir, handle.PfNetdevName, netdevDriverDir, netdevUnbindFile)
	cmdFileObj := fileObject{
		Path: cmdFile,
	}
	err := cmdFileObj.Write(vf.PciAddress)
	if err != nil {
		vf.Bound = false
	}
	return err
}

func BindVf(handle *PfNetdevHandle, vf *VfObj) error {
	cmdFile := filepath.Join(NetSysDir, handle.PfNetdevName, netdevDriverDir, netdevBindFile)
	cmdFileObj := fileObject{
		Path: cmdFile,
	}
	err := cmdFileObj.Write(vf.PciAddress)
	if err != nil {
		vf.Bound = true
	}
	return err
}

func GetVfDefaultMacAddr(vfNetdevName string) (string, error) {
	ethHandle, err1 := netlinkops.GetNetlinkOps().LinkByName(vfNetdevName)
	if err1 != nil {
		return "", err1
	}

	ethAttr := ethHandle.Attrs()
	return ethAttr.HardwareAddr.String(), nil
}

func SetVfDefaultMacAddress(handle *PfNetdevHandle, vf *VfObj) error {
	netdevName := vfNetdevNameFromParent(handle.PfNetdevName, vf.Index)
	ethHandle, err1 := netlinkops.GetNetlinkOps().LinkByName(netdevName)
	if err1 != nil {
		return err1
	}
	ethAttr := ethHandle.Attrs()
	return netlinkops.GetNetlinkOps().LinkSetVfHardwareAddr(handle.pfLinkHandle, vf.Index, ethAttr.HardwareAddr)
}

func SetVfVlan(handle *PfNetdevHandle, vf *VfObj, vlan int) error {
	return netlinkops.GetNetlinkOps().LinkSetVfVlan(handle.pfLinkHandle, vf.Index, vlan)
}

func setVfNodeGUID(handle *PfNetdevHandle, vf *VfObj, guid []byte) error {
	var err error

	nodeGUIDHwAddr := net.HardwareAddr(guid)

	err = ibSetNodeGUID(handle.PfNetdevName, vf.Index, nodeGUIDHwAddr)
	if err == nil {
		return nil
	}
	err = netlinkops.GetNetlinkOps().LinkSetVfNodeGUID(handle.pfLinkHandle, vf.Index, guid)
	return err
}

func setVfPortGUID(handle *PfNetdevHandle, vf *VfObj, guid []byte) error {
	var err error

	portGUIDHwAddr := net.HardwareAddr(guid)

	err = ibSetPortGUID(handle.PfNetdevName, vf.Index, portGUIDHwAddr)
	if err == nil {
		return nil
	}
	err = netlinkops.GetNetlinkOps().LinkSetVfPortGUID(handle.pfLinkHandle, vf.Index, guid)
	return err
}

func SetVfDefaultGUID(handle *PfNetdevHandle, vf *VfObj) error {
	randUUID, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	guid := randUUID[0:8]
	guid[7] = byte(vf.Index)

	err = setVfNodeGUID(handle, vf, guid)
	if err != nil {
		return err
	}

	err = setVfPortGUID(handle, vf, guid)
	return err
}

func SetVfPrivileged(handle *PfNetdevHandle, vf *VfObj, privileged bool) error {
	var spoofChk bool
	var trusted bool

	ethAttr := handle.pfLinkHandle.Attrs()
	if ethAttr.EncapType != etherEncapType {
		return nil
	}
	// Only ether type is supported
	if privileged {
		spoofChk = false
		trusted = true
	} else {
		spoofChk = true
		trusted = false
	}

	/* do not check for error status as older kernels doesn't
	 * have support for it.
	 * golangci-lint complains on missing error check. ignore it
	 * with nolint comment until we update the code to ignore ENOTSUP error
	 */
	netlinkops.GetNetlinkOps().LinkSetVfTrust(handle.pfLinkHandle, vf.Index, trusted)     //nolint
	netlinkops.GetNetlinkOps().LinkSetVfSpoofchk(handle.pfLinkHandle, vf.Index, spoofChk) //nolint
	return nil
}

func setDefaultHwAddr(handle *PfNetdevHandle, vf *VfObj) error {
	var err error

	ethAttr := handle.pfLinkHandle.Attrs()
	if ethAttr.EncapType == etherEncapType {
		err = SetVfDefaultMacAddress(handle, vf)
	} else if ethAttr.EncapType == ibEncapType {
		err = SetVfDefaultGUID(handle, vf)
	}
	return err
}

func setPortAdminState(handle *PfNetdevHandle, vf *VfObj) error {
	ethAttr := handle.pfLinkHandle.Attrs()
	if ethAttr.EncapType == ibEncapType {
		state, err2 := ibGetPortAdminState(handle.PfNetdevName, vf.Index)
		// Ignore the error where this file is not available
		if err2 != nil {
			return nil
		}
		log.Printf("Admin state = %v", state)
		err2 = ibSetPortAdminState(handle.PfNetdevName, vf.Index, ibSriovPortAdminStateFollow)
		if err2 != nil {
			// If file exist, we must be able to write
			log.Printf("Admin state setting error = %v", err2)
			return err2
		}
	}
	return nil
}

func ConfigVfs(handle *PfNetdevHandle, privileged bool) error {
	var err error

	for _, vf := range handle.List {
		log.Printf("vf = %v\n", vf)
		err = setPortAdminState(handle, vf)
		if err != nil {
			break
		}
		// skip VFs in another namespace
		netdevName := vfNetdevNameFromParent(handle.PfNetdevName, vf.Index)
		if _, err = netlinkops.GetNetlinkOps().LinkByName(netdevName); err != nil {
			continue
		}
		err = setDefaultHwAddr(handle, vf)
		if err != nil {
			break
		}
		_ = SetVfPrivileged(handle, vf, privileged)
	}
	if err != nil {
		return err
	}
	for _, vf := range handle.List {
		if !vf.Bound {
			continue
		}

		err = UnbindVf(handle, vf)
		if err != nil {
			log.Printf("Fail to unbind err=%v\n", err)
			break
		}

		err = BindVf(handle, vf)
		if err != nil {
			log.Printf("Fail to bind err=%v\n", err)
			break
		}
		log.Printf("vf = %v unbind/bind completed", vf)
	}
	return nil
}

func AllocateVf(handle *PfNetdevHandle) (*VfObj, error) {
	for _, vf := range handle.List {
		if vf.Allocated {
			continue
		}
		vf.Allocated = true
		log.Printf("Allocated vf = %v\n", *vf)
		return vf, nil
	}
	return nil, fmt.Errorf("all Vfs for %v are allocated", handle.PfNetdevName)
}

func AllocateVfByMacAddress(handle *PfNetdevHandle, vfMacAddress string) (*VfObj, error) {
	for _, vf := range handle.List {
		if vf.Allocated {
			continue
		}

		netdevName := vfNetdevNameFromParent(handle.PfNetdevName, vf.Index)
		macAddr, _ := GetVfDefaultMacAddr(netdevName)
		if macAddr != vfMacAddress {
			continue
		}
		vf.Allocated = true
		log.Printf("Allocated vf by mac = %v\n", *vf)
		return vf, nil
	}
	return nil, fmt.Errorf("all Vfs for %v are allocated for mac address %v",
		handle.PfNetdevName, vfMacAddress)
}

func FreeVf(handle *PfNetdevHandle, vf *VfObj) {
	vf.Allocated = false
	log.Printf("Free vf = %v\n", *vf)
}

func FreeVfByNetdevName(handle *PfNetdevHandle, vfIndex int) error {
	vfNetdevName := fmt.Sprintf("%s%v", netDevVfDevicePrefix, vfIndex)
	for _, vf := range handle.List {
		netdevName := vfNetdevNameFromParent(handle.PfNetdevName, vf.Index)
		if vf.Allocated && netdevName == vfNetdevName {
			vf.Allocated = true
			return nil
		}
	}
	return fmt.Errorf("vf netdev %v not found", vfNetdevName)
}

func GetVfNetdevName(handle *PfNetdevHandle, vf *VfObj) string {
	return vfNetdevNameFromParent(handle.PfNetdevName, vf.Index)
}

// GetVfIndexByPciAddress gets a VF PCI address (e.g '0000:03:00.4') and
// returns the correlate VF index.
func GetVfIndexByPciAddress(vfPciAddress string) (int, error) {
	vfPath := filepath.Join(PciSysDir, vfPciAddress, "physfn", "virtfn*")
	matches, err := filepath.Glob(vfPath)
	if err != nil {
		return -1, err
	}
	for _, match := range matches {
		tmp, err := os.Readlink(match)
		if err != nil {
			continue
		}
		if strings.Contains(tmp, vfPciAddress) {
			result := virtFnRe.FindStringSubmatch(match)
			vfIndex, err := strconv.Atoi(result[1])
			if err != nil {
				continue
			}
			return vfIndex, nil
		}
	}
	return -1, fmt.Errorf("vf index for %s not found", vfPciAddress)
}

// gets the PF index that's associated with a VF PCI address (e.g '0000:03:00.4')
func GetPfIndexByVfPciAddress(vfPciAddress string) (int, error) {
	const pciParts = 4
	pfPciAddress, err := GetPfPciFromVfPci(vfPciAddress)
	if err != nil {
		return -1, err
	}
	var domain, bus, dev, fn int
	parsed, err := fmt.Sscanf(pfPciAddress, "%04x:%02x:%02x.%d", &domain, &bus, &dev, &fn)
	if err != nil {
		return -1, fmt.Errorf("error trying to parse PF PCI address %s: %v", pfPciAddress, err)
	}
	if parsed != pciParts {
		return -1, fmt.Errorf("failed to parse PF PCI address %s. Unexpected format", pfPciAddress)
	}
	return fn, err
}

// GetPfPciFromVfPci retrieves the parent PF PCI address of the provided VF PCI address in D:B:D.f format
func GetPfPciFromVfPci(vfPciAddress string) (string, error) {
	pfPath := filepath.Join(PciSysDir, vfPciAddress, "physfn")
	pciDevDir, err := utilfs.Fs.Readlink(pfPath)
	if err != nil {
		return "", fmt.Errorf("failed to read physfn link, provided address may not be a VF. %v", err)
	}

	pf := path.Base(pciDevDir)
	if pf == "" {
		return pf, fmt.Errorf("could not find PF PCI Address")
	}
	return pf, err
}

// GetNetDevicesFromPci gets a PCI address (e.g '0000:03:00.1') and
// returns the correlate list of netdevices
func GetNetDevicesFromPci(pciAddress string) ([]string, error) {
	pciDir := filepath.Join(PciSysDir, pciAddress, "net")
	return getFileNamesFromPath(pciDir)
}

// GetPciFromNetDevice returns the PCI address associated with a network device name
func GetPciFromNetDevice(name string) (string, error) {
	devPath := filepath.Join(NetSysDir, name)

	realPath, err := utilfs.Fs.Readlink(devPath)
	if err != nil {
		return "", fmt.Errorf("device %s not found: %s", name, err)
	}

	parent := filepath.Dir(realPath)
	base := filepath.Base(parent)
	// Devices can have their PCI device sysfs entry at different levels:
	// PF, VF, SF representor:
	//   /sys/devices/pci0000:00/.../0000:03:00.0/net/p0
	//   /sys/devices/pci0000:00/.../0000:03:00.0/net/pf0hpf
	//   /sys/devices/pci0000:00/.../0000:03:00.0/net/pf0vf0
	//   /sys/devices/pci0000:00/.../0000:03:00.0/net/pf0sf0
	// SF port:
	//   /sys/devices/pci0000:00/.../0000:03:00.0/mlx5_core.sf.3/net/enp3s0f0s1
	// This loop allows detecting any of them.
	for parent != "/" && !pciAddressRe.MatchString(base) {
		parent = filepath.Dir(parent)
		base = filepath.Base(parent)
	}
	// If we stopped on '/' and the base was never a proper PCI address,
	// then 'netdev' is not a PCI device.
	if !pciAddressRe.MatchString(base) {
		return "", fmt.Errorf("device %s is not a PCI device: %s", name, realPath)
	}
	return base, nil
}
