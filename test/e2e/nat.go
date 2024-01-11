package e2e

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	"k8s.io/utils/pointer"
)

var _ = ginkgo.Describe("NAT", ginkgo.Serial, func() {
	const (
		natTestName           = "natvalidation"
		externalContainerName = "e2e-" + natTestName
		externalContainerPort = 8080
		clusterNetPodName     = "e2e-" + natTestName + "cluster-networked"
	)
	var externalContainerIP string
	f := wrappedTestFramework(natTestName)

	ginkgo.BeforeEach(func() {
		externalNodeIPV4, externalNodeIPV6 := createClusterExternalContainer(
			externalContainerName,
			agnhostImage,
			[]string{"--privileged", "--network", "kind"},
			[]string{"netexec", fmt.Sprintf("--http-port=%d", externalContainerPort)},
		)
		if IsIPv6Cluster(f.ClientSet) {
			externalContainerIP = externalNodeIPV6
		} else {
			externalContainerIP = externalNodeIPV4
		}
	})

	ginkgo.AfterEach(func() {
		deleteClusterExternalContainer(externalContainerName)
	})

	// Sends a request to an agnhost destination's /clientip which returns the src port of the packet.
	getSrcPort := func(namespace, pod string, srcPort int, dstIP string, dstPort int) (int, error) {
		dst := net.JoinHostPort(dstIP, strconv.Itoa(dstPort))
		curlCmd := fmt.Sprintf("curl --local-port %d -s --retry-connrefused --retry 2 --max-time 0.5 --connect-timeout 0.5 --retry-delay 1 http://%s/clientip", srcPort, dst)
		out, err := e2epodoutput.RunHostCmd(namespace, pod, curlCmd)
		if err != nil {
			return 0, fmt.Errorf("failed to curl agnhost on %s from %s, err: %w", dstIP, pod, err)
		}
		_, srcPortFoundStr, err := net.SplitHostPort(out)
		if err != nil {
			return 0, fmt.Errorf("failed to split agnhost's clientip host:port response, err: %w", err)
		}
		srcPort, err = strconv.Atoi(srcPortFoundStr)
		if err != nil {
			return 0, fmt.Errorf("failed to convert found src port %q to integer: %v", srcPortFoundStr, err)
		}
		return srcPort, nil
	}

	// Validate that the node source port is port address translated when a cluster networked pod selects a source port outside
	// a port range as defined by Linux net.ipv4.ip_local_port_range
	// Assumption - all nodes within the cluster have equal net.ipv4.ip_local_port_range values.
	// 1. Create cluster networked pod and gather local port range (privilege required)
	// 2. Picked a source port number outside kernel port range
	// 3. Ensure source port was port address translated
	ginkgo.It("Should limit external source port range for cluster networked pods in shared gateway mode", func() {
		if IsGatewayModeLocal() {
			ginkgo.Skip("local gateway mode does not need to limit source port range")
		}
		// pods get a copy of the sysfs file system on the host and we assume all nodes have equivilent port ranges, therefore
		// we can read the local port range from the pods sysfs.
		pod, err := createPod(f, clusterNetPodName, "", f.Namespace.Name, []string{}, map[string]string{}, func(pod *corev1.Pod) {
			// privileged is required to read /proc/sys/net/ipv4/ip_local_port_range
			pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: pointer.Bool(true)}
		})
		framework.ExpectNoError(err, "unable to create privileged pod")
		ginkgo.By("1. Create cluster networked pod and gather local port range (privilege required)")
		podExec := ForPod(pod.Namespace, pod.Name, pod.Spec.Containers[0].Name)
		kernelLocalPortRange, err := podExec.Exec("cat", "/proc/sys/net/ipv4/ip_local_port_range")
		framework.ExpectNoError(err, "unable to get ip_local_port_range")
		gomega.Expect(kernelLocalPortRange).ShouldNot(gomega.BeEmpty())
		var kernelPortRangeStart, kernelPortRangeEnd int
		_, err = fmt.Sscanf(kernelLocalPortRange, "%d\t%d", &kernelPortRangeStart, &kernelPortRangeEnd)
		framework.ExpectNoError(err, fmt.Sprintf("unable to extract port range from %q", kernelLocalPortRange))
		podSrcPortOutsidePortRange := kernelPortRangeStart - 1
		gomega.Expect(podSrcPortOutsidePortRange).Should(gomega.BeNumerically(">", 0))
		ginkgo.By("2. Picked a source port number outside kernel port range")
		var curlErr error
		var podSrcPortObservedExternally int
		_ = wait.PollUntilContextTimeout(
			context.Background(),
			retryInterval,
			retryTimeout,
			true,
			func(ctx context.Context) (bool, error) {
				podSrcPortObservedExternally, curlErr = getSrcPort(pod.Namespace, pod.Name, podSrcPortOutsidePortRange, externalContainerIP, externalContainerPort)
				return curlErr == nil, nil
			},
		)
		framework.ExpectNoError(curlErr, "source port check to the external kind container failed: %v", curlErr)
		ginkgo.By("3. Ensure source port was port address translated")
		if podSrcPortObservedExternally == podSrcPortOutsidePortRange {
			framework.Failf("Statically set source source port %d is not within the port range %s and was expected to"+
				" be port address translated but was not", podSrcPortOutsidePortRange, kernelLocalPortRange)
		}
		if podSrcPortObservedExternally < kernelPortRangeStart || podSrcPortObservedExternally > kernelPortRangeEnd {
			framework.Failf("Statically set source source port %d is not within the port range %s", podSrcPortOutsidePortRange, kernelLocalPortRange)
		}
	})
})
