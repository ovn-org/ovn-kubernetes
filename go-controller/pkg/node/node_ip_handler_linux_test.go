package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	ovnNodeL3GatewayConfig   = "k8s.ovn.org/l3-gateway-config"
	ovnDefaultNetworkGateway = "default"
	ovnNodeChassisID         = "k8s.ovn.org/node-chassis-id"
	ovnNodeHostAddresses     = "k8s.ovn.org/host-addresses"
	ovnNodeIfAddr            = "k8s.ovn.org/node-primary-ifaddr"
)

// nodeIPNet allows to pass thread safe IP address updates into
// addressManagerNetEnumerator (in order to mock the addressManager)
type nodeIPNet struct {
	currentNodeIPNet        net.IPNet
	currentNodeIPUpdateLock sync.RWMutex
}

func newNodeIPNet() *nodeIPNet {
	return &nodeIPNet{net.IPNet{}, sync.RWMutex{}}
}

func (n *nodeIPNet) set(ipnet net.IPNet) {
	n.currentNodeIPUpdateLock.Lock()
	defer n.currentNodeIPUpdateLock.Unlock()
	n.currentNodeIPNet = ipnet
}

func (n *nodeIPNet) get() net.IPNet {
	n.currentNodeIPUpdateLock.RLock()
	defer n.currentNodeIPUpdateLock.RUnlock()
	return n.currentNodeIPNet
}

// testCase contains the test cases for ipv4, ipv6
type testCase struct {
	nodeName               string
	nodeIPString           string
	updatedNodeIPString    string
	managementPortIPString string
	bridgeName             string
	bridgeMacAddress       string
	nodeChassisID          string
	nodeInterfaceID        string
	nodeNextHops           string
}

// setupEnvironment sets up the fake kubernetes API and client, populates the fake API with a test node,
// it sets up the node watch factory and a new AddressManager
func setupEnvironment(ipFamily string, tc testCase, currentNodeIPNet *nodeIPNet) (kubeFakeClient *fake.Clientset, wf *factory.WatchFactory, stopChan chan struct{}, doneWg *sync.WaitGroup) {
	if ipFamily == "ipv4" {
		config.IPv4Mode = true
	} else if ipFamily == "ipv6" {
		config.IPv6Mode = true
	}

	// Parse node's IP and subnet mask
	nodeIP, nodeNet, err := net.ParseCIDR(tc.nodeIPString)
	if err != nil {
		Expect(err).NotTo(HaveOccurred())
	}
	nodeIPNet := net.IPNet{
		IP:   nodeIP,
		Mask: nodeNet.Mask,
	}

	// initialize currentNodeIPNet
	currentNodeIPNet.set(nodeIPNet)

	// Marshal the existing node's L3GatewayConfig gateway annotation
	l3Config := &util.L3GatewayConfig{
		Mode:           config.GatewayModeShared,
		ChassisID:      tc.nodeChassisID,
		InterfaceID:    tc.nodeInterfaceID,
		MACAddress:     ovntest.MustParseMAC(tc.bridgeMacAddress),
		IPAddresses:    ovntest.MustParseIPNets(tc.nodeIPString),
		NextHops:       ovntest.MustParseIPs(tc.nodeNextHops),
		NodePortEnable: true,
	}
	gatewayAnnotation := map[string]*util.L3GatewayConfig{ovnDefaultNetworkGateway: l3Config}
	l3ConfigBytes, err := json.Marshal(gatewayAnnotation)
	if err != nil {
		Expect(err).NotTo(HaveOccurred())
	}

	// Marshal the existing node's host address annotation
	hostAddressesBytes, err := json.Marshal([]string{nodeIP.String()})
	if err != nil {
		Expect(err).NotTo(HaveOccurred())
	}

	// Create the existing node - certain annotations are guaranteed to be set
	// before the addressManager looks at them
	existingNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: tc.nodeName,
			Annotations: map[string]string{
				ovnNodeL3GatewayConfig: string(l3ConfigBytes),
				ovnNodeChassisID:       tc.nodeChassisID,
				ovnNodeHostAddresses:   string(hostAddressesBytes),
			},
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: nodeIP.String(),
				},
			},
		},
	}

	// Populate the fake client set with the existing node
	kubeFakeClient = fake.NewSimpleClientset(
		&v1.NodeList{
			Items: []v1.Node{existingNode},
		})

	// Create required ClientSets
	fakeClient := &util.OVNClientset{
		KubeClient: kubeFakeClient,
	}
	k := &kube.Kube{fakeClient.KubeClient, nil, nil, nil}

	// Create a new NodeWatchFactory and start it
	wf, err = factory.NewNodeWatchFactory(fakeClient, tc.nodeName)
	if err != nil {
		Expect(err).NotTo(HaveOccurred())
	}

	// Start the watch factory
	err = wf.Start()
	if err != nil {
		Expect(err).NotTo(HaveOccurred())
	}

	// Parse managed IP and subnet
	managementPortIP, managementPortNet, err := net.ParseCIDR("100.0.0.1/24")
	if err != nil {
		Expect(err).NotTo(HaveOccurred())
	}
	managementPortIPNet := net.IPNet{
		IP:   managementPortIP,
		Mask: managementPortNet.Mask,
	}

	// Make a fake managementPortConfig with only the fields we care about
	mgmtPortIPFamilyConfig := managementPortIPFamilyConfig{
		ipt:        nil,
		allSubnets: nil,
		ifAddr:     &managementPortIPNet,
		gwIP:       net.ParseIP("100.0.0.254"),
	}
	manPortConf := managementPortConfig{
		ifName:    tc.nodeName,
		link:      nil,
		routerMAC: nil,
		ipv4:      &mgmtPortIPFamilyConfig,
		ipv6:      nil,
	}

	// Create an empty gatewayBridge with name 'br-ex'
	gwBridge := &bridgeConfiguration{
		bridgeName: tc.bridgeName,
	}

	// Create and run the new address manager
	addressManagerNetEnumerator := &addressManagerNetEnumerator{
		tickInterval: 1,
		netInterfaceAddrs: func() ([]net.Addr, error) {
			toReturn := currentNodeIPNet.get()
			return []net.Addr{&toReturn}, nil
		},
		getNetworkInterfaceIPAddresses: func(iface string, kubeNodeIP net.IP) ([]*net.IPNet, error) {
			toReturn := currentNodeIPNet.get()
			return []*net.IPNet{&toReturn}, nil
		},
		netlinkAddrSubscribeWithOptions: func(ch chan<- netlink.AddrUpdate, done <-chan struct{}, options netlink.AddrSubscribeOptions) error {
			/* tbd for more advanced scenarios, something along these lines ...
			go func() {
				defer close(ch)

				ch <- AddrUpdate{LinkAddress: *addr.IPNet,
					LinkIndex:   addr.LinkIndex,
					NewAddr:     msgType == unix.RTM_NEWADDR,
					Flags:       addr.Flags,
					Scope:       addr.Scope,
					PreferedLft: addr.PreferedLft,
					ValidLft:    addr.ValidLft}
			}()*/
			return nil
		},
	}

	stopChan = make(chan struct{})
	doneWg = &sync.WaitGroup{}
	am := newAddressManager(tc.nodeName, k, &manPortConf, wf, gwBridge, addressManagerNetEnumerator)
	// Now, run the address manager
	am.Run(stopChan, doneWg)

	return
}

