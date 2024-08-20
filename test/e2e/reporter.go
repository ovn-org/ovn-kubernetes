// SPDX-License-Identifier:Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"

	aprv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/adminpolicybasedroute/v1"
	egressfwv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	egressqosv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressqos/v1"
	egressservicev1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressservice/v1"

	"github.com/openshift-kni/k8sreporter"
	"github.com/pkg/errors"
	metallbv1beta1 "go.universe.tf/metallb/api/v1beta1"
	metallbv1beta2 "go.universe.tf/metallb/api/v1beta2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	"k8s.io/utils/strings/slices"
)

const frrContainerName = "frr"

func InitReporter(kubeconfig, path string, namespaces []string) *k8sreporter.KubernetesReporter {
	// When using custom crds, we need to add them to the scheme
	addToScheme := func(s *runtime.Scheme) error {
		err := aprv1.AddToScheme(s)
		if err != nil {
			return err
		}
		err = egressipv1.AddToScheme(s)
		if err != nil {
			return err
		}
		err = egressfwv1.AddToScheme(s)
		if err != nil {
			return err
		}
		err = egressqosv1.AddToScheme(s)
		if err != nil {
			return err
		}
		err = egressservicev1.AddToScheme(s)
		if err != nil {
			return err
		}
		err = metallbv1beta1.AddToScheme(s)
		if err != nil {
			return err
		}
		err = metallbv1beta2.AddToScheme(s)
		if err != nil {
			return err
		}
		return nil
	}

	// The namespaces we want to dump resources for (including pods and pod logs)
	dumpNamespace := func(ns string) bool {
		return slices.Contains(namespaces, ns)
	}

	// The list of CRDs we want to dump
	crds := []k8sreporter.CRData{
		{Cr: &aprv1.AdminPolicyBasedExternalRouteList{}},
		{Cr: &egressipv1.EgressIPList{}},
		{Cr: &egressfwv1.EgressFirewallList{}},
		{Cr: &egressqosv1.EgressQoSList{}},
		{Cr: &egressservicev1.EgressServiceList{}},
		{Cr: &metallbv1beta1.IPAddressPoolList{}},
		{Cr: &metallbv1beta1.AddressPoolList{}},
		{Cr: &metallbv1beta2.BGPPeerList{}},
		{Cr: &metallbv1beta1.L2AdvertisementList{}},
		{Cr: &metallbv1beta1.BGPAdvertisementList{}},
		{Cr: &metallbv1beta1.BFDProfileList{}},
		{Cr: &metallbv1beta1.CommunityList{}},
		{Cr: &corev1.ServiceList{}},
	}

	reporter, err := k8sreporter.New(kubeconfig, addToScheme, dumpNamespace, path, crds...)
	if err != nil {
		log.Fatalf("Failed to initialize the reporter %s", err)
	}
	return reporter
}

// DumpInfo dumps crs, pod container logs, pod specs for namespaces initialized
// in the reporter into testNameNoSpaces subpath directory. It just collects pod
// logs for past 10 mins which is more appropriate for test run time.
func DumpInfo(reporter *k8sreporter.KubernetesReporter) {
	testNameNoSpaces := strings.ReplaceAll(ginkgo.CurrentSpecReport().LeafNodeText, " ", "-")
	reporter.Dump(10*time.Minute, testNameNoSpaces)
}

// DumpBGPInfo dumps current bgp specific configuration from frr router container and metallb
// speaker pod's frr container which helps to troubleshoot if there is any problem with route
// advertisement for a load balancer service ip address.
func DumpBGPInfo(basePath, testName string, f *framework.Framework) {
	testPath := path.Join(basePath, strings.ReplaceAll(testName, " ", "-"))
	err := os.MkdirAll(testPath, 0755)
	if err != nil && !errors.Is(err, os.ErrExist) {
		fmt.Fprintf(os.Stderr, "failed to create test dir: %v\n", err)
		return
	}
	frrContainer := &containerExecutor{container: frrContainerName}
	dump, err := rawDump(frrContainer, "/etc/frr/bgpd.conf", "/tmp/frr.log", "/etc/frr/daemons")
	if err != nil {
		framework.Logf("External frr dump for container %s failed %v", frrContainerName, err)
	}
	logFile, err := logFileFor(testPath, fmt.Sprintf("frrdump-%s", frrContainerName))
	if err != nil {
		framework.Logf("External frr dump for container %s, failed to open file %v", frrContainerName, err)
		return
	}
	_, err = fmt.Fprint(logFile, dump)
	if err != nil {
		framework.Logf("External frr dump for container %s, failed to write to file %v", frrContainerName, err)
		return
	}

	speakerPods, err := speakerPods(f.ClientSet)
	framework.ExpectNoError(err)
	for _, pod := range speakerPods {
		if len(pod.Spec.Containers) == 1 { // we dump only in case of frr
			break
		}
		podExec := ForPod(pod.Namespace, pod.Name, "frr")
		dump, err := rawDump(podExec, "/etc/frr/frr.conf", "/etc/frr/frr.log")
		if err != nil {
			framework.Logf("External frr dump for pod %s failed %v", pod.Name, err)
			continue
		}
		f, err := logFileFor(testPath, fmt.Sprintf("frrdump-%s", pod.Name))
		if err != nil {
			framework.Logf("External frr dump for pod %s, failed to open file %v", pod.Name, err)
			continue
		}
		fmt.Fprintf(f, "Dumping information for %s, local addresses: %s\n", pod.Name, pod.Status.PodIPs)
		_, err = fmt.Fprint(f, dump)
		if err != nil {
			framework.Logf("External frr dump for pod %s, failed to write to file %v", pod.Name, err)
			continue
		}
	}
	describeSvc(f.Namespace.Name)
}

