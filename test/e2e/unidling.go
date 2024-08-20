package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
)

type serviceStatus int

const (
	works serviceStatus = iota
	rejects
	failsWithNoReject
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

	f := wrappedTestFramework("unidling")

	var cs clientset.Interface

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
	})

	// We simulate the idling feature that is Openshift specific creating a service and removing the pods
	ginkgo.It("Should generate a NeedPods event for traffic destined to idled services", func() {
		namespace := f.Namespace.Name
		jig := e2eservice.NewTestJig(cs, namespace, serviceName)
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, e2eservice.MaxNodesForEndpointsTests)
		framework.ExpectNoError(err)
		nodeName := nodes.Items[0].Name

		ginkgo.By("creating an annotated service with no endpoints and idle annotation")
		_, err = jig.CreateTCPServiceWithPort(context.TODO(), func(svc *v1.Service) {
			svc.Annotations = map[string]string{ovnServiceIdledAt: "true"}
		}, int32(port))
		framework.ExpectNoError(err)

		// Add a backend pod to the service in one node
		ginkgo.By("creating a backend pod for the service " + serviceName)
		serverPod := e2epod.NewAgnhostPod(namespace, "pod-backend", nil, nil, []v1.ContainerPort{{ContainerPort: 9376}}, "serve-hostname")
		serverPod.Labels = jig.Labels
		serverPod.Spec.NodeName = nodeName
		e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)

		// Emulate the idling feature deleting the pod
		e2epod.NewPodClient(f).DeleteSync(context.TODO(), serverPod.Name, metav1.DeleteOptions{}, e2epod.DefaultPodDeletionTimeout)

		// Create exec pod to test the PodEvent is generated if it receives traffic to the idled service
		ginkgo.By(fmt.Sprintf("creating %v on node %v", podName, nodeName))
		execPod := e2epod.CreateExecPodOrFail(context.TODO(), f.ClientSet, namespace, podName, func(pod *v1.Pod) {
			pod.Spec.NodeName = nodeName
		})

		serviceAddress := net.JoinHostPort(serviceName, strconv.Itoa(port))
		framework.Logf("waiting up to %v to connect to %v", e2eservice.KubeProxyEndpointLagTimeout, serviceAddress)
		cmd := fmt.Sprintf("/agnhost connect --timeout=3s %s", serviceAddress)

		ginkgo.By(fmt.Sprintf("hitting service %v from pod %v on node %v", serviceAddress, podName, nodeName))
		nonExpectedErr := "REFUSED"
		if pollErr := wait.PollImmediate(framework.Poll, e2eservice.KubeProxyEndpointLagTimeout, func() (bool, error) {
			_, err := e2epodoutput.RunHostCmd(execPod.Namespace, execPod.Name, cmd)
			if err != nil && strings.Contains(err.Error(), nonExpectedErr) {
				return false, fmt.Errorf("service is rejecting packets")
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

	// Check the creation of needPods events starting with annotated services.
	// All the tests start with a given configuration, waits for the behaviour of the service to settle
	// (if must reject or must timeout), then try to hit the service again and check if new events are
	// generated or not according to the test scenario. This is done to avoid races and make sure that
	// the right configuration is applied.
	ginkgo.Context("With annotated service", func() {
		var (
			clientPod   *v1.Pod
			node        string
			cmd         string
			namespace   string
			serviceName string
			service     *v1.Service
			jig         *e2eservice.TestJig
		)

		ginkgo.BeforeEach(func() {
			namespace = f.Namespace.Name
			serviceName = "testservice" + randString(5)
			jig = e2eservice.NewTestJig(cs, namespace, serviceName)
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, e2eservice.MaxNodesForEndpointsTests)
			framework.ExpectNoError(err)
			node = nodes.Items[0].Name
			ginkgo.By("creating an annotated service with no endpoints and idle annotation")
			service, err = jig.CreateTCPServiceWithPort(context.TODO(), func(svc *v1.Service) {
				svc.Annotations = map[string]string{ovnServiceIdledAt: "true"}
			}, int32(port))
			framework.ExpectNoError(err)

			ginkgo.By(fmt.Sprintf("creating %v on node %v", podName, node))
			clientPod = e2epod.CreateExecPodOrFail(context.TODO(), f.ClientSet, namespace, podName, func(pod *v1.Pod) {
				pod.Spec.NodeName = node
			})
			serviceAddress := net.JoinHostPort(serviceName, strconv.Itoa(port))
			framework.Logf("waiting up to %v to connect to %v", e2eservice.KubeProxyEndpointLagTimeout, serviceAddress)
			cmd = fmt.Sprintf("/agnhost connect --timeout=10s %s", serviceAddress)
		})

		ginkgo.It("Should generate a NeedPods event for traffic destined to idled services", func() {
			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(failsWithNoReject), "Service is rejecting")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(true), "New events are not generated")
		})

		ginkgo.It("Should not generate a NeedPods event when removing the annotation", func() {
			ginkgo.Skip("Not supported by OVN: Enable back when https://bugzilla.redhat.com/show_bug.cgi?id=2177173 is fixed")

			_, err := jig.UpdateService(context.TODO(), func(service *v1.Service) {
				service.Annotations = nil
			})
			framework.ExpectNoError(err)

			// Service starts a grace period in which it doesn't reject connections.
			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(failsWithNoReject), "Service is rejecting")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(false), "New events are generated")
		})

		ginkgo.It("Should not generate a NeedPods event when has backend", func() {
			createBackend(f, serviceName, namespace, node, jig.Labels, port)
			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(works), "Service is failing")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(false), "New events are generated")
		})

		ginkgo.It("Should generate a NeedPods event when backends were added and then removed", func() {
			be := createBackend(f, serviceName, namespace, node, jig.Labels, port)
			e2epod.NewPodClient(f).DeleteSync(context.TODO(), be.Name, metav1.DeleteOptions{}, e2epod.DefaultPodDeletionTimeout)
			err := framework.WaitForServiceEndpointsNum(context.TODO(), f.ClientSet, f.Namespace.Name, serviceName, 0, time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err)

			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(failsWithNoReject), "Service is not timing out")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(true), "New events are not generated")
		})

		ginkgo.It("Should connect to an unidled backend at the first attempt", func() {

			// Simulate service unidling
			_, err := jig.UpdateService(context.TODO(), func(service *v1.Service) {
				service.Annotations = nil
			})
			framework.ExpectNoError(err)

			var wg sync.WaitGroup
			done := make(chan struct{})
			wg.Add(1)
			go func() {
				defer ginkgo.GinkgoRecover()
				wg.Done()

				time.Sleep(time.Second)
				createBackend(f, serviceName, namespace, node, jig.Labels, port)
				close(done)
			}()
			wg.Wait()
			defer func() { <-done }()

			// Connecting to the service should work at the first attempt
			gomega.Expect(checkService(clientPod, cmd)).To(gomega.Equal(works))
		})
	})

	// Check the creation of needPods events starting with non annotated services.
	// All the tests start with a given configuration, waits for the behaviour of the service to settle
	// (if must reject or must timeout), then try to hit the service again and check if new events are
	// generated or not according to the test scenario. This is done to avoid races and make sure that
	// the right configuration is applied.
	ginkgo.Context("With non annotated service", func() {
		var (
			clientPod   *v1.Pod
			node        string
			cmd         string
			namespace   string
			serviceName string
			service     *v1.Service
			jig         *e2eservice.TestJig
		)

		ginkgo.BeforeEach(func() {
			namespace = f.Namespace.Name
			serviceName = "testservice" + randString(5)
			jig = e2eservice.NewTestJig(cs, namespace, serviceName)
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), cs, e2eservice.MaxNodesForEndpointsTests)
			framework.ExpectNoError(err)
			node = nodes.Items[0].Name
			ginkgo.By("creating an annotated service with no endpoints and idle annotation")
			service, err = jig.CreateTCPServiceWithPort(context.TODO(), func(svc *v1.Service) {
			}, int32(port))
			framework.ExpectNoError(err)

			ginkgo.By(fmt.Sprintf("creating %v on node %v", podName, node))
			clientPod = e2epod.CreateExecPodOrFail(context.TODO(), f.ClientSet, namespace, podName, func(pod *v1.Pod) {
				pod.Spec.NodeName = node
			})
			serviceAddress := net.JoinHostPort(serviceName, strconv.Itoa(port))
			framework.Logf("waiting up to %v to connect to %v", e2eservice.KubeProxyEndpointLagTimeout, serviceAddress)
			cmd = fmt.Sprintf("/agnhost connect --timeout=3s %s", serviceAddress)
		})

		ginkgo.It("Should not generate a NeedPods event for traffic destined to idled services", func() {
			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(rejects), "Service is not rejecting")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(false), "New events are generated")
		})

		ginkgo.It("Should generate a NeedPods event when adding the annotation", func() {
			_, err := jig.UpdateService(context.TODO(), func(service *v1.Service) {
				service.Annotations = map[string]string{ovnServiceIdledAt: "true"}
			})
			framework.ExpectNoError(err)

			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(failsWithNoReject), "Service is not timing out")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(true), "New events are not generated")

		})

		ginkgo.It("Should not generate a NeedPods event when has backend", func() {
			createBackend(f, serviceName, namespace, node, jig.Labels, port)
			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(works), "Service is failing")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(false), "New events are generated")
		})

		ginkgo.It("Should not generate a NeedPods event when backends were added and then removed", func() {
			be := createBackend(f, serviceName, namespace, node, jig.Labels, port)
			e2epod.NewPodClient(f).DeleteSync(context.TODO(), be.Name, metav1.DeleteOptions{}, e2epod.DefaultPodDeletionTimeout)
			err := framework.WaitForServiceEndpointsNum(context.TODO(), f.ClientSet, f.Namespace.Name, serviceName, 0, time.Second, wait.ForeverTestTimeout)
			framework.ExpectNoError(err)

			gomega.Eventually(func() serviceStatus {
				return checkService(clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(rejects), "Service is not rejecting")

			gomega.Eventually(func() bool {
				return hittingGeneratesNewEvents(service, cs, clientPod, cmd)
			}, 10*time.Second, 1*time.Second).Should(gomega.Equal(false), "New events are generated")

		})
	})

})