// isNodeUpdated makes sure that node with nodeName has updated annotaions which match expectedNodeIPNet and ipFamily.
// Loop for a maximum of maxTries and sleep for 1 second in between tries.
func isNodeUpdated(c clientset.Interface, nodeName string, expectedNodeIPNet net.IPNet, ipFamily string, maxTries int) bool {
	var err error
	var retrievedNode *v1.Node
	var retrievedNodeL3GatewayConfig *util.L3GatewayConfig
	var retrievedNodeHostAddresses sets.String
	var retrievedNodePrimaryIfAddr *util.ParsedNodeEgressIPConfiguration
	for i := 0; i < maxTries; i++ {
		time.Sleep(1 * time.Second)

		// compare what we got to what we want
		retrievedNode, err = c.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "Encountered error, skiping: %v", err)
			continue
		}
		// k8s.ovn.org/l3-gateway-config
		retrievedNodeL3GatewayConfig, err = util.ParseNodeL3GatewayAnnotation(retrievedNode)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "Encountered error, skiping: %v", err)
			continue
		}
		// k8s.ovn.org/host-addresses
		retrievedNodeHostAddresses, err = util.ParseNodeHostAddresses(retrievedNode)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "Encountered error, skiping: %v", err)
			continue
		}
		// k8s.ovn.org/node-primary-ifaddr
		retrievedNodePrimaryIfAddr, err = util.ParseNodePrimaryIfAddr(retrievedNode)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "Encountered error, skiping: %v", err)
			continue
		}
		// check if we have a match in the L3 GW annotation
		l3IpMatches := false
		for _, l3IPAddr := range retrievedNodeL3GatewayConfig.IPAddresses {
			if expectedNodeIPNet.String() == l3IPAddr.String() {
				l3IpMatches = true
				continue
			}
		}
		if !l3IpMatches {
			fmt.Fprintf(GinkgoWriter, "No match in k8s.ovn.org/l3-gateway-config, looking for %s, got %v\n", expectedNodeIPNet, retrievedNodeL3GatewayConfig.IPAddresses)
			continue
		}
		if !retrievedNodeHostAddresses.Has(expectedNodeIPNet.IP.String()) {
			fmt.Fprintf(GinkgoWriter, "No match in k8s.ovn.org/host-addresses, looking for %s, got %v\n", expectedNodeIPNet.IP, retrievedNodeHostAddresses)
			continue
		}
		if ipFamily == "ipv4" && !expectedNodeIPNet.IP.Equal(retrievedNodePrimaryIfAddr.V4.IP) ||
			ipFamily == "ipv6" && !expectedNodeIPNet.IP.Equal(retrievedNodePrimaryIfAddr.V6.IP) {
			fmt.Fprintf(GinkgoWriter, "No match in k8s.ovn.org/node-primary-ifaddr, looking for %s, got %v\n", expectedNodeIPNet.IP, retrievedNodePrimaryIfAddr)
			continue
		}
		// if we get here, break out of the loop
		return true
	}
	return false
}

