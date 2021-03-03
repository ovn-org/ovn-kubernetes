package e2e

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/onsi/ginkgo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
)

// Validate that Services with the well-known annotation k8s.ovn.org/idled-at
// generate a NeedPods Event if the service doesn´t have endpoints and
// OVN EmptyLB-Backends feature is enabled
var _ = ginkgo.Describe("Unidling", func() {
	const (
		serviceName       = "empty-service"
		podName           = "execpod-noendpoints"
		ovnServiceIdledAt = "k8s.ovn.org/idled-at"
		port              = 80
	)

	f := framework.NewDefaultFramework("unidling")

	var cs clientset.Interface

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
	})

	// We simulate the idling feature that is Openshift specific creating a service and removing the pods
	ginkgo.It("Should generate a NeedPods event for traffic destined to idled services", func() {
		namespace := f.Namespace.Name
		jig := e2eservice.NewTestJig(cs, namespace, serviceName)
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(cs, e2eservice.MaxNodesForEndpointsTests)
		framework.ExpectNoError(err)
		nodeName := nodes.Items[0].Name

		ginkgo.By("creating an annotated service with no endpoints and idle annotation")
		_, err = jig.CreateTCPServiceWithPort(func(svc *v1.Service) {
			svc.Annotations = map[string]string{ovnServiceIdledAt: "true"}
		}, int32(port))
		framework.ExpectNoError(err)

		// Add a backend pod to the service in one node
		ginkgo.By("creating a backend pod for the service " + serviceName)
		serverPod := e2epod.NewAgnhostPod(namespace, "pod-backend", nil, nil, []v1.ContainerPort{{ContainerPort: 9376}}, "serve-hostname")
		serverPod.Labels = jig.Labels
		serverPod.Spec.NodeName = nodeName
		f.PodClient().CreateSync(serverPod)

		// Emulate the idling feature deleting the pod
		f.PodClient().DeleteSync(serverPod.Name, metav1.DeleteOptions{}, framework.DefaultPodDeletionTimeout)

		// Create exec pod to test the PodEvent is generated if it receives traffic to the idled service
		ginkgo.By(fmt.Sprintf("creating %v on node %v", podName, nodeName))
		execPod := e2epod.CreateExecPodOrFail(f.ClientSet, namespace, podName, func(pod *v1.Pod) {
			pod.Spec.NodeName = nodeName
		})

		serviceAddress := net.JoinHostPort(serviceName, strconv.Itoa(port))
		framework.Logf("waiting up to %v to connect to %v", e2eservice.KubeProxyEndpointLagTimeout, serviceAddress)
		cmd := fmt.Sprintf("/agnhost connect --timeout=3s %s", serviceAddress)

		ginkgo.By(fmt.Sprintf("hitting service %v from pod %v on node %v", serviceAddress, podName, nodeName))
		nonExpectedErr := "REFUSED"
		if pollErr := wait.PollImmediate(framework.Poll, e2eservice.KubeProxyEndpointLagTimeout, func() (bool, error) {
			_, err := framework.RunHostCmd(execPod.Namespace, execPod.Name, cmd)
			if err != nil && strings.Contains(err.Error(), nonExpectedErr) {
				return false, fmt.Errorf("Service is rejecting packets")
			}
			// An event like this must be generated
			// oc.recorder.Eventf(&serviceRef, kapi.EventTypeNormal, "NeedPods", "The service %s needs pods", serviceName.Name)
			events, err := cs.CoreV1().Events(namespace).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			for _, e := range events.Items {
				framework.Logf("At %v - event for %v: %v %v: %v", e.FirstTimestamp, e.InvolvedObject.Name, e.Source, e.Reason, e.Message)
				if e.Reason == "NeedPods" && strings.Contains(e.Message, serviceName) {
					return true, nil
				}
			}
			return false, nil

		}); pollErr != nil {
			framework.ExpectNoError(pollErr)
		}
	})

})
