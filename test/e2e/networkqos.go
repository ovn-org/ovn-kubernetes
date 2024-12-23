package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = ginkgo.Describe("e2e NetworkQoS validation", func() {
	const (
		podImage       = "nicolaka/netshoot"
		networkQoSYaml = "networkqos.yaml"
		nqosSpecName   = "nqos-test-spec"
		srcPodName     = "src-nqos-pod"
		tcpdumpIPv4    = "(ip and (ip[1] & 0xfc) >> 2 == %d)"
		tcpdumpIPv6    = "(ip6 and (ip6[0:2] & 0xfc0) >> 6 == %d)"
		dstPod1Name    = "nqos-dst-pod1"
		dstPod2Name    = "nqos-dst-pod2"
		dstPod3Name    = "nqos-dst-pod3"
		dstPod4Name    = "nqos-dst-pod4"

		bandwidthFluctuation = 1.5
	)

	var (
		skipIpv4        bool
		skipIpv6        bool
		dstPodNamespace string
		dstNode         string
		dstPod1IPv4     string
		dstPod1IPv6     string
		dstPod2IPv4     string
		dstPod2IPv6     string
		dstPod3IPv4     string
		dstPod3IPv6     string
		dstPod4IPv4     string
		dstPod4IPv6     string
		nodeIPv4Range   string
		nodeIPv6Range   string
	)

	f := wrappedTestFramework("networkqos")

	waitForNetworkQoSApplied := func(namespace string) {
		gomega.Eventually(func() bool {
			output, err := e2ekubectl.RunKubectl(namespace, "get", "networkqos", nqosSpecName)
			if err != nil {
				framework.Failf("could not get the networkqos default in namespace: %s", namespace)
			}
			return strings.Contains(output, "NetworkQoS Destinations applied")
		}, 10*time.Second).Should(gomega.BeTrue(), fmt.Sprintf("expected networkqos in namespace %s to be successfully applied", namespace))
	}

	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		nodeAddresses := map[string]string{}
		err = json.Unmarshal([]byte(nodes.Items[0].Annotations["k8s.ovn.org/node-primary-ifaddr"]), &nodeAddresses)
		framework.ExpectNoError(err)
		if nodeIP, ok := nodeAddresses["ipv4"]; ok {
			_, ipnet, _ := net.ParseCIDR(nodeIP)
			nodeIPv4Range = ipnet.String()
			skipIpv4 = false
		} else {
			framework.Fail("Node IPv4 address not found")
			ginkgo.By("Node IPv4 address not found: Will be skipping IPv4 checks in the Networking QoS test")
			nodeIPv4Range = "0.0.0.0/0"
			skipIpv4 = true
		}
		if nodeIP, ok := nodeAddresses["ipv6"]; ok {
			_, ipnet, _ := net.ParseCIDR(nodeIP)
			nodeIPv6Range = ipnet.String()
			skipIpv6 = false
		} else {
			ginkgo.By("Node IPv6 address not found: Will be skipping IPv6 checks in the Networking QoS test")
			nodeIPv6Range = "::/0"
			skipIpv6 = true
		}
		dstPodNamespace = f.Namespace.Name + "-dest"
		// set up dest namespace
		dstNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: dstPodNamespace,
				Labels: map[string]string{
					"app": "nqos-test",
				},
			},
		}
		_, err = f.ClientSet.CoreV1().Namespaces().Create(context.Background(), dstNs, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Error creating Namespace %v: %v", dstPodNamespace, err)

		_, err = createPod(f, srcPodName, nodes.Items[0].Name, f.Namespace.Name, []string{"bash", "-c", "sleep infinity"}, map[string]string{"component": "nqos-test-src"}, func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = podImage
		})
		framework.ExpectNoError(err)
		dstNode = nodes.Items[1].Name
	})

	ginkgo.DescribeTable("Should have correct DSCP value for overlay traffic when NetworkQoS is applied",
		func(skipThisTableEntry *bool, tcpDumpTpl string, dst1IP, dst2IP, dst3IP, dst4IP *string) {
			if *skipThisTableEntry {
				return
			}
			dscpValue := 50
			// dest pod without protocol and port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, map[string]string{"component": "nqos-test-dst"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)

			// dest pod covered by tcp without port rule
			dstPod2, err := createPod(f, dstPod2Name, dstNode, dstPodNamespace, []string{"bash", "-c", "nc -l -p 9090; sleep infinity"}, map[string]string{"component": "nqos-test-tcp"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "nc")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)

			// dest pod covered by tcp with port rule
			dstPod3, err := createPod(f, dstPod3Name, dstNode, dstPodNamespace, []string{"bash", "-c", "python3 -m http.server 80; sleep infinity"}, map[string]string{"component": "nqos-test-web"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "python3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod3IPv4, dstPod3IPv6 = getPodAddresses(dstPod3)

			// dest pod not covered by networkqos
			dstPod4, err := createPod(f, dstPod4Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod4IPv4, dstPod4IPv6 = getPodAddresses(dstPod4)

			// no dscp (dscp == 0) should be deteced before networkqos is applied
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, 0)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)

			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  egress:
  - priority: 1001
    dscp: %d
    classifier:
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-dst
        namespaceSelector:
          matchLabels:
            app: nqos-test
  - priority: 1002
    dscp: %d
    classifier:
      port:
        protocol: TCP
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-tcp
        namespaceSelector:
          matchLabels:
           app: nqos-test
  - priority: 1003
    dscp: %d
    classifier:
      port:
        protocol: TCP
        port: 80
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-web
        namespaceSelector:
          matchLabels:
           app: nqos-test
`, f.Namespace.Name, nqosSpecName, dscpValue, dscpValue+1, dscpValue+2)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name)
			// verify dscp
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, dscpValue)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, dscpValue+1)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, dscpValue+2)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)
		},
		ginkgo.Entry("ipv4", &skipIpv4, tcpdumpIPv4, &dstPod1IPv4, &dstPod2IPv4, &dstPod3IPv4, &dstPod4IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, tcpdumpIPv6, &dstPod1IPv6, &dstPod2IPv6, &dstPod3IPv6, &dstPod4IPv6),
	)

	ginkgo.DescribeTable("Should have correct DSCP value for host network traffic when NetworkQoS is applied",
		func(skipThisTableEntry *bool, tcpDumpTpl string, dst1IP, dst2IP, dst3IP, dst4IP *string) {
			if *skipThisTableEntry {
				return
			}
			dscpValue := 32
			// dest pod to test traffic without protocol and port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)

			// dest pod to test traffic with tcp protocol but no port
			dstPod2, err := createPod(f, dstPod2Name, dstNode, dstPodNamespace, []string{"bash", "-c", "nc -l -p 9090; sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "nc")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)

			// dest pod to test traffic with tcp protocol and port
			dstPod3, err := createPod(f, dstPod3Name, dstNode, dstPodNamespace, []string{"bash", "-c", "python3 -m http.server 80; sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "python3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod3IPv4, dstPod3IPv6 = getPodAddresses(dstPod3)

			// dest pod not covered by networkqos
			dstPod4, err := createPod(f, dstPod4Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod4IPv4, dstPod4IPv6 = getPodAddresses(dstPod4)

			// no dscp (dscp == 0) should be deteced before networkqos is applied
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, 0)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)

			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  egress:
  - priority: 1001
    dscp: %d
    classifier:
      to:
      - ipBlock:
          cidr: %s
      - ipBlock:
          cidr: %s
  - priority: 1002
    dscp: %d
    classifier:
      port:
        protocol: TCP
      to:
      - ipBlock:
          cidr: %s
      - ipBlock:
          cidr: %s
  - priority: 1003
    dscp: %d
    classifier:
      port:
        protocol: TCP
        port: 80
      to:
      - ipBlock:
          cidr: %s
      - ipBlock:
          cidr: %s
`, f.Namespace.Name, nqosSpecName, dscpValue, nodeIPv4Range, nodeIPv6Range, dscpValue+1, nodeIPv4Range, nodeIPv6Range, dscpValue+2, nodeIPv4Range, nodeIPv6Range)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name)
			// verify dscp
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, dscpValue)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, dscpValue+1)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, dscpValue+2)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)
		},
		ginkgo.Entry("ipv4", &skipIpv4, tcpdumpIPv4, &dstPod1IPv4, &dstPod2IPv4, &dstPod3IPv4, &dstPod4IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, tcpdumpIPv6, &dstPod1IPv6, &dstPod2IPv6, &dstPod3IPv6, &dstPod4IPv6),
	)

	ginkgo.DescribeTable("Limits egress traffic to all target pods below the specified rate in NetworkQoS spec",
		func(skipThisTableEntry *bool, dst1IP, dst2IP *string) {
			if *skipThisTableEntry {
				return
			}
			rate := 10000
			// dest pod 1 for test without protocol & port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s"}, map[string]string{"component": "nqos-test-dst"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)
			// dest pod 2 for test without protocol & port
			dstPod2, err := createPod(f, dstPod2Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s"}, map[string]string{"component": "nqos-test-dst"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)

			bps := twoStreamIperf3Tests(f, srcPodName, *dst1IP, *dst2IP, 5201)
			gomega.Expect(bps/1000 > float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())

			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  egress:
  - priority: 1001
    dscp: 1
    bandwidth:
      rate: %d
    classifier:
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-dst
        namespaceSelector:
          matchLabels:
            app: nqos-test
`, f.Namespace.Name, nqosSpecName, rate)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name)
			bps = twoStreamIperf3Tests(f, srcPodName, *dst1IP, *dst2IP, 5201)
			gomega.Expect(bps/1000 <= float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
		},
		ginkgo.Entry("ipv4", &skipIpv4, &dstPod1IPv4, &dstPod2IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, &dstPod1IPv6, &dstPod2IPv6),
	)

	ginkgo.DescribeTable("Limits egress traffic targeting an individual pod by protocol through a NetworkQoS spec",
		func(skipThisTableEntry *bool, dst1IP *string) {
			if *skipThisTableEntry {
				return
			}
			rate := 5000
			// dest pod for test with protocol
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s"}, map[string]string{"component": "nqos-test-tcp"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)
			bps := iperf3Test(f, srcPodName, *dst1IP, 5201)
			gomega.Expect(bps/1000 > float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  egress:
  - priority: 1001
    dscp: 2
    bandwidth:
      rate: %d
    classifier:
      port:
        protocol: TCP

      to:
      - podSelector:
          matchLabels:
            component: nqos-test-tcp
        namespaceSelector:
          matchLabels:
           app: nqos-test
`, f.Namespace.Name, nqosSpecName, rate)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name)
			bps = iperf3Test(f, srcPodName, *dst1IP, 5201)
			gomega.Expect(bps/1000 <= float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
		},
		ginkgo.Entry("ipv4", &skipIpv4, &dstPod1IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, &dstPod1IPv6),
	)

	ginkgo.DescribeTable("Limits egress traffic targeting a pod by protocol and port through a NetworkQoS spec",
		func(skipThisTableEntry *bool, dst1IP *string) {
			if *skipThisTableEntry {
				return
			}
			rate := 5000
			// dest pod for test with protocol and port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s -p 80"}, map[string]string{"component": "nqos-test-proto-and-port"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = podImage
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)
			bps := iperf3Test(f, srcPodName, *dst1IP, 80)
			gomega.Expect(bps/1000 > float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  egress:
  - priority: 102
    dscp: 3
    bandwidth:
      rate: %d
    classifier:
      port:
        protocol: TCP
        port: 80
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-proto-and-port
        namespaceSelector:
          matchLabels:
           app: nqos-test
`, f.Namespace.Name, nqosSpecName, rate)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name)
			bps = iperf3Test(f, srcPodName, *dst1IP, 80)
			gomega.Expect(bps/1000 <= float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
		},
		ginkgo.Entry("ipv4", &skipIpv4, &dstPod1IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, &dstPod1IPv6),
	)

	ginkgo.AfterEach(func() {
		err := f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), dstPodNamespace, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "Error deleting Namespace %v: %v", dstPodNamespace, err)
	})
})

func pingExpectDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, dscp int) {
	tcpDumpSync := errgroup.Group{}
	pingSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", pod, "--", "timeout", "10",
			"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
		return err
	}

	pingFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "ping", "-c", "5", dst)
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	pingSync.Go(func() error {
		return pingFromSrcPod(srcPod, dstPodIP)
	})
	err := pingSync.Wait()
	framework.ExpectNoError(err, "Failed to ping dst pod")
	err = tcpDumpSync.Wait()
	framework.ExpectNoError(err, "Failed to detect ping with correct DSCP on pod")
}

func netcatExpectDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, port, dscp int) {
	tcpDumpSync := errgroup.Group{}
	netcatSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", pod, "--", "timeout", "10",
			"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
		return err
	}

	netcatFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "bash", "-c", fmt.Sprintf("for i in {1..5}; do nc -vz -w 1 %s %d; sleep 1; done", dst, port))
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	netcatSync.Go(func() error {
		return netcatFromSrcPod(srcPod, dstPodIP)
	})
	err := netcatSync.Wait()
	framework.ExpectNoError(err, "Failed to connect to dst pod")
	err = tcpDumpSync.Wait()
	framework.ExpectNoError(err, "Failed to detect packets with correct DSCP on pod")
}

func iperf3Test(f *framework.Framework, srcPod, dstIP string, port int, protocol ...string) float64 {
	iperf3Sync := errgroup.Group{}

	iperfTest := func(pod, destIP string, port int, bps *float64) error {
		args := []string{"exec", pod, "--", "iperf3", "-c", destIP, "-p", strconv.Itoa(port), "-J"}
		if len(protocol) > 0 && protocol[0] == "udp" {
			args = append(args, "-u", "-b", "0")
		}
		output, err := e2ekubectl.RunKubectl(f.Namespace.Name, args...)
		if err != nil {
			return err
		}
		var data map[string]interface{}
		err = json.Unmarshal([]byte(output), &data)
		if err != nil {
			return err
		}
		end := data["end"].(map[string]interface{})
		if sum_sent, ok := end["sum_sent"]; ok {
			*bps = sum_sent.(map[string]interface{})["bits_per_second"].(float64)
		} else if sum, ok := end["sum"]; ok {
			*bps = sum.(map[string]interface{})["bits_per_second"].(float64)
		}
		return nil
	}
	bps := 0.0
	iperf3Sync.Go(func() error {
		return iperfTest(srcPod, dstIP, port, &bps)
	})
	err := iperf3Sync.Wait()
	framework.ExpectNoError(err, fmt.Sprintf("Failed to run iperf3 test for IP %s", dstIP))
	return bps
}

func twoStreamIperf3Tests(f *framework.Framework, srcPod, dstPod1IP, dstPod2IP string, port int) float64 {
	iperf3Sync1 := errgroup.Group{}
	iperf3Sync2 := errgroup.Group{}

	iperfTest := func(pod, destIP string, port int, bps *float64) error {
		output, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "iperf3", "-c", destIP, "-p", strconv.Itoa(port), "-J")
		if err != nil {
			return err
		}
		var data map[string]interface{}
		err = json.Unmarshal([]byte(output), &data)
		if err != nil {
			return err
		}
		end := data["end"].(map[string]interface{})
		sum_sent := end["sum_sent"].(map[string]interface{})
		*bps = sum_sent["bits_per_second"].(float64)
		return nil
	}

	bps1 := 0.0
	bps2 := 0.0

	iperf3Sync1.Go(func() error {
		return iperfTest(srcPod, dstPod1IP, port, &bps1)
	})
	iperf3Sync2.Go(func() error {
		return iperfTest(srcPod, dstPod2IP, port, &bps2)
	})
	err := iperf3Sync1.Wait()
	framework.ExpectNoError(err, fmt.Sprintf("Failed to run iperf3 test for IP %s", dstPod1IP))
	err = iperf3Sync2.Wait()
	framework.ExpectNoError(err, fmt.Sprintf("Failed to run iperf3 test for IP %s", dstPod2IP))
	return bps1 + bps2
}

func pingExpectNoDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, dscp int) {
	tcpDumpSync := errgroup.Group{}
	pingSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		output, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", pod, "--", "timeout", "10",
			"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(output)) == 0 {
			return fmt.Errorf("no packets captured")
		}
		return nil
	}

	pingFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "ping", "-c", "5", dst)
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	pingSync.Go(func() error {
		return pingFromSrcPod(srcPod, dstPodIP)
	})
	err := pingSync.Wait()
	gomega.Expect(err).To(gomega.BeNil())
	err = tcpDumpSync.Wait()
	gomega.Expect(err).To(gomega.HaveOccurred())
}

func netcatExpectNoDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, port, dscp int) {
	tcpDumpSync := errgroup.Group{}
	netcatSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		output, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", pod, "--", "timeout", "10",
			"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(output)) == 0 {
			return fmt.Errorf("no packets captured")
		}
		return nil
	}

	netcatFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "bash", "-c", fmt.Sprintf("for i in {1..5}; do nc -vz -w 1 %s %d; sleep 1; done", dst, port))
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	netcatSync.Go(func() error {
		return netcatFromSrcPod(srcPod, dstPodIP)
	})
	err := netcatSync.Wait()
	framework.ExpectNoError(err, "Failed to connect to dst pod")
	err = tcpDumpSync.Wait()
	gomega.Expect(err).To(gomega.HaveOccurred())
}