func logFileFor(base string, kind string) (*os.File, error) {
	path := path.Join(base, kind) + ".log"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// rawDump dumps all the low level info as a single string.
// To be used for debugging in order to print the status of the frr instance.
func rawDump(exec Executor, filesToDump ...string) (string, error) {
	allerrs := errors.New("")

	res := "####### Show running config\n"
	out, err := exec.Exec("vtysh", "-c", "show running-config")
	if err != nil {
		allerrs = errors.Wrapf(allerrs, "\nFailed exec show bgp neighbor: %v", err)
	}
	res += out

	for _, file := range filesToDump {
		res += fmt.Sprintf("####### Dumping file %s\n", file)
		// limiting the output to 500 lines:
		out, err = exec.Exec("bash", "-c", fmt.Sprintf("cat %s | tail -n 500", file))
		if err != nil {
			allerrs = errors.Wrapf(allerrs, "\nFailed to cat file %s: %v", file, err)
		}
		res += out
	}

	res += "####### BGP Neighbors\n"
	out, err = exec.Exec("vtysh", "-c", "show bgp neighbor")
	if err != nil {
		allerrs = errors.Wrapf(allerrs, "\nFailed exec show bgp neighbor: %v", err)
	}
	res += out

	res += "####### BFD Peers\n"
	out, err = exec.Exec("vtysh", "-c", "show bfd peer")
	if err != nil {
		allerrs = errors.Wrapf(allerrs, "\nFailed exec show bfd peer: %v", err)
	}
	res += out

	res += "####### Routes published by BGP Speakers\n"
	out, err = exec.Exec("vtysh", "-c", "show ip bgp detail")
	if err != nil {
		allerrs = errors.Wrapf(allerrs, "\nFailed exec show ip bgp detail: %v", err)
	}
	res += out

	res += "####### ip route show\n"
	out, err = exec.Exec("bash", "-c", "ip route show")
	if err != nil {
		allerrs = errors.Wrapf(allerrs, "\nFailed exec ip route show: %v", err)
	}
	res += out

	res += "####### Check for any crashinfo files\n"
	if crashInfo, err := exec.Exec("bash", "-c", "ls /var/tmp/frr/bgpd.*/crashlog"); err == nil {
		crashInfo = strings.TrimSuffix(crashInfo, "\n")
		files := strings.Split(crashInfo, "\n")
		for _, file := range files {
			res += fmt.Sprintf("####### Dumping crash file %s\n", file)
			out, err = exec.Exec("bash", "-c", fmt.Sprintf("cat %s", file))
			if err != nil {
				allerrs = errors.Wrapf(allerrs, "\nFailed to cat bgpd crashinfo file %s: %v", file, err)
			}
			res += out
		}
	}

	if allerrs.Error() == "" {
		allerrs = nil
	}

	return res, allerrs
}

// speakerPods returns the set of pods running the speakers.
func speakerPods(cs clientset.Interface) ([]*corev1.Pod, error) {
	speakers, err := cs.CoreV1().Pods(metallbNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: speakerLabelgSelector,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to fetch speaker pods")
	}
	if len(speakers.Items) == 0 {
		return nil, errors.New("no speaker pods found")
	}
	speakerPods := make([]*corev1.Pod, 0)
	for _, item := range speakers.Items {
		i := item
		speakerPods = append(speakerPods, &i)
	}
	return speakerPods, nil
}

// describeSvc logs the output of kubectl describe svc for the given namespace.
func describeSvc(ns string) {
	framework.Logf("\nOutput of kubectl describe svc:\n")
	desc, _ := e2ekubectl.RunKubectl(
		ns, "describe", "svc", fmt.Sprintf("--namespace=%v", ns))
	framework.Logf(desc)
}