var _ = Describe("Test that addressManager is operational", func() {
	tcs := map[string]testCase{
		"ipv4": {
			nodeName:               "worker1",
			nodeIPString:           "10.1.1.10/24",
			updatedNodeIPString:    "10.1.1.50/24",
			managementPortIPString: "10.1.2.1/24",
			bridgeName:             "br-ex",
			bridgeMacAddress:       "00:00:00:00:00:01",
			nodeChassisID:          "chassis1",
			nodeInterfaceID:        "br-ex",
			nodeNextHops:           "10.1.1.254",
		},
		"ipv6": {
			nodeName:               "worker2",
			nodeIPString:           "2000::10:1:1:10/64",
			updatedNodeIPString:    "2000::10:1:1:50/24",
			managementPortIPString: "2001::10:1:2:1/64",
			bridgeName:             "br-ex2",
			bridgeMacAddress:       "00:00:00:00:00:01",
			nodeChassisID:          "chassis2",
			nodeInterfaceID:        "br-ex2",
			nodeNextHops:           "2000::10:1:1:254",
		},
	}

	Context("For IP family IPv4", func() {
		ipFamily := "ipv4"
		tc := tcs[ipFamily]
		var kubeFakeClient *fake.Clientset
		var wf *factory.WatchFactory
		var stopChan chan struct{}
		var doneWg *sync.WaitGroup
		var currentNodeIPNet *nodeIPNet

		BeforeEach(func() {
			currentNodeIPNet = newNodeIPNet()
			kubeFakeClient, wf, stopChan, doneWg = setupEnvironment(ipFamily, tc, currentNodeIPNet)
		})

		AfterEach(func() {
			// Shut down the watch factory and the address manager run  when we exit the test method
			close(stopChan)
			doneWg.Wait()
			wf.Shutdown()
		})

		It("updates the node IP annotations to "+tc.updatedNodeIPString, func() {
			// Parse updated node's IP and subnet mask
			updatedNodeIP, updatedNodeNet, err := net.ParseCIDR(tc.updatedNodeIPString)
			if err != nil {
				Expect(err).NotTo(HaveOccurred())
			}
			updatedNodeIPNet := net.IPNet{
				IP:   updatedNodeIP,
				Mask: updatedNodeNet.Mask,
			}

			// update currentNodeIPNet
			currentNodeIPNet.set(updatedNodeIPNet)

			updated := isNodeUpdated(kubeFakeClient, tc.nodeName, updatedNodeIPNet, ipFamily, 10)
			Expect(updated).To(BeTrue())
		})
	})

	Context("For IP family IPv6", func() {
		ipFamily := "ipv6"
		tc := tcs[ipFamily]
		var kubeFakeClient *fake.Clientset
		var wf *factory.WatchFactory
		var stopChan chan struct{}
		var doneWg *sync.WaitGroup
		var currentNodeIPNet *nodeIPNet

		BeforeEach(func() {
			currentNodeIPNet = newNodeIPNet()
			kubeFakeClient, wf, stopChan, doneWg = setupEnvironment(ipFamily, tc, currentNodeIPNet)
		})

		AfterEach(func() {
			// Shut down the watch factory and the address manager run  when we exit the test method
			close(stopChan)
			doneWg.Wait()
			wf.Shutdown()
		})

		It("updates the node IP annotations to "+tc.updatedNodeIPString, func() {
			// Parse updated node's IP and subnet mask
			updatedNodeIP, updatedNodeNet, err := net.ParseCIDR(tc.updatedNodeIPString)
			if err != nil {
				Expect(err).NotTo(HaveOccurred())
			}
			updatedNodeIPNet := net.IPNet{
				IP:   updatedNodeIP,
				Mask: updatedNodeNet.Mask,
			}

			// update currentNodeIPNet
			currentNodeIPNet.set(updatedNodeIPNet)

			updated := isNodeUpdated(kubeFakeClient, tc.nodeName, updatedNodeIPNet, ipFamily, 10)
			Expect(updated).To(BeTrue())
		})
	})
})
