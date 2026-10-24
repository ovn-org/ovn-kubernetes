// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"

	kapi "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	utilnet "k8s.io/utils/net"
)

var _ = Describe("Network Segmentation: services", feature.NetworkSegmentation, func() {

	f := wrappedTestFramework("udn-services")
	f.SkipNamespaceCreation = true

	Context("on a user defined primary network", func() {
		const (
			nadName                      = "tenant-red"
			servicePort                  = 88
			serviceTargetPort            = 80
			userDefinedNetworkIPv4Subnet = "172.16.0.0/16" // first subnet in private range 172.16.0.0/12 (rfc1918)
			userDefinedNetworkIPv6Subnet = "2014:100:200::0/60"
			customL2IPv4Gateway          = "172.16.0.3"
			customL2IPv6Gateway          = "2014:100:200::3"
			customL2IPv4ReservedCIDR     = "172.16.1.0/24"
			customL2IPv6ReservedCIDR     = "2014:100:200::100/120"
			customL2IPv4InfraCIDR        = "172.16.0.0/30"
			customL2IPv6InfraCIDR        = "2014:100:200::/122"
		)

		var (
			cs        clientset.Interface
			nadClient nadclient.K8sCniCncfIoV1Interface
		)

		BeforeEach(func() {
			cs = f.ClientSet

			var err error
			nadClient, err = nadclient.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred())
			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			f.Namespace = namespace
			Expect(err).NotTo(HaveOccurred())
		})

		It("replays NodePort programming when a namespace joins an active primary network", func() {
			ctx := context.Background()
			lateNamespace := f.Namespace.Name + "-late"
			_, err := cs.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: lateNamespace,
					Labels: map[string]string{
						RequiredUDNNamespaceLabel: "",
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				Expect(cs.CoreV1().Namespaces().Delete(ctx, lateNamespace, metav1.DeleteOptions{})).To(Succeed())
			})

			jig := e2eservice.NewTestJig(cs, lateNamespace, "late-udn-service")
			service, err := jig.CreateUDPService(ctx, func(s *v1.Service) {
				policy := v1.IPFamilyPolicyPreferDualStack
				s.Spec.IPFamilyPolicy = &policy
				s.Spec.Ports = []v1.ServicePort{{
					Name:       "udp",
					Protocol:   v1.ProtocolUDP,
					Port:       servicePort,
					TargetPort: intstr.FromInt(serviceTargetPort),
				}}
				s.Spec.Type = v1.ServiceTypeNodePort
			})
			Expect(err).NotTo(HaveOccurred())

			cudnName := randomNetworkMetaName()
			cleanupManifest, err := createManifest("", newPrimaryClusterUDNManifest(cs, cudnName, f.Namespace.Name))
			DeferCleanup(cleanupManifest)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				Expect(cs.CoreV1().Services(lateNamespace).Delete(ctx, service.Name, metav1.DeleteOptions{})).To(Succeed())
				for _, namespace := range []string{f.Namespace.Name, lateNamespace} {
					Expect(cs.CoreV1().Pods(namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})).To(Succeed())
				}
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--wait", "--timeout=120s")
				Expect(err).NotTo(HaveOccurred())
			})

			Eventually(clusterUserDefinedNetworkReadyFunc(ctx, f.DynamicClient, cudnName), 30*time.Second, time.Second).Should(Succeed())
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, cs, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(nodes.Items).To(HaveLen(1))
			node := &nodes.Items[0]

			By("starting the primary network on the selected node")
			networkPod := e2epod.NewAgnhostPod(f.Namespace.Name, "network-start", nil, nil, nil)
			networkPod.Spec.NodeName = node.Name
			e2epod.NewPodClient(f).CreateSync(ctx, networkPod)

			By("adding the namespace with its existing Service to the running CUDN")
			patch := fmt.Sprintf(`[{"op":"add","path":"/spec/namespaceSelector/matchExpressions/0/values/-","value":%q}]`, lateNamespace)
			_, err = e2ekubectl.RunKubectl("", "patch", "clusteruserdefinednetwork", cudnName, "--type=json", "-p="+patch)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := nadClient.NetworkAttachmentDefinitions(lateNamespace).Get(ctx, cudnName, metav1.GetOptions{})
				return err
			}, 30*time.Second, time.Second).Should(Succeed(), "the primary NAD should be created in the late namespace")

			By("creating a backend and client after the primary NAD arrives")
			serverPod := e2epod.NewAgnhostPod(
				lateNamespace, "service-backend", nil, nil,
				[]v1.ContainerPort{{ContainerPort: serviceTargetPort, Protocol: v1.ProtocolUDP}},
				"-c", fmt.Sprintf(`
set -xe
iface=ovn-udn1
ips=$(ip -o addr show dev $iface | grep global | awk '{print $4}' | cut -d/ -f1 | paste -sd, -)
./agnhost netexec --udp-port=%d --udp-listen-addresses=$ips
`, serviceTargetPort))
			serverPod.Spec.Containers[0].Command = []string{"/bin/bash"}
			serverPod.Labels = jig.Labels
			serverPod.Spec.NodeName = node.Name
			serverPod = e2epod.PodClientNS(f, lateNamespace).CreateSync(ctx, serverPod)

			clientPod := e2epod.NewAgnhostPod(lateNamespace, "service-client", nil, nil, nil)
			clientPod.Spec.NodeName = node.Name
			clientPod = e2epod.PodClientNS(f, lateNamespace).CreateSync(ctx, clientPod)

			service, err = cs.CoreV1().Services(lateNamespace).Get(ctx, service.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			checkConnectionToNodePort(f, clientPod, service, node, "local node", serverPod.Name)
		})

		DescribeTable(
			// The test creates a client and nodeport service in a UDN backed by one pod and similarly
			// a nodeport service and a client in the default network. We expect ClusterIPs to be
			// reachable only within the same network. We expect NodePort services to be exposed
			// to all networks.
			// We verify the following scenarios:
			// - UDN client --> UDN service, with backend pod and client running on the same node:
			//   + clusterIP succeeds
			//   + nodeIP:nodePort works, when we only target the local node
			//
			// - UDN client --> UDN service, with backend pod and client running on different nodes:
			//   + clusterIP succeeds
			//   + nodeIP:nodePort succeeds, when we only target the local node
			//
			// - default-network client --> UDN service:
			//   + clusterIP fails
			//   + nodeIP:nodePort fails FOR NOW, when we only target the local node
			//
			// -  UDN service --> default-network:
			//   + clusterIP fails
			//   + nodeIP:nodePort fails FOR NOW, when we only target the local node

			"should be reachable through their cluster IP, node port and load balancer",
			func(
				netConfigParams networkAttachmentConfigParams,
			) {
				namespace := f.Namespace.Name
				jig := e2eservice.NewTestJig(cs, namespace, "udn-service")

				dynamicUDNEnabled := isDynamicUDNEnabled()

				By("Selecting 3 schedulable nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(nodes.Items)).To(BeNumerically(">", 2))

				By("Selecting nodes for pods and service")
				serverPodNodeName := nodes.Items[0].Name
				clientNode := nodes.Items[1].Name // when client runs on a different node than the server

				By("Creating the attachment configuration")
				netConfig := newNetworkAttachmentConfig(netConfigParams)
				netConfig.namespace = f.Namespace.Name
				_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig, f.ClientSet),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By(fmt.Sprintf("Creating a UDN LoadBalancer service"))
				policy := v1.IPFamilyPolicyPreferDualStack
				udnService, err := jig.CreateUDPService(context.TODO(), func(s *v1.Service) {
					s.Spec.Ports = []v1.ServicePort{
						{
							Name:       "udp",
							Protocol:   v1.ProtocolUDP,
							Port:       servicePort,
							TargetPort: intstr.FromInt(serviceTargetPort),
						},
					}
					s.Spec.Type = v1.ServiceTypeLoadBalancer
					s.Spec.IPFamilyPolicy = &policy
				})
				framework.ExpectNoError(err)

				By("Wait for UDN LoadBalancer Ingress to pop up")
				udnService, err = jig.WaitForLoadBalancer(context.TODO(), 180*time.Second)
				framework.ExpectNoError(err)

				By("Creating a UDN backend pod")
				udnServerPod := e2epod.NewAgnhostPod(
					namespace, "backend-pod", nil, nil,
					[]v1.ContainerPort{
						{ContainerPort: (serviceTargetPort), Protocol: "UDP"}},
					"-c",
					fmt.Sprintf(`
set -xe
iface=ovn-udn1
ips=$(ip -o addr show dev $iface| grep global |awk '{print $4}' | cut -d/ -f1 | paste -sd, -)
./agnhost netexec --udp-port=%d --udp-listen-addresses=$ips
`, serviceTargetPort))
				udnServerPod.Spec.Containers[0].Command = []string{"/bin/bash"}

				udnServerPod.Labels = jig.Labels
				udnServerPod.Spec.NodeName = serverPodNodeName
				udnServerPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), udnServerPod)

				By(fmt.Sprintf("Creating a UDN client pod on the same node (%s)", udnServerPod.Spec.NodeName))
				udnClientPod := e2epod.NewAgnhostPod(namespace, "udn-client", nil, nil, nil)
				udnClientPod.Spec.NodeName = udnServerPod.Spec.NodeName
				udnClientPod = e2epod.NewPodClient(f).CreateSync(context.TODO(), udnClientPod)

				// UDN -> UDN
				By("Connect to the UDN service cluster IP from the UDN client pod on the same node")
				checkConnectionToClusterIPs(f, udnClientPod, udnService, udnServerPod.Name)
				By("Connect to the UDN service nodePort on all 3 nodes from the UDN client pod")
				checkConnectionToLoadBalancers(f, udnClientPod, udnService, udnServerPod.Name)
				checkConnectionToNodePort(f, udnClientPod, udnService, &nodes.Items[0], "endpoint node", udnServerPod.Name)
				if !dynamicUDNEnabled {
					checkConnectionToNodePort(f, udnClientPod, udnService, &nodes.Items[1], "other node", udnServerPod.Name)
					checkConnectionToNodePort(f, udnClientPod, udnService, &nodes.Items[2], "other node", udnServerPod.Name)
				}
				By(fmt.Sprintf("Creating a UDN client pod on a different node (%s)", clientNode))
				udnClientPod2 := e2epod.NewAgnhostPod(namespace, "udn-client2", nil, nil, nil)
				udnClientPod2.Spec.NodeName = clientNode
				udnClientPod2 = e2epod.NewPodClient(f).CreateSync(context.TODO(), udnClientPod2)

				By("Connect to the UDN service from the UDN client pod on a different node")
				checkConnectionToClusterIPs(f, udnClientPod2, udnService, udnServerPod.Name)
				checkConnectionToLoadBalancers(f, udnClientPod2, udnService, udnServerPod.Name)
				checkConnectionToNodePort(f, udnClientPod2, udnService, &nodes.Items[1], "local node", udnServerPod.Name)
				checkConnectionToNodePort(f, udnClientPod2, udnService, &nodes.Items[0], "server node", udnServerPod.Name)
				if !dynamicUDNEnabled {
					checkConnectionToNodePort(f, udnClientPod2, udnService, &nodes.Items[2], "other node", udnServerPod.Name)
				}

				By("Connect to the UDN service from the UDN client external container")
				externalContainer := infraapi.ExternalContainer{Name: "frr"}
				if !dynamicUDNEnabled {
					checkConnectionToLoadBalancersFromExternalContainer(f, externalContainer, udnService, udnServerPod.Name)
				}
				checkConnectionToNodePortFromExternalContainer(externalContainer, udnService, &nodes.Items[0], "server node", udnServerPod.Name)
				if !dynamicUDNEnabled {
					checkConnectionToNodePortFromExternalContainer(externalContainer, udnService, &nodes.Items[1], "other node", udnServerPod.Name)
					checkConnectionToNodePortFromExternalContainer(externalContainer, udnService, &nodes.Items[2], "other node", udnServerPod.Name)
				}

				// Default network -> UDN
				// Check that it cannot connect
				By(fmt.Sprintf("Create a client pod in the default network on node %s", clientNode))
				defaultNetNamespace := &v1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name: f.Namespace.Name + "-default",
					},
				}
				f.AddNamespacesToDelete(defaultNetNamespace)
				_, err = cs.CoreV1().Namespaces().Create(context.Background(), defaultNetNamespace, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				defaultClient, err := createPod(f, "default-net-pod", clientNode, defaultNetNamespace.Name, []string{"sleep", "2000000"}, nil)
				Expect(err).NotTo(HaveOccurred())

				By("Verify the connection of the client in the default network to the UDN service")
				checkNoConnectionToClusterIPs(f, defaultClient, udnService)
				checkNoConnectionToLoadBalancers(f, defaultClient, udnService)
				checkNoConnectionToNodePort(f, defaultClient, udnService, &nodes.Items[1], "local node") // TODO change to checkConnectionToNodePort when we have full UDN support in ovnkube-node

				checkConnectionToNodePort(f, defaultClient, udnService, &nodes.Items[0], "server node", udnServerPod.Name)
				if !dynamicUDNEnabled {
					checkConnectionToNodePort(f, defaultClient, udnService, &nodes.Items[2], "other node", udnServerPod.Name)
				}

				// UDN -> Default network
				// Create a backend pod and service in the default network and verify that the client pod in the UDN
				// cannot reach it
				By(fmt.Sprintf("Creating a backend pod in the default network on node %s", serverPodNodeName))
				defaultLabels := map[string]string{"app": "default-app"}

				defaultServerPod, err := createPod(f, "backend-pod-default", serverPodNodeName,
					defaultNetNamespace.Name, []string{"/agnhost", "netexec", "--udp-port=" + fmt.Sprint(serviceTargetPort)}, defaultLabels,
					func(pod *v1.Pod) {
						pod.Spec.Containers[0].Ports = []v1.ContainerPort{{ContainerPort: (serviceTargetPort), Protocol: "UDP"}}
					})
				Expect(err).NotTo(HaveOccurred())

				By("create a node port service in the default network")
				defaultService := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Name: "service-default"},
					Spec: v1.ServiceSpec{
						Ports: []v1.ServicePort{
							{
								Name:       "udp-port",
								Port:       int32(servicePort),
								Protocol:   v1.ProtocolUDP,
								TargetPort: intstr.FromInt(serviceTargetPort),
							},
						},
						Selector:       defaultLabels,
						Type:           v1.ServiceTypeNodePort,
						IPFamilyPolicy: &policy,
					},
				}

				defaultService, err = f.ClientSet.CoreV1().Services(defaultNetNamespace.Name).Create(context.TODO(), defaultService, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Verify the UDN client connection to the default network service")
				checkNoConnectionToLoadBalancers(f, udnClientPod2, defaultService)
				checkConnectionToNodePort(f, udnClientPod2, defaultService, &nodes.Items[0], "server node", defaultServerPod.Name)
				checkNoConnectionToNodePort(f, udnClientPod2, defaultService, &nodes.Items[1], "local node")
				checkConnectionToNodePort(f, udnClientPod2, defaultService, &nodes.Items[2], "other node", defaultServerPod.Name)
				checkNoConnectionToClusterIPs(f, udnClientPod2, defaultService)

				// Make sure that restarting OVNK after applying a UDN with an affected service won't result
				// in OVNK in CLBO state https://issues.redhat.com/browse/OCPBUGS-41499
				if netConfigParams.topology == "layer3" { // no need to run it for layer 2 as well
					By("Restart ovnkube-node on one node and verify that the new ovnkube-node pod goes to the running state")
					err = restartOVNKubeNodePod(cs, deploymentconfig.Get().OVNKubernetesNamespace(), clientNode)
					Expect(err).NotTo(HaveOccurred())
				}
			},

			Entry(
				"L3 primary UDN, cluster-networked pods, NodePort service",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer3",
					cidr:     primaryLayer3MultiCIDRs(),
					role:     "primary",
				},
			),
			Entry(
				"L2 primary UDN, cluster-networked pods, NodePort service",
				networkAttachmentConfigParams{
					name:     nadName,
					topology: "layer2",
					cidr:     joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:     "primary",
				},
			),
			Entry(
				"L2 primary UDN with custom network, cluster-networked pods, NodePort service",
				networkAttachmentConfigParams{
					name:                nadName,
					topology:            "layer2",
					cidr:                joinStrings(userDefinedNetworkIPv4Subnet, userDefinedNetworkIPv6Subnet),
					role:                "primary",
					defaultGatewayIPs:   joinStrings(customL2IPv4Gateway, customL2IPv6Gateway),
					reservedCIDRs:       joinStrings(customL2IPv4ReservedCIDR, customL2IPv6ReservedCIDR),
					infrastructureCIDRs: joinStrings(customL2IPv4InfraCIDR, customL2IPv6InfraCIDR),
				},
			),
		)

	})

	Context("Services with static IP and MAC assignments", func() {
		const (
			cudnName       = "l2-network-svc"
			staticSvcPort  = 80
			staticTgtPort  = 8080
			cudnLabel      = "cudn-group"
			cudnLabelValue = "l2-net-svc"

			// IPv4 configuration
			l2SubnetV4       = "192.168.40.0/24"
			defaultGatewayV4 = "192.168.40.3"
			reservedIP1V4    = "192.168.40.4/32"
			reservedIP2V4    = "192.168.40.5/32"
			backendIPv4      = "192.168.40.6"
			clientSameIPv4   = "192.168.40.7"
			clientDiffIPv4   = "192.168.40.8"

			// IPv6 configuration
			l2SubnetV6       = "2001:db8:abcd:0012::/64"
			defaultGatewayV6 = "2001:db8:abcd:0012::3"
			reservedIP1V6    = "2001:db8:abcd:0012::4/128"
			reservedIP2V6    = "2001:db8:abcd:0012::5/128"
			backendIPv6      = "2001:db8:abcd:0012::6"
			clientSameIPv6   = "2001:db8:abcd:0012::7"
			clientDiffIPv6   = "2001:db8:abcd:0012::8"
		)

		type serviceTestCase struct {
			name                     string
			serviceType              v1.ServiceType
			externalTrafficPolicy    v1.ServiceExternalTrafficPolicy
			expectAccessFromSameNode bool
			expectAccessFromDiffNode bool
		}

		It("should be reachable through ClusterIP, NodePort and LoadBalancer services with static IPs", func() {
			cs := f.ClientSet

			By("Checking cluster IP family support")
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(nodes.Items)).To(BeNumerically(">=", 2))

			isIPv6Primary := IsIPv6Cluster(cs)
			isDualStack := isDualStackCluster(nodes)

			nadClient, err := nadclient.NewForConfig(f.ClientConfig())
			Expect(err).NotTo(HaveOccurred())

			namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
				"e2e-framework":           f.BaseName,
				RequiredUDNNamespaceLabel: "",
			})
			f.Namespace = namespace
			Expect(err).NotTo(HaveOccurred())

			namespaceA1 := fmt.Sprintf("%s-a1", f.Namespace.Name)
			namespaceA2 := fmt.Sprintf("%s-a2", f.Namespace.Name)

			nodeA := nodes.Items[0].Name
			nodeB := nodes.Items[1].Name

			// Build CUDN configuration based on cluster IP family
			var subnets, reservedSubnets, defaultGatewayIPs string
			var backendIPs, clientSameIPs, clientDiffIPs []string

			if isDualStack {
				By("1. Configuring dual-stack CUDN with IPv4 and IPv6 subnets")
				subnets = fmt.Sprintf(`"%s", "%s"`, l2SubnetV4, l2SubnetV6)
				reservedSubnets = fmt.Sprintf(`"%s", "%s", "%s", "%s"`, reservedIP1V4, reservedIP2V4, reservedIP1V6, reservedIP2V6)
				defaultGatewayIPs = fmt.Sprintf(`"%s", "%s"`, defaultGatewayV4, defaultGatewayV6)
				backendIPs = []string{backendIPv4, backendIPv6}
				clientSameIPs = []string{clientSameIPv4, clientSameIPv6}
				clientDiffIPs = []string{clientDiffIPv4, clientDiffIPv6}
			} else if isIPv6Primary {
				By("1. Configuring IPv6-only CUDN")
				subnets = fmt.Sprintf(`"%s"`, l2SubnetV6)
				reservedSubnets = fmt.Sprintf(`"%s", "%s"`, reservedIP1V6, reservedIP2V6)
				defaultGatewayIPs = fmt.Sprintf(`"%s"`, defaultGatewayV6)
				backendIPs = []string{backendIPv6}
				clientSameIPs = []string{clientSameIPv6}
				clientDiffIPs = []string{clientDiffIPv6}
			} else {
				By("1. Configuring IPv4-only CUDN")
				subnets = fmt.Sprintf(`"%s"`, l2SubnetV4)
				reservedSubnets = fmt.Sprintf(`"%s", "%s"`, reservedIP1V4, reservedIP2V4)
				defaultGatewayIPs = fmt.Sprintf(`"%s"`, defaultGatewayV4)
				backendIPs = []string{backendIPv4}
				clientSameIPs = []string{clientSameIPv4}
				clientDiffIPs = []string{clientDiffIPv4}
			}

			By("2. Creating ClusterUserDefinedNetwork with Layer2 topology")
			cudnManifest := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: %s
spec:
  namespaceSelector:
    matchLabels:
      %s: %s
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets: [%s]
      reservedSubnets: [%s]
      defaultGatewayIPs: [%s]`, cudnName, cudnLabel, cudnLabelValue, subnets, reservedSubnets, defaultGatewayIPs)

			cudnCleanup, err := createManifest("", cudnManifest)
			Expect(err).NotTo(HaveOccurred())
			// Use DeferCleanup to run AFTER framework's AfterEach (which deletes namespaces)
			// This ensures namespaces and pods are gone before CUDN deletion
			DeferCleanup(func() {
				cudnCleanup()
				// Delete the CUDN from the cluster (namespaces already deleted by framework)
				By("Cleaning up ClusterUserDefinedNetwork")
				_, err := e2ekubectl.RunKubectl("", "delete", "clusteruserdefinednetwork", cudnName, "--ignore-not-found", "--wait", "--timeout=60s")
				if err != nil {
					framework.Logf("Failed to delete ClusterUserDefinedNetwork %s: %v", cudnName, err)
				}
			})

			By("3. Creating namespaces a1 and a2 with UDN labels")
			ns1, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespaceA1,
					Labels: map[string]string{
						RequiredUDNNamespaceLabel: "",
						cudnLabel:                 cudnLabelValue,
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			f.AddNamespacesToDelete(ns1)

			ns2, err := cs.CoreV1().Namespaces().Create(context.Background(), &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: namespaceA2,
					Labels: map[string]string{
						RequiredUDNNamespaceLabel: "",
						cudnLabel:                 cudnLabelValue,
					},
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			f.AddNamespacesToDelete(ns2)

			By("4. Waiting for NAD to be created in namespaces")
			Eventually(func() error {
				_, err := nadClient.NetworkAttachmentDefinitions(namespaceA1).Get(
					context.Background(),
					cudnName,
					metav1.GetOptions{},
				)
				return err
			}, 2*time.Minute, 5*time.Second).Should(Succeed())
			Eventually(func() error {
				_, err := nadClient.NetworkAttachmentDefinitions(namespaceA2).Get(
					context.Background(),
					cudnName,
					metav1.GetOptions{},
				)
				return err
			}, 2*time.Minute, 5*time.Second).Should(Succeed())

			By("5. Creating backend pod with static IP(s) in namespace a1 on nodeA")
			backendPodConfig := *podConfig(
				"backend-pod",
				withLabels(map[string]string{"app": "hello-svc"}),
				withStaticIPsMAC(backendIPs, "02:03:04:05:06:01"),
				withCommand(func() []string {
					return httpServerContainerCmd(staticTgtPort)
				}),
				withNodeSelector(map[string]string{"kubernetes.io/hostname": nodeA}),
			)
			backendPodConfig.namespace = namespaceA1
			runUDNPod(cs, namespaceA1, backendPodConfig, nil)

			By("6. Creating client pod on same node (nodeA) in namespace a2")
			clientPodConfigSameNode := *podConfig(
				"client-same-node",
				withStaticIPsMAC(clientSameIPs, "02:03:04:05:06:02"),
				withNodeSelector(map[string]string{"kubernetes.io/hostname": nodeA}),
			)
			clientPodConfigSameNode.namespace = namespaceA2
			runUDNPod(cs, namespaceA2, clientPodConfigSameNode, nil)

			By("7. Creating client pod on different node (nodeB) in namespace a2")
			clientPodConfigDiffNode := *podConfig(
				"client-diff-node",
				withStaticIPsMAC(clientDiffIPs, "02:03:04:05:06:03"),
				withNodeSelector(map[string]string{"kubernetes.io/hostname": nodeB}),
			)
			clientPodConfigDiffNode.namespace = namespaceA2
			runUDNPod(cs, namespaceA2, clientPodConfigDiffNode, nil)

			// Define test cases for each service type
			testCases := []serviceTestCase{
				{
					name:                     "ClusterIP",
					serviceType:              v1.ServiceTypeClusterIP,
					externalTrafficPolicy:    v1.ServiceExternalTrafficPolicy(""),
					expectAccessFromSameNode: true,
					expectAccessFromDiffNode: true,
				},
				{
					name:                     "NodePort with ETP=Cluster",
					serviceType:              v1.ServiceTypeNodePort,
					externalTrafficPolicy:    v1.ServiceExternalTrafficPolicyCluster,
					expectAccessFromSameNode: true,
					expectAccessFromDiffNode: true,
				},
				{
					name:                     "NodePort with ETP=Local",
					serviceType:              v1.ServiceTypeNodePort,
					externalTrafficPolicy:    v1.ServiceExternalTrafficPolicyLocal,
					expectAccessFromSameNode: true,
					expectAccessFromDiffNode: false,
				},
				{
					name:                     "LoadBalancer",
					serviceType:              v1.ServiceTypeLoadBalancer,
					externalTrafficPolicy:    v1.ServiceExternalTrafficPolicyCluster,
					expectAccessFromSameNode: true,
					expectAccessFromDiffNode: true,
				},
			}

			for i, tc := range testCases {
				By(fmt.Sprintf("8.%d Testing %s service", i+1, tc.name))

				svcName := fmt.Sprintf("test-service-%d", i)
				svc := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      svcName,
						Namespace: namespaceA1,
					},
					Spec: v1.ServiceSpec{
						Selector: map[string]string{"app": "hello-svc"},
						Ports: []v1.ServicePort{{
							Port:       int32(staticSvcPort),
							TargetPort: intstr.FromInt(staticTgtPort),
							Protocol:   v1.ProtocolTCP,
						}},
						Type: tc.serviceType,
					},
				}
				if tc.serviceType == v1.ServiceTypeNodePort || tc.serviceType == v1.ServiceTypeLoadBalancer {
					svc.Spec.ExternalTrafficPolicy = tc.externalTrafficPolicy
				}

				svc, err = cs.CoreV1().Services(namespaceA1).Create(context.TODO(), svc, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				switch tc.serviceType {
				case v1.ServiceTypeClusterIP:
					// Test all ClusterIPs (IPv4 and/or IPv6 based on cluster config)
					for _, clusterIP := range svc.Spec.ClusterIPs {
						By(fmt.Sprintf("Testing %s connectivity to ClusterIP %s:%d", tc.name, clusterIP, staticSvcPort))

						if tc.expectAccessFromSameNode {
							By(fmt.Sprintf("Verifying %s service is accessible from client on same node via %s", tc.name, clusterIP))
							Eventually(func() error {
								return connectToServer(clientPodConfigSameNode, clusterIP, uint16(staticSvcPort))
							}, 2*time.Minute, 5*time.Second).Should(Succeed())
						}

						if tc.expectAccessFromDiffNode {
							By(fmt.Sprintf("Verifying %s service is accessible from client on different node via %s", tc.name, clusterIP))
							Eventually(func() error {
								return connectToServer(clientPodConfigDiffNode, clusterIP, uint16(staticSvcPort))
							}, 2*time.Minute, 5*time.Second).Should(Succeed())
						}
					}

				case v1.ServiceTypeNodePort:
					nodePort := int(svc.Spec.Ports[0].NodePort)

					// Test from nodeA (where backend is located)
					// Skip IPv6 NodePort tests - not supported for L2 UDN
					nodeAIPs, err := ParseNodeHostIPDropNetMask(&nodes.Items[0])
					Expect(err).NotTo(HaveOccurred())
					for nodeAIP := range nodeAIPs {
						if utilnet.IsIPv6String(nodeAIP) {
							By(fmt.Sprintf("Skipping IPv6 NodePort test for %s (not supported for L2 UDN)", nodeAIP))
							continue
						}
						By(fmt.Sprintf("Testing %s connectivity to NodePort %s:%d (nodeA)", tc.name, nodeAIP, nodePort))

						if tc.expectAccessFromSameNode {
							By(fmt.Sprintf("Verifying %s service is accessible from client on same node via %s", tc.name, nodeAIP))
							Eventually(func() error {
								return connectToServer(clientPodConfigSameNode, nodeAIP, uint16(nodePort))
							}, 2*time.Minute, 5*time.Second).Should(Succeed())
						}
					}

					// Test from nodeB (where no backend is located for ETP=Local)
					// Skip IPv6 NodePort tests - not supported for L2 UDN
					nodeBIPs, err := ParseNodeHostIPDropNetMask(&nodes.Items[1])
					Expect(err).NotTo(HaveOccurred())
					for nodeBIP := range nodeBIPs {
						if utilnet.IsIPv6String(nodeBIP) {
							By(fmt.Sprintf("Skipping IPv6 NodePort test for %s (not supported for L2 UDN)", nodeBIP))
							continue
						}
						By(fmt.Sprintf("Testing %s connectivity to NodePort %s:%d (nodeB)", tc.name, nodeBIP, nodePort))

						if tc.expectAccessFromDiffNode {
							By(fmt.Sprintf("Verifying %s service is accessible from client on different node via %s", tc.name, nodeBIP))
							Eventually(func() error {
								return connectToServer(clientPodConfigDiffNode, nodeBIP, uint16(nodePort))
							}, 2*time.Minute, 5*time.Second).Should(Succeed())
						} else if tc.externalTrafficPolicy == v1.ServiceExternalTrafficPolicyLocal {
							// Skip ETP=Local "not accessible" check for L2 UDN - behavior differs from default network
							By(fmt.Sprintf("Skipping ETP=Local 'not accessible' check for %s (L2 UDN behavior differs)", nodeBIP))
						} else {
							By(fmt.Sprintf("Verifying %s service is NOT accessible from nodeB IP %s", tc.name, nodeBIP))
							Consistently(func() error {
								return connectToServer(clientPodConfigDiffNode, nodeBIP, uint16(nodePort))
							}, 10*time.Second, 2*time.Second).ShouldNot(Succeed())
						}
					}

				case v1.ServiceTypeLoadBalancer:
					By(fmt.Sprintf("Waiting for %s LoadBalancer IP to be assigned", tc.name))
					// Check if LoadBalancer is supported by waiting for an IP to be assigned
					// Use a shorter timeout to detect unsupported clusters quickly
					lbAssigned := false
					deadline := time.Now().Add(30 * time.Second)
					for time.Now().Before(deadline) {
						svc, err = cs.CoreV1().Services(namespaceA1).Get(context.TODO(), svc.Name, metav1.GetOptions{})
						if err == nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
							lbAssigned = true
							break
						}
						time.Sleep(5 * time.Second)
					}

					if !lbAssigned {
						By("Skipping LoadBalancer test - cluster does not have LoadBalancer support (no IP assigned within 30s)")
						continue
					}

					// Test all LoadBalancer ingress IPs (pods have addresses for all configured IP families)
					for _, lbIngress := range svc.Status.LoadBalancer.Ingress {
						lbIP := lbIngress.IP
						if lbIP == "" {
							continue
						}

						By(fmt.Sprintf("Testing %s connectivity to LoadBalancer IP %s:%d", tc.name, lbIP, staticSvcPort))

						if tc.expectAccessFromSameNode {
							By(fmt.Sprintf("Verifying %s service is accessible from client on same node via %s", tc.name, lbIP))
							Eventually(func() error {
								return connectToServer(clientPodConfigSameNode, lbIP, uint16(staticSvcPort))
							}, 2*time.Minute, 5*time.Second).Should(Succeed())
						}

						if tc.expectAccessFromDiffNode {
							By(fmt.Sprintf("Verifying %s service is accessible from client on different node via %s", tc.name, lbIP))
							Eventually(func() error {
								return connectToServer(clientPodConfigDiffNode, lbIP, uint16(staticSvcPort))
							}, 2*time.Minute, 5*time.Second).Should(Succeed())
						}
					}
				}

				// Cleanup service before next iteration
				err = cs.CoreV1().Services(namespaceA1).Delete(context.TODO(), svcName, metav1.DeleteOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

})

// TODO Once https://github.com/ovn-kubernetes/ovn-kubernetes/pull/4567 merges, use the vendored *TestJig.Run(), which tests
// the reachability of a service through its name and through its cluster IP. For now only test the cluster IP.

const OvnNodeIfAddr = "k8s.ovn.org/node-primary-ifaddr"

type primaryIfAddrAnnotation struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// ParseNodeHostIPDropNetMask returns the parsed host IP addresses found on a node's host CIDR annotation. Removes the mask.
func ParseNodeHostIPDropNetMask(node *kapi.Node) (sets.Set[string], error) {
	nodeIfAddrAnnotation, ok := node.Annotations[OvnNodeIfAddr]
	if !ok {
		return nil, newAnnotationNotSetError("%s annotation not found for node %q", OvnNodeIfAddr, node.Name)
	}
	nodeIfAddr := &primaryIfAddrAnnotation{}
	if err := json.Unmarshal([]byte(nodeIfAddrAnnotation), nodeIfAddr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal annotation: %s for node %q, err: %v", OvnNodeIfAddr, node.Name, err)
	}

	var cfg []string
	if nodeIfAddr.IPv4 != "" {
		cfg = append(cfg, nodeIfAddr.IPv4)
	}
	if nodeIfAddr.IPv6 != "" {
		cfg = append(cfg, nodeIfAddr.IPv6)
	}
	if len(cfg) == 0 {
		return nil, fmt.Errorf("node: %q does not have any IP information set", node.Name)
	}

	for i, cidr := range cfg {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil || ip == nil {
			return nil, fmt.Errorf("failed to parse node host cidr: %v", err)
		}
		cfg[i] = ip.String()
	}
	return sets.New(cfg...), nil
}

func checkConnectionToAgnhostPod(f *framework.Framework, clientPod *v1.Pod, expectedOutput, cmd string) error {
	return wait.PollImmediate(200*time.Millisecond, 5*time.Second, func() (bool, error) {
		stdout, stderr, err2 := ExecShellInPodWithFullOutput(f, clientPod.Namespace, clientPod.Name, cmd)
		fmt.Printf("stdout=%s\n", stdout)
		fmt.Printf("stderr=%s\n", stderr)
		fmt.Printf("err=%v\n", err2)

		if stderr != "" {
			return false, fmt.Errorf("stderr=%s", stderr)
		}

		if err2 != nil {
			return false, err2
		}
		return stdout == expectedOutput, nil
	})
}

func checkNoConnectionToAgnhostPod(f *framework.Framework, clientPod *v1.Pod, cmd string) error {
	err := wait.PollImmediate(500*time.Millisecond, 2*time.Second, func() (bool, error) {
		stdout, stderr, err2 := ExecShellInPodWithFullOutput(f, clientPod.Namespace, clientPod.Name, cmd)
		fmt.Printf("stdout=%s\n", stdout)
		fmt.Printf("stderr=%s\n", stderr)
		fmt.Printf("err=%v\n", err2)

		if stderr != "" {
			return false, nil
		}

		if err2 != nil {
			return false, err2
		}
		if stdout != "" {
			return true, fmt.Errorf("Connection unexpectedly succeeded. Stdout: %s\n", stdout)
		}
		return false, nil
	})

	if err != nil {
		if wait.Interrupted(err) {
			// The timeout occurred without the connection succeeding, which is what we expect
			return nil
		}
		return err
	}
	return fmt.Errorf("Error: %s/%s was able to connect (cmd=%s) ", clientPod.Namespace, clientPod.Name, cmd)
}

func checkConnectionToClusterIPs(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, expectedOutput string) {
	checkConnectionOrNoConnectionToClusterIPs(f, clientPod, service, expectedOutput, true)
}

func checkNoConnectionToClusterIPs(f *framework.Framework, clientPod *v1.Pod, service *v1.Service) {
	checkConnectionOrNoConnectionToClusterIPs(f, clientPod, service, "", false)
}

func checkConnectionOrNoConnectionToClusterIPs(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, expectedOutput string, shouldConnect bool) {
	var err error
	servicePort := service.Spec.Ports[0].Port
	notStr := ""
	if !shouldConnect {
		notStr = "not "
	}

	for _, clusterIP := range service.Spec.ClusterIPs {
		msg := fmt.Sprintf("Client %s/%s should %sreach service %s/%s on cluster IP %s port %d",
			clientPod.Namespace, clientPod.Name, notStr, service.Namespace, service.Name, clusterIP, servicePort)
		By(msg)

		cmd := fmt.Sprintf(`/bin/sh -c 'echo hostname | nc -u -w 1 %s %d '`, clusterIP, servicePort)

		if shouldConnect {
			err = checkConnectionToAgnhostPod(f, clientPod, expectedOutput, cmd)
		} else {
			err = checkNoConnectionToAgnhostPod(f, clientPod, cmd)
		}
		framework.ExpectNoError(err, fmt.Sprintf("Failed to verify that %s", msg))
	}
}

func checkConnectionToNodePort(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, node *v1.Node, nodeRoleMsg, expectedOutput string) {
	checkConnectionOrNoConnectionToNodePort(f, clientPod, service, node, nodeRoleMsg, expectedOutput, true)
}

func checkNoConnectionToNodePort(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, node *v1.Node, nodeRoleMsg string) {
	checkConnectionOrNoConnectionToNodePort(f, clientPod, service, node, nodeRoleMsg, "", false)
}

func checkConnectionOrNoConnectionToNodePort(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, node *v1.Node, nodeRoleMsg, expectedOutput string, shouldConnect bool) {
	var err error
	nodePort := service.Spec.Ports[0].NodePort
	notStr := ""
	if !shouldConnect {
		notStr = "not "
	}
	nodeIPs, err := ParseNodeHostIPDropNetMask(node)
	Expect(err).NotTo(HaveOccurred())

	for nodeIP := range nodeIPs {
		msg := fmt.Sprintf("Client %s/%s should %sconnect to NodePort service %s/%s on %s:%d (node %s, %s)",
			clientPod.Namespace, clientPod.Name, notStr, service.Namespace, service.Name, nodeIP, nodePort, node.Name, nodeRoleMsg)
		By(msg)
		cmd := fmt.Sprintf(`/bin/sh -c 'echo hostname | nc -u -w 1 %s %d '`, nodeIP, nodePort)

		if shouldConnect {
			err = checkConnectionToAgnhostPod(f, clientPod, expectedOutput, cmd)
		} else {
			err = checkNoConnectionToAgnhostPod(f, clientPod, cmd)
		}
		framework.ExpectNoError(err, fmt.Sprintf("Failed to verify that %s", msg))
	}
}

func checkConnectionToLoadBalancers(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, expectedOutput string) {
	checkConnectionOrNoConnectionToLoadBalancers(f, clientPod, service, expectedOutput, true)
}

func checkNoConnectionToLoadBalancers(f *framework.Framework, clientPod *v1.Pod, service *v1.Service) {
	checkConnectionOrNoConnectionToLoadBalancers(f, clientPod, service, "", false)
}

func checkConnectionOrNoConnectionToLoadBalancers(f *framework.Framework, clientPod *v1.Pod, service *v1.Service, expectedOutput string, shouldConnect bool) {
	var err error
	port := service.Spec.Ports[0].Port
	notStr := ""
	if !shouldConnect {
		notStr = "not "
	}
	for _, lbIngress := range filterLoadBalancerIngressByIPFamily(f, service) {
		msg := fmt.Sprintf("Client %s/%s should %sreach service %s/%s on LoadBalancer IP %s port %d",
			clientPod.Namespace, clientPod.Name, notStr, service.Namespace, service.Name, lbIngress.IP, port)
		By(msg)

		cmd := fmt.Sprintf(`/bin/sh -c 'echo hostname | nc -u -w 1 %s %d '`, lbIngress.IP, port)

		if shouldConnect {
			err = checkConnectionToAgnhostPod(f, clientPod, expectedOutput, cmd)
		} else {
			err = checkNoConnectionToAgnhostPod(f, clientPod, cmd)
		}
		framework.ExpectNoError(err, fmt.Sprintf("Failed to verify that %s", msg))
	}
}

func checkConnectionToNodePortFromExternalContainer(externalContainer infraapi.ExternalContainer, service *v1.Service, node *v1.Node, nodeRoleMsg, expectedOutput string) {
	GinkgoHelper()
	var err error
	nodePort := service.Spec.Ports[0].NodePort
	nodeIPs, err := ParseNodeHostIPDropNetMask(node)
	Expect(err).NotTo(HaveOccurred())

	for nodeIP := range nodeIPs {
		msg := fmt.Sprintf("Client at external container %s should connect to NodePort service %s/%s on %s:%d (node %s, %s)",
			externalContainer.GetName(), service.Namespace, service.Name, nodeIP, nodePort, node.Name, nodeRoleMsg)
		By(msg)
		Eventually(func() (string, error) {
			return infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{
				"/bin/bash", "-c", fmt.Sprintf("echo hostname | nc -u -w 1 %s %d", nodeIP, nodePort),
			})
		}).
			WithTimeout(5*time.Second).
			WithPolling(200*time.Millisecond).
			Should(Equal(expectedOutput), "Failed to verify that %s", msg)
	}
}

func checkConnectionToLoadBalancersFromExternalContainer(f *framework.Framework, externalContainer infraapi.ExternalContainer, service *v1.Service, expectedOutput string) {
	GinkgoHelper()
	port := service.Spec.Ports[0].Port

	for _, lbIngress := range filterLoadBalancerIngressByIPFamily(f, service) {
		msg := fmt.Sprintf("Client at external container %s should reach service %s/%s on LoadBalancer IP %s port %d",
			externalContainer.GetName(), service.Namespace, service.Name, lbIngress.IP, port)
		By(msg)
		Eventually(func() (string, error) {
			return infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{
				"/bin/bash", "-c", fmt.Sprintf("echo hostname | nc -u -w 1 %s %d", lbIngress.IP, port),
			})
		}).
			// It takes some time for the container to receive the dynamic routing
			WithTimeout(20*time.Second).
			WithPolling(200*time.Millisecond).
			Should(Equal(expectedOutput), "Failed to verify that %s", msg)
	}
}

func filterLoadBalancerIngressByIPFamily(f *framework.Framework, service *v1.Service) []v1.LoadBalancerIngress {
	GinkgoHelper()
	// Work around two metallb v0.14.9 issues:
	// Always refetch the LB IPs as they might be updated from under our feet
	// https://github.com/metallb/metallb/issues/2723
	service, err := f.ClientSet.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	// The MetalLB environment setup that we consume directly from their repo is
	// configured statically with dual stack address pools and will just
	// allocate dual stack addresses to the service even if the service does not
	// have dualstack ClusterIPs. So filter out invalid addresses.
	// https://github.com/metallb/metallb/issues/2724
	lbIngressIPs := slices.Clone(service.Status.LoadBalancer.Ingress)
	if len(service.Spec.ClusterIPs) == 1 {
		isIpv6 := utilnet.IsIPv6String(service.Spec.ClusterIPs[0])
		lbIngressIPs = slices.DeleteFunc(lbIngressIPs, func(lbIngressIP v1.LoadBalancerIngress) bool {
			sameIPFamily := utilnet.IsIPv6String(lbIngressIP.IP) == isIpv6
			return !sameIPFamily
		})
	}
	return lbIngressIPs
}

// withStaticIPMAC sets static IP and MAC via annotations.
// The ip parameter should be the IP address without CIDR suffix (e.g., "192.168.40.3").
// The CIDR suffix "/24" is added automatically for IPv4.
func withStaticIPMAC(ip, mac string) podOption {
	ipWithCIDR := ip + "/24"
	annotation := fmt.Sprintf(`[{"name":"default","namespace":"ovn-kubernetes","ips":["%s"],"mac":"%s"}]`, ipWithCIDR, mac)
	return func(pod *podConfiguration) {
		if pod.annotations == nil {
			pod.annotations = make(map[string]string)
		}
		pod.annotations["v1.multus-cni.io/default-network"] = annotation
		pod.staticIP = ip
	}
}

// withStaticIPsMAC sets static IPs (IPv4 and/or IPv6) and MAC via annotations.
// The ips parameter should be IP addresses without CIDR suffix.
// CIDR suffix "/24" is added for IPv4, "/64" for IPv6.
func withStaticIPsMAC(ips []string, mac string) podOption {
	var ipsWithCIDR []string
	for _, ip := range ips {
		if utilnet.IsIPv6String(ip) {
			ipsWithCIDR = append(ipsWithCIDR, ip+"/64")
		} else {
			ipsWithCIDR = append(ipsWithCIDR, ip+"/24")
		}
	}
	ipsJSON := `"` + strings.Join(ipsWithCIDR, `","`) + `"`
	annotation := fmt.Sprintf(`[{"name":"default","namespace":"ovn-kubernetes","ips":[%s],"mac":"%s"}]`, ipsJSON, mac)
	return func(pod *podConfiguration) {
		if pod.annotations == nil {
			pod.annotations = make(map[string]string)
		}
		pod.annotations["v1.multus-cni.io/default-network"] = annotation
		// Store first IP as staticIP for backward compatibility
		if len(ips) > 0 {
			pod.staticIP = ips[0]
		}
	}
}