func createBackend(f *framework.Framework, serviceName, namespace, node string, labels map[string]string, port int32) *v1.Pod {
	ginkgo.By("creating a backend pod for the service " + serviceName)
	serverPod := e2epod.NewAgnhostPod(namespace, "pod-backend", nil, nil, []v1.ContainerPort{{ContainerPort: port}}, "netexec", "--http-port=80")
	serverPod.Labels = labels
	serverPod.Spec.NodeName = node
	pod := e2epod.NewPodClient(f).CreateSync(context.TODO(), serverPod)
	err := framework.WaitForServiceEndpointsNum(context.TODO(), f.ClientSet, f.Namespace.Name, serviceName, 1, time.Second, wait.ForeverTestTimeout)
	framework.ExpectNoError(err)
	return pod
}

// hittingGeneratesNewEvents tells if by hitting a service a brand new needPods event is generated
func hittingGeneratesNewEvents(service *v1.Service, cs clientset.Interface, clientPod *v1.Pod, cmd string) bool {
	framework.Logf("checking if needPods events are emitted for service %s in namespace %s", service.Name, service.Namespace)
	lastEventTime := lastIdlingEventForService(service, cs)
	// the event time resolution is one second, which is what we use to check if there are new events
	// we need to sleep 1 second to ensure two events are generated with different timestamps.
	time.Sleep(1 * time.Second)
	checkService(clientPod, cmd)
	err := wait.PollImmediate(framework.Poll, 3*time.Second, func() (bool, error) {
		newLastEvent := lastIdlingEventForService(service, cs)
		if newLastEvent.After(lastEventTime.Time) {
			return true, nil
		}
		return false, nil
	})

	if err != nil { // timed out
		framework.Logf("hittingGeneratesNewEvents fails with error: %v\n", err)
		return false
	}
	return true
}

