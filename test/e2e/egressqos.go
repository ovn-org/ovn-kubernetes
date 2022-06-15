package e2e

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/onsi/ginkgo"
	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
)

var _ = ginkgo.Describe("e2e EgressQoS validation", func() {
	const (
		egressQoSYaml = "egressqos.yaml"
		srcPodName    = "src-dscp-pod"
		dstPod1Name   = "dst-dscp-pod1"
		dstPod2Name   = "dst-dscp-pod2"
		// tcpdump args: http://darenmatthews.com/blog/?p=1199 , https://www.tucny.com/home/dscp-tos
		tcpdumpIPv4 = "icmp and (ip and (ip[1] & 0xfc) >> 2 == %d)"
		tcpdumpIPv6 = "icmp6 and (ip6 and (ip6[0:2] & 0xfc0) >> 6 == %d)"
	)

	var (
		dstPod1IPv4 string
		dstPod1IPv6 string
		dstPod2IPv4 string
		dstPod2IPv6 string
		srcNode     string
	)

	f := newPrivelegedTestFramework("egressqos")

	ginkgo.BeforeEach(func() {
		clientSet := f.ClientSet
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(clientSet, 3)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(nodes.Items))
		}

		srcNode = nodes.Items[0].Name

		dstPod1, err := createPod(f, dstPod1Name, nodes.Items[1].Name, f.Namespace.Name, []string{"bash", "-c", "apk update; apk add tcpdump; sleep 20000"}, map[string]string{}, func(p *v1.Pod) {
			p.Spec.HostNetwork = true
		})
		framework.ExpectNoError(err)
		dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)

		dstPod2, err := createPod(f, dstPod2Name, nodes.Items[2].Name, f.Namespace.Name, []string{"bash", "-c", "apk update; apk add tcpdump; sleep 20000"}, map[string]string{}, func(p *v1.Pod) {
			p.Spec.HostNetwork = true
		})
		framework.ExpectNoError(err)

		dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)

		gomega.Eventually(func() error {
			_, err := framework.RunKubectl(f.Namespace.Name, "exec", dstPod1Name, "--", "which", "tcpdump")
			if err != nil {
				return err
			}

			_, err = framework.RunKubectl(f.Namespace.Name, "exec", dstPod2Name, "--", "which", "tcpdump")
			return err
		}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
	})

	// Validate a pod's egress traffic heading to different CIDRs is marked with correct DSCP
	// values corresponding to the EgressQoS resource. Updating the resource should change these values.
	// We also validate that both current pods and new pods are affected by the EgressQoS resource by
	// creating the pod before or after it (podBeforeQoS param).
	ginkgotable.DescribeTable("Should validate correct DSCP value on EgressQoS resource changes",
		func(tcpDumpTpl string, dst1IP *string, prefix1 string, dst2IP *string, podBeforeQoS bool) {
			dscpValue := 50
			if podBeforeQoS {
				_, err := createPod(f, srcPodName, srcNode, f.Namespace.Name, []string{}, map[string]string{"app": "test"})
				framework.ExpectNoError(err)
			}

			egressQoSConfig := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressQoS
metadata:
  name: default
  namespace: ` + f.Namespace.Name + `
spec:
  egress:
  - dscp: ` + strconv.Itoa(dscpValue-1) + `
    dstCIDR: ` + *dst1IP + prefix1 + `
  - dscp: ` + strconv.Itoa(dscpValue-2) + `
    podSelector:
      matchLabels:
        app: test
`)

			if err := ioutil.WriteFile(egressQoSYaml, []byte(egressQoSConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressQoS configuration")
			framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressQoSYaml)

			if !podBeforeQoS {
				_, err := createPod(f, srcPodName, srcNode, f.Namespace.Name, []string{}, map[string]string{"app": "test"})
				framework.ExpectNoError(err)
			}

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, dscpValue-1, dscpValue-2)

			egressQoSConfig = fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressQoS
metadata:
  name: default
  namespace: ` + f.Namespace.Name + `
spec:
  egress:
  - dscp: ` + strconv.Itoa(dscpValue-10) + `
    dstCIDR: ` + *dst1IP + prefix1 + `
  - dscp: ` + strconv.Itoa(dscpValue-20) + `
    podSelector:
      matchLabels:
        app: test
`)

			if err := ioutil.WriteFile(egressQoSYaml, []byte(egressQoSConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			framework.Logf("Update the EgressQoS configuration")
			framework.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressQoSYaml)

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, dscpValue-10, dscpValue-20)

			framework.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressQoSYaml)

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, 0, 0)

		},
		ginkgotable.Entry("ipv4 pod before resource", tcpdumpIPv4, &dstPod1IPv4, "/32", &dstPod2IPv4, true),
		ginkgotable.Entry("ipv4 pod after resource", tcpdumpIPv4, &dstPod1IPv4, "/32", &dstPod2IPv4, false),
		ginkgotable.Entry("ipv6 pod before resource", tcpdumpIPv6, &dstPod1IPv6, "/128", &dstPod2IPv6, true),
		ginkgotable.Entry("ipv6 pod after resource", tcpdumpIPv6, &dstPod1IPv6, "/128", &dstPod2IPv6, false))

	ginkgotable.DescribeTable("Should validate correct DSCP value on pod labels changes",
		func(tcpDumpTpl string, dst1IP *string, prefix1 string, dst2IP *string, prefix2 string) {
			dscpValue := 50

			// create without labels, no packets should be marked
			pod, err := createPod(f, srcPodName, srcNode, f.Namespace.Name, []string{}, nil)
			framework.ExpectNoError(err)

			egressQoSConfig := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressQoS
metadata:
  name: default
  namespace: ` + f.Namespace.Name + `
spec:
  egress:
  - dscp: ` + strconv.Itoa(dscpValue-1) + `
    dstCIDR: ` + *dst1IP + prefix1 + `
    podSelector:
      matchLabels:
        test1: test1
  - dscp: ` + strconv.Itoa(dscpValue-2) + `
    dstCIDR: ` + *dst2IP + prefix2 + `
    podSelector:
      matchLabels:
        test2: test2
`)

			if err := ioutil.WriteFile(egressQoSYaml, []byte(egressQoSConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressQoS configuration")
			framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressQoSYaml)

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, 0, 0)

			// match the first rule only
			pod.Labels = map[string]string{"test1": "test1"}
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.Background(), pod, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "unable to update pod labels")

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, dscpValue-1, 0)

			// match the second rule only
			pod.Labels = map[string]string{"test2": "test2"}
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.Background(), pod, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "unable to update pod labels")

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, 0, dscpValue-2)

			// match both rules
			pod.Labels = map[string]string{"test1": "test1", "test2": "test2"}
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.Background(), pod, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "unable to update pod labels")

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, dscpValue-1, dscpValue-2)

			// match no rules again
			pod.Labels = map[string]string{"unrelated": "unrelated"}
			_, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Update(context.Background(), pod, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "unable to update pod labels")

			pingAndCheckDSCP(f, srcPodName, dstPod1Name, *dst1IP, dstPod2Name, *dst2IP, tcpDumpTpl, 0, 0)
		},
		ginkgotable.Entry("ipv4 pod", tcpdumpIPv4, &dstPod1IPv4, "/32", &dstPod2IPv4, "/32"),
		ginkgotable.Entry("ipv6 pod", tcpdumpIPv6, &dstPod1IPv6, "/128", &dstPod2IPv6, "/128"))
})

func pingAndCheckDSCP(f *framework.Framework, srcPod, dstPod1, dstPod1IP, dstPod2, dstPod2IP, tcpDumpTpl string, dscp1, dscp2 int) {
	tcpDumpSync := errgroup.Group{}
	pingSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		_, err := framework.RunKubectl(f.Namespace.Name, "exec", pod, "--", "timeout", "10",
			"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
		return err
	}

	pingFromSrcPod := func(pod, dst string) error {
		_, err := framework.RunKubectl(f.Namespace.Name, "exec", pod, "--", "ping", "-c", "3", dst)
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod1, dscp1)
	})
	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod2, dscp2)
	})

	pingSync.Go(func() error {
		return pingFromSrcPod(srcPod, dstPod1IP)
	})
	pingSync.Go(func() error {
		return pingFromSrcPod(srcPod, dstPod2IP)
	})

	err := pingSync.Wait()
	framework.ExpectNoError(err, "Failed to ping dst pod")
	err = tcpDumpSync.Wait()
	framework.ExpectNoError(err, "Failed to detect ping with correct DSCP on pod")

}
