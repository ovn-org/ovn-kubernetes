package ovn

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

type addressSet struct {
	sync.Mutex

	name      string
	hashName  string
	ports     map[string]net.IP // logical port :: pod IP
	destroyed bool
}

// hash the provided input to make it a valid addressSet name.
func hashedAddressSet(s string) string {
	return hashForOVN(s)
}

type addressSetIterFn func(name, namespace, suffix string)

// forEachAddressSetUnhashedName will pass the unhashedName, namespaceName and
// the first suffix in the name to the 'iteratorFn' for every address_set in
// OVN. (Each unhashed name for an addressSet can be of the form
// namespaceName.suffix1.suffix2. .suffixN)
func forEachAddressSetUnhashedName(iteratorFn addressSetIterFn) error {
	output, stderr, err := util.RunOVNNbctl("--data=bare", "--no-heading",
		"--columns=external_ids", "find", "address_set")
	if err != nil {
		return fmt.Errorf("error reading address sets: "+
			"stdout: %q, stderr: %q err: %v", output, stderr, err)
	}
	for _, addrSet := range strings.Fields(output) {
		if !strings.HasPrefix(addrSet, "name=") {
			continue
		}
		addrSetName := addrSet[5:]
		names := strings.Split(addrSetName, ".")
		addrSetNamespace := names[0]
		nameSuffix := ""
		if len(names) >= 2 {
			nameSuffix = names[1]
		}
		iteratorFn(addrSetName, addrSetNamespace, nameSuffix)
	}
	return nil
}

func NewAddressSet(name string, ports map[string]net.IP) (*addressSet, error) {
	if ports == nil {
		ports = make(map[string]net.IP)
	}
	as := &addressSet{
		name:     name,
		hashName: hashedAddressSet(name),
		ports:    ports,
	}

	klog.V(5).Infof("New(%s/%s) with %v", as.hashName, as.name, ports)

	addressSet, stderr, err := util.RunOVNNbctl("--data=bare",
		"--no-heading", "--columns=_uuid", "find", "address_set",
		"name="+as.hashName)
	if err != nil {
		return nil, fmt.Errorf("find failed to get address set %q, stderr: %q (%v)",
			as.name, stderr, err)
	}

	// addressSet has already been created in the database and nothing to set.
	if addressSet != "" {
		if err := as.setOrClear(); err != nil {
			return nil, err
		}
	} else {
		// addressSet has not been created yet. Create it.
		args := []string{
			"create",
			"address_set",
			"name=" + as.hashName,
			"external-ids:name=" + as.name,
		}
		joinedIPs := as.joinIPs()
		if len(joinedIPs) > 0 {
			args = append(args, "addresses="+joinedIPs)
		}
		_, stderr, err = util.RunOVNNbctl(args...)
		if err != nil {
			return nil, fmt.Errorf("failed to create address set %q, stderr: %q (%v)",
				as.name, stderr, err)
		}
	}

	return as, nil
}

func (as *addressSet) joinIPs() string {
	list := make([]string, 0, len(as.ports))
	for _, ip := range as.ports {
		list = append(list, `"`+ip.String()+`"`)
	}
	return strings.Join(list, " ")
}

// setOrClear updates the OVN database with the address set's addresses or
// clears the address set if there are no addresses in the address set
func (as *addressSet) setOrClear() error {
	joinedIPs := as.joinIPs()
	if len(joinedIPs) > 0 {
		_, stderr, err := util.RunOVNNbctl("set", "address_set", as.hashName, "addresses="+joinedIPs)
		if err != nil {
			return fmt.Errorf("failed to set address set %q, stderr: %q (%v)",
				as.name, stderr, err)
		}
	} else {
		_, stderr, err := util.RunOVNNbctl("clear", "address_set", as.hashName, "addresses")
		if err != nil {
			return fmt.Errorf("failed to clear address set %q, stderr: %q (%v)",
				as.name, stderr, err)
		}
	}
	return nil
}

func (as *addressSet) ReplacePorts(ports map[string]net.IP) error {
	as.Lock()
	defer as.Unlock()
	if as.destroyed {
		return nil
	}

	klog.V(5).Infof("ReplacePorts(%s/%s) with %v", as.hashName, as.name, ports)
	as.ports = ports
	return as.setOrClear()
}

func (as *addressSet) AddPort(logicalPort string, ip net.IP) error {
	as.Lock()
	defer as.Unlock()
	if as.destroyed || as.ports[logicalPort] != nil {
		return nil
	}

	klog.V(5).Infof("AddPort(%s/%s) %s=%s", as.hashName, as.name, logicalPort, ip)
	as.ports[logicalPort] = ip

	_, stderr, err := util.RunOVNNbctl("add", "address_set", as.hashName, "addresses", `"`+ip.String()+`"`)
	if err != nil {
		return fmt.Errorf("failed to add port %q address %q to address_set %q, stderr: %q (%v)",
			logicalPort, ip, as.hashName, stderr, err)
	}
	return nil
}

func (as *addressSet) DeletePort(logicalPort string) error {
	as.Lock()
	defer as.Unlock()
	if as.destroyed || as.ports[logicalPort] == nil {
		return nil
	}

	ip := as.ports[logicalPort]
	klog.V(5).Infof("DeletePort(%s/%s) %s=%s", as.hashName, as.name, logicalPort, ip)
	delete(as.ports, logicalPort)

	_, stderr, err := util.RunOVNNbctl("remove", "address_set", as.hashName, "addresses", `"`+ip.String()+`"`)
	if err != nil {
		return fmt.Errorf("failed to remove port %q address %q from address_set %q, stderr: %q (%v)",
			logicalPort, ip, as.hashName, stderr, err)
	}
	return nil
}

func getPodIP(pod *kapi.Pod) net.IP {
	annotation, _ := util.UnmarshalPodAnnotation(pod.Annotations)
	if annotation != nil {
		return annotation.IPs[0].IP
	}
	if pod.Status.PodIP != "" {
		return net.ParseIP(pod.Status.PodIP)
	}
	return nil
}

func (as *addressSet) AddPod(pod *kapi.Pod) error {
	if ip := getPodIP(pod); ip != nil {
		return as.AddPort(podLogicalPortName(pod), ip)
	}
	return fmt.Errorf("failed to determine pod %s/%s IP", pod.Namespace, pod.Name)
}

func (as *addressSet) DelPod(pod *kapi.Pod) error {
	return as.DeletePort(podLogicalPortName(pod))
}

// PortIterFn will be called once per port and should return an error to stop
// iteration.
type PortIterFn func(logicalPort string, ip net.IP)

func (as *addressSet) ForEachPort(iterFn PortIterFn) {
	as.Lock()
	defer as.Unlock()
	if !as.destroyed {
		for logicalPort, ip := range as.ports {
			iterFn(logicalPort, ip)
		}
	}
}

func destroyAddressSet(name string) {
	_, stderr, err := util.RunOVNNbctl("--if-exists", "destroy", "address_set", hashedAddressSet(name))
	if err != nil {
		klog.Errorf("failed to destroy address set %q, stderr: %q, (%v)", name, stderr, err)
	}
}

func (as *addressSet) Destroy() {
	as.Lock()
	defer as.Unlock()

	klog.V(5).Infof("Destroy(%s/%s)", as.hashName, as.name)
	if !as.destroyed {
		destroyAddressSet(as.name)
		as.destroyed = true
	}
}