// checkService tries to hit a service and tells the behaviour of the connection.
// The connection can be refused, can timeout or can work.
func checkService(clientPod *v1.Pod, cmd string) serviceStatus {
	refusedError := "REFUSED"
	stdout, stderr, err := e2epodoutput.RunHostCmdWithFullOutput(clientPod.Namespace, clientPod.Name, cmd)
	framework.Logf("checking service with cmd \"%s\" from pod %s in ns %s returned stdout: %v stderr: %v", cmd,
		clientPod.Name, clientPod.Namespace, stdout, stderr)
	if err != nil && strings.Contains(err.Error(), refusedError) {
		return rejects
	}
	if err != nil {
		return failsWithNoReject
	}
	return works
}

// lastIdlingEventForService returns the most recent idling event for the given service
func lastIdlingEventForService(service *v1.Service, cs clientset.Interface) metav1.Time {
	// An event like this must be generated
	// oc.recorder.Eventf(&serviceRef, kapi.EventTypeNormal, "NeedPods", "The service %s needs pods", serviceName.Name)
	fieldSelector := fmt.Sprintf("reason=NeedPods")
	events, err := cs.CoreV1().Events(service.Namespace).List(context.Background(), metav1.ListOptions{FieldSelector: fieldSelector})
	framework.ExpectNoError(err)

	mostRecent := metav1.Time{}
	for _, e := range events.Items {
		if strings.Contains(e.Message, service.Name) && e.LastTimestamp.After(mostRecent.Time) {
			mostRecent = e.LastTimestamp
		}
	}
	return mostRecent
}

const charset = "abcdefghijklmnopqrstuvwxyz"

var seededRand *rand.Rand = rand.New(
	rand.NewSource(time.Now().UnixNano()))

func StringWithCharset(length int, charset string) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[seededRand.Intn(len(charset))]
	}
	return string(b)
}

func randString(length int) string {
	return StringWithCharset(length, charset)
}
