package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/urfave/cli/v2"

	hotypes "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/ipallocator"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
)

func getPodAnnotations(fakeClient kubernetes.Interface, namespace, name string) string {
	pod, err := fakeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return pod.Annotations[util.OvnPodAnnotationName]
}

func newPodMeta(namespace, name string, additionalLabels map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"name": name,
	}
	for k, v := range additionalLabels {
		labels[k] = v
	}
	return metav1.ObjectMeta{
		Name:      name,
		UID:       types.UID(name),
		Namespace: namespace,
		Labels:    labels,
	}
}

func newPodWithLabels(namespace, name, node, podIP string, additionalLabels map[string]string) *v1.Pod {
	podIPs := []v1.PodIP{}
	if podIP != "" {
		podIPs = append(podIPs, v1.PodIP{IP: podIP})
	}
	return &v1.Pod{
		ObjectMeta: newPodMeta(namespace, name, additionalLabels),
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: v1.PodStatus{
			Phase:  v1.PodRunning,
			PodIP:  podIP,
			PodIPs: podIPs,
		},
	}
}

func newPod(namespace, name, node, podIP string) *v1.Pod {
	podIPs := []v1.PodIP{}
	if podIP != "" {
		podIPs = append(podIPs, v1.PodIP{IP: podIP})
	}
	return &v1.Pod{
		ObjectMeta: newPodMeta(namespace, name, nil),
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "containerName",
					Image: "containerImage",
				},
			},
			NodeName: node,
		},
		Status: v1.PodStatus{
			Phase:  v1.PodRunning,
			PodIP:  podIP,
			PodIPs: podIPs,
		},
	}
}

type testPod struct {
	portUUID     string
	nodeName     string
	nodeSubnet   string
	nodeMgtIP    string
	nodeGWIP     string
	podName      string
	podIP        string
	podMAC       string
	namespace    string
	portName     string
	routes       []util.PodRoute
	noIfaceIdVer bool
}

func newTPod(nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIP, podMAC, namespace string) (to testPod) {
	portName := util.GetLogicalPortName(namespace, podName, "",
		util.NetNameInfo{NetName: ovntypes.DefaultNetworkName, Prefix: "", IsSecondary: false})
	to = testPod{
		nodeName:   nodeName,
		nodeSubnet: nodeSubnet,
		nodeMgtIP:  nodeMgtIP,
		nodeGWIP:   nodeGWIP,
		podName:    podName,
		podIP:      podIP,
		podMAC:     podMAC,
		namespace:  namespace,
		portName:   portName,
		portUUID:   portName + "-UUID",
	}
	return
}

func (p testPod) populateLogicalSwitchCache(fakeOvn *FakeOVN, uuid string) {
	gomega.Expect(p.nodeName).NotTo(gomega.Equal(""))
	fakeOvn.controller.lsManager.AddNode(p.nodeName, uuid, []*net.IPNet{ovntest.MustParseIPNet(p.nodeSubnet)})
}

func (p testPod) getAnnotationsJson() string {
	var podRoutes string
	for key, route := range p.routes {
		routeString := `{"dest":"` + route.Dest.String() + `","nextHop":"` + route.NextHop.String() + `"}`
		if key == len(p.routes)-1 {
			podRoutes += podRoutes + routeString
		} else {
			podRoutes += podRoutes + routeString + ","
		}
	}

	podRoutesJSON := ""
	if len(podRoutes) > 0 {
		podRoutesJSON = `, "routes":[` + podRoutes + `]`
	}
	return `{"default": {"ip_addresses":["` + p.podIP + `/24"], "mac_address":"` + p.podMAC + `",
		"gateway_ips": ["` + p.nodeGWIP + `"], "ip_address":"` + p.podIP + `/24", "gateway_ip": "` + p.nodeGWIP + `"` + podRoutesJSON + `}}`

}

func setPodAnnotations(podObj *v1.Pod, testPod testPod) {
	podAnnot := map[string]string{
		util.OvnPodAnnotationName: testPod.getAnnotationsJson(),
	}
	podObj.Annotations = podAnnot
}

func getLogicalSwitchUUID(client libovsdbclient.Client, name string) string {
	ctext, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
	defer cancel()
	lsl := []nbdb.LogicalSwitch{}
	err := client.WhereCache(
		func(ls *nbdb.LogicalSwitch) bool {
			return ls.Name == name
		}).List(ctext, &lsl)

	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(len(lsl)).To(gomega.Equal(1))
	return lsl[0].UUID

}

func getExpectedDataPodsAndSwitches(pods []testPod, nodes []string) []libovsdbtest.TestData {
	nodeslsps := make(map[string][]string)
	var logicalSwitchPorts []*nbdb.LogicalSwitchPort
	for _, pod := range pods {
		portName := util.GetLogicalPortName(pod.namespace, pod.podName, "",
			util.NetNameInfo{NetName: ovntypes.DefaultNetworkName, Prefix: "", IsSecondary: false})
		var lspUUID string
		if len(pod.portUUID) == 0 {
			lspUUID = portName + "-UUID"
		} else {
			lspUUID = pod.portUUID
		}
		podAddr := fmt.Sprintf("%s %s", pod.podMAC, pod.podIP)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspUUID,
			Name:      portName,
			Addresses: []string{podAddr},
			ExternalIDs: map[string]string{
				"pod":       "true",
				"namespace": pod.namespace,
			},
			Options: map[string]string{
				"requested-chassis": pod.nodeName,
				"iface-id-ver":      pod.podName,
			},
			PortSecurity: []string{podAddr},
		}
		if pod.noIfaceIdVer {
			delete(lsp.Options, "iface-id-ver")
		}
		logicalSwitchPorts = append(logicalSwitchPorts, lsp)
		nodeslsps[pod.nodeName] = append(nodeslsps[pod.nodeName], lspUUID)

	}
	var logicalSwitches []*nbdb.LogicalSwitch
	for _, node := range nodes {
		logicalSwitches = append(logicalSwitches, &nbdb.LogicalSwitch{
			UUID:  node + "-UUID",
			Name:  node,
			Ports: nodeslsps[node],
		})
	}
	data := []libovsdbtest.TestData{}
	for _, lsp := range logicalSwitchPorts {
		data = append(data, lsp)
	}
	for _, ls := range logicalSwitches {
		data = append(data, ls)
	}

	return data
}

var _ = ginkgo.Describe("OVN Pod Operations", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup

		defaultNetNameInfo util.NetNameInfo
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags
		defaultNetNameInfo = util.NetNameInfo{NetName: ovntypes.DefaultNetworkName, Prefix: "", IsSecondary: false}

		fakeOvn = NewFakeOVN()
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					Name: "node1",
				},
			},
		}

	})

	ginkgo.AfterEach(func() {
		fakeOvn.shutdown()
	})

	ginkgo.Context("during execution", func() {

		ginkgo.It("reconciles an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)

				ctext, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
				defer cancel()
				lsl := []nbdb.LogicalSwitch{}
				err := fakeOvn.controller.nbClient.WhereCache(
					func(ls *nbdb.LogicalSwitch) bool {
						return ls.Name == "node1"
					}).List(ctext, &lsl)

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(len(lsl)).To(gomega.Equal(1))

				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				gomega.Expect(ok).To(gomega.BeFalse())

				// Assign it and perform the update
				t.nodeName = "node1"
				t.portName = util.GetLogicalPortName(t.namespace, t.podName, "", defaultNetNameInfo)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Update(context.TODO(),
					newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				pod, _ := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(pod).To(gomega.BeNil())

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("allows allocation after pods are completed", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				pod, _ := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(pod).To(gomega.BeNil())

				myPod := newPod(t.namespace, t.podName, t.nodeName, t.podIP)
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					myPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))

				ginkgo.By("Allocating all of the rest of the node subnet")
				// allocate all the rest of the IPs in the subnet
				fakeOvn.controller.lsManager.AllocateUntilFull("node1")

				ginkgo.By("Creating another pod which will fail due to allocation full")
				t2 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod2",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				myPod2, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					newPod(t2.namespace, t2.podName, t2.nodeName, ""), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.HaveLen(0))

				myPod2Key, err := getResourceKey(factory.PodType, myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				checkRetryObjectEventually(myPod2Key, true, fakeOvn.controller.retryPods)

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))
				ginkgo.By("Marking myPod as completed should free IP")
				myPod.Status.Phase = v1.PodSucceeded

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).UpdateStatus(context.TODO(),
					myPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// port should be gone or marked for removal in logical port cache
				logicalPort := util.GetLogicalPortName(myPod.Namespace, myPod.Name, "", defaultNetNameInfo)
				gomega.Eventually(func() bool {
					info, err := fakeOvn.controller.logicalPortCache.get(logicalPort)
					return err != nil || !info.expires.IsZero()
				}, 2).Should(gomega.BeTrue())

				ginkgo.By("Freed IP should now allow mypod2 to come up")
				key, err := getResourceKey(factory.PodType, myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				setRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()
				// there should also be no entry for this pod in the retry cache
				gomega.Eventually(func() bool {
					return checkRetryObj(myPod2Key, fakeOvn.controller.retryPods)
				}, retryObjInterval+time.Second).Should(gomega.BeFalse())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.MatchJSON(t2.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t2}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not deallocate in-use and previously freed completed pods IP", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				pod, _ := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(pod).To(gomega.BeNil())

				myPod := newPod(t.namespace, t.podName, t.nodeName, t.podIP)
				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					myPod, metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))

				ginkgo.By("Allocating all of the rest of the node subnet")
				// allocate all the rest of the IPs in the subnet
				fakeOvn.controller.lsManager.AllocateUntilFull("node1")

				ginkgo.By("Creating another pod which will fail due to allocation full")
				t2 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod2",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				myPod2, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					newPod(t2.namespace, t2.podName, t2.nodeName, ""), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.HaveLen(0))

				myPod2Key, err := getResourceKey(factory.PodType, myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				checkRetryObjectEventually(myPod2Key, true, fakeOvn.controller.retryPods)
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))
				ginkgo.By("Marking myPod as completed should free IP")
				myPod.Status.Phase = v1.PodSucceeded

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).UpdateStatus(context.TODO(),
					myPod, metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// port should be gone or marked for removal in logical port cache
				logicalPort := util.GetLogicalPortName(myPod.Namespace, myPod.Name, "", defaultNetNameInfo)
				gomega.Eventually(func() bool {
					info, err := fakeOvn.controller.logicalPortCache.get(logicalPort)
					return err != nil || !info.expires.IsZero()
				}, 2).Should(gomega.BeTrue())

				ginkgo.By("Freed IP should now allow mypod2 to come up")
				key, err := getResourceKey(factory.PodType, myPod2)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				setRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()
				// there should also be no entry for this pod in the retry cache
				gomega.Eventually(func() bool {
					return checkRetryObj(myPod2Key, fakeOvn.controller.retryPods)
				}, retryObjInterval+time.Second).Should(gomega.BeFalse())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				}, 2).Should(gomega.MatchJSON(t2.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t2}, []string{"node1"})))
				// 2nd pod should now have the IP
				myPod2, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(),
					t2.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				ginkgo.By("Updating the completed pod should not free the IP")
				patch := struct {
					Metadata map[string]interface{} `json:"metadata"`
				}{
					Metadata: map[string]interface{}{
						"annotations": map[string]string{"dummy": "data"},
					},
				}
				patchData, err := json.Marshal(&patch)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// trigger update event
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Patch(context.TODO(), myPod.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// sleep a small amount to ensure the event was processed
				time.Sleep(time.Second)
				// try to allocate the IP and it should not work
				annotation, err := util.UnmarshalPodAnnotation(myPod2.Annotations, ovntypes.DefaultNetworkName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.lsManager.AllocateIPs(t.nodeName, annotation.IPs)
				gomega.Expect(err).To(gomega.Equal(ipallocator.ErrAllocated))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t2}, []string{"node1"})))

				ginkgo.By("Deleting the completed pod should not allow a third pod to take the IP")
				// now delete the completed pod
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(),
					t.podName, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// now create a 3rd pod
				t3 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod3",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				myPod3, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					newPod(t3.namespace, t3.podName, t3.nodeName, t3.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t3.namespace, t3.podName)
				}, 2).Should(gomega.HaveLen(0))

				// should be in retry because there are no more IPs left
				myPod3Key, err := getResourceKey(factory.PodType, myPod3)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				checkRetryObjectEventually(myPod3Key, true, fakeOvn.controller.retryPods)
				// TODO validate that the pods also have correct GW SNATs and route policies
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t2}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("should not allocate a completed pod on start up", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				myPod := newPod(t.namespace, t.podName, t.nodeName, t.podIP)
				myPod.Status.Phase = v1.PodSucceeded

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{*myPod},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				expectedData := []libovsdbtest.TestData{getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})}
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(namespaceT.Name, []string{})
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("retryPod cache operations while adding a new pod", func() {
			app.Action = func(ctx *cli.Context) error {
				config.Gateway.DisableSNATMultipleGWs = true
				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				pod, _ := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(pod).To(gomega.BeNil())

				podObj := &v1.Pod{
					Spec: v1.PodSpec{NodeName: "node1"},
					ObjectMeta: metav1.ObjectMeta{
						Name:      t.podName,
						Namespace: namespaceT.Name,
					},
				}
				err = fakeOvn.controller.ensurePod(nil, podObj, true) // this fails since pod doesn't exist to set annotations
				gomega.Expect(err).To(gomega.HaveOccurred())

				key, err := getResourceKey(factory.PodType, podObj)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(retryObjsLen(fakeOvn.controller.retryPods)).To(gomega.Equal(0))
				initRetryObjWithAdd(podObj, key, fakeOvn.controller.retryPods)
				gomega.Expect(retryObjsLen(fakeOvn.controller.retryPods)).To(gomega.Equal(1))
				gomega.Expect(checkRetryObj(key, fakeOvn.controller.retryPods)).To(gomega.BeTrue())
				retryEntry, found := getRetryObj(key, fakeOvn.controller.retryPods)
				gomega.Expect(found).To(gomega.BeTrue())
				storedPod, ok := retryEntry.newObj.(*v1.Pod)
				gomega.Expect(ok).To(gomega.BeTrue())
				gomega.Expect(storedPod.UID).To(gomega.Equal(podObj.UID))

				deleteRetryObj(key, fakeOvn.controller.retryPods)
				gomega.Expect(checkRetryObj(key, fakeOvn.controller.retryPods)).To(gomega.BeFalse())
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries a failure while adding a pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := newPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)

				key, err := getResourceKey(factory.PodType, pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(podTest.namespace, []string{podTest.podIP})
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})...))

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())
				// trigger pod add which will fail with "context deadline exceeded: while awaiting reconnection"
				fakeOvn.controller.WatchPods()

				// sleep long enough for TransactWithRetry to fail, causing pod add to fail
				time.Sleep(ovntypes.OVSDBTimeout + time.Second)

				// check to see if the pod retry cache has an entry for this policy
				checkRetryObjectEventually(key, true, fakeOvn.controller.retryPods)
				connCtx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)

				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				// reset backoff for immediate retry
				setRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs() // retry the failed entry

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.asf.ExpectAddressSetWithIPs(podTest.namespace, []string{podTest.podIP})
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{podTest}, []string{"node1"})...))
				// check the retry cache no longer has the entry
				checkRetryObjectEventually(key, false, fakeOvn.controller.retryPods)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly retries a failure while deleting a pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := newPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)
				expectedData := []libovsdbtest.TestData{getExpectedDataPodsAndSwitches([]testPod{podTest}, []string{"node1"})}
				key, err := getResourceKey(factory.PodType, pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(podTest.namespace, []string{podTest.podIP})

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())
				// trigger pod delete which will fail with "context deadline exceeded: while awaiting reconnection"
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing pod delete to fail
				time.Sleep(ovntypes.OVSDBTimeout + time.Second)

				// check to see if the pod retry cache has an entry for this pod
				checkRetryObjectEventually(key, true, fakeOvn.controller.retryPods)
				connCtx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)

				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)
				setRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs() // retry the failed entry

				fakeOvn.asf.ExpectEmptyAddressSet(podTest.namespace)
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})...))
				// check the retry cache no longer has the entry
				checkRetryObjectEventually(key, false, fakeOvn.controller.retryPods)
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly stops retrying adding a pod after failing n times", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := newPod(podTest.namespace, podTest.podName,
					podTest.nodeName, podTest.podIP)

				key, err := getResourceKey(factory.PodType, pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.asf.ExpectAddressSetWithIPs(podTest.namespace, []string{podTest.podIP})
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})...))

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// trigger pod add, which will fail with "context deadline exceeded: while awaiting reconnection"
				fakeOvn.controller.WatchPods()
				// sleep long enough for TransactWithRetry to fail, causing pod add to fail
				time.Sleep(ovntypes.OVSDBTimeout + time.Second)

				// wait until retry entry appears
				checkRetryObjectEventually(key, true, fakeOvn.controller.retryPods)

				// check that the retry entry is marked for creation
				retryEntry, found := getRetryObj(key, fakeOvn.controller.retryPods)
				gomega.Expect(found).To(gomega.BeTrue())
				gomega.Expect(retryEntry.oldObj).To(gomega.BeNil())
				gomega.Expect(retryEntry.newObj).ToNot(gomega.BeNil())
				gomega.Expect(retryEntry.failedAttempts).To(gomega.Equal(uint8(1)))
				// set failedAttempts to maxFailedAttempts-1, trigger a retry (which will fail due to nbdb being down)
				// and verify that failedAttempts is now equal to maxFailedAttempts
				setFailedAttemptsCounterForTestingOnly(key, maxFailedAttempts-1, fakeOvn.controller.retryPods)
				// reset backoff for immediate retry
				setRetryObjWithNoBackoff(key, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()
				gomega.Eventually(func() uint8 {
					entry, found := getRetryObj(key, fakeOvn.controller.retryPods)
					gomega.Expect(found).To(gomega.BeTrue())
					return entry.failedAttempts
				}).Should(gomega.Equal(uint8(maxFailedAttempts))) // no more retries are allowed

				// restore nbdb, trigger a retry and verify that the retry entry gets deleted
				// because it reached maxFailedAttempts and the corresponding pod has NOT been added to OVN
				connCtx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				fakeOvn.controller.retryPods.RequestRetryObjs()
				// check that pod is in API server
				pod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(
					context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(pod).NotTo(gomega.BeNil())

				// check that the retry cache no longer has the entry
				checkRetryObjectEventually(key, false, fakeOvn.controller.retryPods)

				// check that pod doesn't appear in OVN
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(
					getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly stops retrying deleting a pod after failing n times", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := newPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)
				expectedData := []libovsdbtest.TestData{getExpectedDataPodsAndSwitches(
					[]testPod{podTest},
					[]string{"node1"})}
				key, err := getResourceKey(factory.PodType, pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(
					fakeOvn,
					getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.controller.WatchPods()

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(
					context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(podTest.namespace, []string{podTest.podIP})

				// inject transient problem, nbdb is down
				fakeOvn.controller.nbClient.Close()
				gomega.Eventually(func() bool {
					return fakeOvn.controller.nbClient.Connected()
				}).Should(gomega.BeFalse())

				// trigger pod delete, which will fail with "context deadline exceeded: while awaiting reconnection"
				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(
					context.TODO(), pod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// sleep long enough for TransactWithRetry to fail, causing pod delete to fail
				time.Sleep(ovntypes.OVSDBTimeout + time.Second)

				// wait until retry entry appears
				checkRetryObjectEventually(key, true, fakeOvn.controller.retryPods)

				// check that the retry entry is marked for deletion
				retryEntry, found := getRetryObj(key, fakeOvn.controller.retryPods)
				gomega.Expect(found).To(gomega.BeTrue())
				gomega.Expect(retryEntry.oldObj).ToNot(gomega.BeNil())
				gomega.Expect(retryEntry.newObj).To(gomega.BeNil())
				gomega.Expect(retryEntry.failedAttempts).To(gomega.Equal(uint8(1)))

				// set failedAttempts to maxFailedAttempts-1, trigger a retry (which will fail due to nbdb),
				// check that failedAttempts is now equal to maxFailedAttempts
				setFailedAttemptsCounterForTestingOnly(key, maxFailedAttempts-1, fakeOvn.controller.retryPods)
				fakeOvn.controller.retryPods.RequestRetryObjs()
				gomega.Eventually(func() uint8 {
					entry, found := getRetryObj(key, fakeOvn.controller.retryPods)
					gomega.Expect(found).To(gomega.BeTrue())
					return entry.failedAttempts
				}).Should(gomega.Equal(uint8(maxFailedAttempts))) // no more retries are allowed

				// restore nbdb and verify that the retry entry gets deleted because it reached
				// maxFailedAttempts and the corresponding pod has NOT been deleted from OVN
				connCtx, cancel := context.WithTimeout(context.Background(), ovntypes.OVSDBTimeout)
				defer cancel()
				resetNBClient(connCtx, fakeOvn.controller.nbClient)

				fakeOvn.controller.retryPods.RequestRetryObjs()

				// check that the pod is not in API server
				pod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(
					context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(pod).To(gomega.BeNil())

				// check that the retry cache no longer has the entry
				checkRetryObjectEventually(key, false, fakeOvn.controller.retryPods)

				// check that the pod is still in OVN
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("correctly remove a LSP from a pod that has stale nodeName annotation", func() {
			app.Action = func(ctx *cli.Context) error {
				namespace1 := *newNamespace("namespace1")
				podTest := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespace1.Name,
				)
				pod := newPod(podTest.namespace, podTest.podName, podTest.nodeName, podTest.podIP)
				expectedData := []libovsdbtest.TestData{getExpectedDataPodsAndSwitches([]testPod{podTest}, []string{"node1"})}
				key, err := getResourceKey(factory.PodType, pod)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespace1,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{*pod},
					},
				)

				podTest.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectedData...))
				fakeOvn.asf.ExpectAddressSetWithIPs(podTest.namespace, []string{podTest.podIP})

				// Get pod from api with its metadata filled in
				pod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// Fudge nodename from pod's spec, to ensure it is not used by deleteLogicalPort
				pod.Spec.NodeName = "this_is_the_wrong_nodeName"

				// Deleting port from a pod that has no portInfo and the wrong nodeName should still be okay!
				err = fakeOvn.controller.deleteLogicalPort(pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// OVN db should be empty now
				fakeOvn.asf.ExpectEmptyAddressSet(podTest.namespace)
				gomega.Eventually(fakeOvn.controller.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})...))

				// Once again, get pod from api with its metadata filled in
				pod, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(podTest.namespace).Get(context.TODO(), podTest.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Expect(fakeOvn.controller.deleteLogicalPort(pod, nil)).To(gomega.Succeed(), "Deleting port that no longer exists should be okay")

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// check the retry cache has no entry
				checkRetryObjectEventually(key, false, fakeOvn.controller.retryPods)

				// Remove Logical Switch created on behalf of node and make sure deleteLogicalPort will not fail
				err = libovsdbops.DeleteLogicalSwitch(fakeOvn.controller.nbClient, pod.Spec.NodeName)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(fakeOvn.controller.deleteLogicalPort(pod, nil)).To(gomega.Succeed(), "Deleting port from switch that no longer exists should be okay")

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("remove a LSP from a pod that has no OVN annotations", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				pod := newPod(t.namespace, t.podName, t.nodeName, t.podIP)
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*pod,
						},
					},
				)
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				gomega.Expect(annotations).To(gomega.Equal(""))

				// Deleting port from a pod that has no annotations should be okay
				err := fakeOvn.controller.deleteLogicalPort(pod, nil)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles a deleted pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				// Setup an assigned pod
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))

				err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, *metav1.NewDeleteOptions(0))
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).To(gomega.HaveOccurred())
				gomega.Expect(pod).To(gomega.BeNil())

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("retries a failed pod Add on Update", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				// Setup an unassigned pod, perform an update later on which assigns it.
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				// allow pod retry from update annotation to fail
				time.Sleep(2 * time.Second)

				patch := struct {
					Metadata map[string]interface{} `json:"metadata"`
				}{
					Metadata: map[string]interface{}{
						"annotations": map[string]string{"dummy": "data"},
					},
				}
				patchData, err := json.Marshal(&patch)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				// trigger update event
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Patch(context.TODO(), t.podName, types.MergePatchType, patchData, metav1.PatchOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("pod Add should succeed even when namespace doesn't yet exist", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				podJSON := t.getAnnotationsJson()

				fakeOvn.startWithDBSetup(initialDB)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				// Add pod before namespace; pod will be annotated
				// but namespace address set will not exist
				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(podJSON))

				// Add Pod logical port should succeed even without namespace
				gomega.Expect(getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)).Should(gomega.MatchJSON(podJSON))

				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Context("on startup", func() {

		ginkgo.It("reconciles a new pod", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*newPod(t.namespace, t.podName, t.nodeName, t.podIP),
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				gomega.Expect(ok).To(gomega.BeFalse())

				err = fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				// check that after start networks annotations and nbdb will be updated
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))
				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing pod without an existing logical switch port", func() {
			app.Action = func(ctx *cli.Context) error {

				namespaceT := *newNamespace("namespace1")
				t := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				pod := newPod(t.namespace, t.podName, t.nodeName, t.podIP)
				setPodAnnotations(pod, t)
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*pod,
						},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				// pod annotations exist, lsp doesn't
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t.getAnnotationsJson()))

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				// check nbdb data is added
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t}, []string{"node1"})))
				// check that the pod annotations are preserved
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing logical switch port without an existing pod", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				// create ovsdb with no pod
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      "namespace1_non-existing-pod-UUID",
							Name:      "namespace1_non-existing-pod",
							Addresses: []string{"0a:58:0a:80:02:03", "10.128.2.3"},
							ExternalIDs: map[string]string{
								"pod": "true",
							},
						},
						&nbdb.LogicalSwitch{
							UUID:  "ls-uuid",
							Name:  "node1",
							Ports: []string{"namespace1_non-existing-pod-UUID"},
						},
					},
				}

				testNode := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
					},
				}

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							testNode,
						},
					},
					// no pods
					&v1.PodList{
						Items: []v1.Pod{},
					},
				)

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()
				// expect stale logical switch port removed and stale logical switch port removed from logical switch
				expectData := []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "ls-uuid",
						Name:  "node1",
						Ports: []string{},
					},
				}

				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(expectData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())

		})

		ginkgo.It("reconciles an existing pod with an existing logical switch port", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				// use 3 pods for different test options
				t1 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				t2 := newTPod(
					"node2",
					"10.128.2.0/24",
					"10.128.2.2",
					"10.128.2.1",
					"myPod2",
					"10.128.2.3",
					"0a:58:0a:80:02:03",
					namespaceT.Name,
				)
				t3 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod3",
					"10.128.1.4",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				// add an outdated hybrid route for pod 3 to the hybrid overlay IF addr on the node
				t3Route := util.PodRoute{}
				_, t3Route.Dest, _ = net.ParseCIDR("10.132.0.0/14")
				_, nodeSubnet, _ := net.ParseCIDR(t3.nodeSubnet)
				t3Route.NextHop = util.GetNodeHybridOverlayIfAddr(nodeSubnet).IP
				t3.routes = []util.PodRoute{t3Route}

				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      t1.portUUID,
							Name:      util.GetLogicalPortName(t1.namespace, t1.podName, "", defaultNetNameInfo),
							Addresses: []string{t1.podMAC, t1.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t1.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								"requested-chassis": t2.nodeName,
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t1.podMAC, t1.podIP)},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      t2.portUUID,
							Name:      util.GetLogicalPortName(t2.namespace, t2.podName, "", defaultNetNameInfo),
							Addresses: []string{t2.podMAC, t2.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t2.namespace,
							},
							Options: map[string]string{
								"requested-chassis": t2.nodeName,
								//"iface-id-ver": is empty to check that it won't be set on update
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t2.podMAC, t2.podIP)},
						},
						&nbdb.LogicalSwitchPort{
							UUID:      t3.portUUID,
							Name:      util.GetLogicalPortName(t3.namespace, t3.podName, "", defaultNetNameInfo),
							Addresses: []string{t3.podMAC, t3.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t3.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								"requested-chassis": t3.nodeName,
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t3.podMAC, t3.podIP)},
						},
						&nbdb.LogicalSwitch{
							Name:  "node1",
							Ports: []string{t1.portUUID, t3.portUUID},
						},
						&nbdb.LogicalSwitch{
							Name:  "node2",
							Ports: []string{t2.portUUID},
						},
					},
				}
				// update TestPod to check nbdb lsp later
				t2.noIfaceIdVer = true

				pod1 := newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				pod2 := newPod(t2.namespace, t2.podName, t2.nodeName, t2.podIP)
				setPodAnnotations(pod2, t2)
				pod3 := newPod(t3.namespace, t3.podName, t3.nodeName, t3.podIP)
				setPodAnnotations(pod3, t3)
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*pod1,
							*pod2,
							*pod3,
						},
					},
				)
				t1.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				t2.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node2"))
				t3.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				// pod annotations and lsp exist now

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				// check db values are updated to correlate with test pods settings
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t1, t2, t3}, []string{"node1", "node2"})))
				// check annotations are preserved
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t1.namespace, t1.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t1.getAnnotationsJson()))
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t2.getAnnotationsJson()))
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t3.namespace, t3.podName)
				// remove the outdated route from pod t3
				t3.routes = []util.PodRoute{}
				gomega.Expect(annotations).To(gomega.MatchJSON(t3.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("Negative test: fails to add existing pod with an existing logical switch port on wrong node", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")
				// use 2 pods for different test options
				t1 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)

				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      t1.portUUID,
							Name:      util.GetLogicalPortName(t1.namespace, t1.podName, "", defaultNetNameInfo),
							Addresses: []string{t1.podMAC, t1.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t1.namespace,
							},
							Options: map[string]string{
								// check requested-chassis will be updated to correct t1.nodeName value
								"requested-chassis": t1.nodeName,
								// check old value for iface-id-ver will be updated to pod.UID
								"iface-id-ver": "wrong_value",
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t1.podMAC, t1.podIP)},
						},
						&nbdb.LogicalSwitch{
							Name:  "node1",
							Ports: []string{},
						},
						&nbdb.LogicalSwitch{
							Name:  "node2",
							Ports: []string{t1.portUUID},
						},
					},
				}

				pod1 := newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*pod1,
						},
					},
				)
				t1.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				// pod annotations and lsp exist now

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				// should fail to update a port on the wrong switch
				myPod1Key, err := getResourceKey(factory.PodType, pod1)
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				checkRetryObjectEventually(myPod1Key, true, fakeOvn.controller.retryPods)

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
		ginkgo.It("correctly configures hybridOverlay: allocates the address on the node annotation", func() {
			app.Action = func(ctx *cli.Context) error {
				config.HybridOverlay.Enabled = true
				namespaceT := *newNamespace("namespace1")
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{
							UUID:  "ls-uuid",
							Name:  "node1",
							Ports: []string{},
						},
					},
				}

				testNode := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							hotypes.HybridOverlayDRIP: "10.128.1.53"},
					},
				}

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							testNode,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{},
					},
				)
				fakeOvn.controller.lsManager.AddNode("node1", "ls-uuid", []*net.IPNet{ovntest.MustParseIPNet("10.128.1.0/24")})

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectData := []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "ls-uuid",
						Name:  "node1",
						Ports: []string{},
					},
				}

				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(expectData))
				err = fakeOvn.controller.lsManager.AllocateIPs("node1", []*net.IPNet{ovntest.MustParseIPNet("10.128.1.53/32")})
				gomega.Expect(err).To(gomega.Equal(ipallocator.ErrAllocated))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			node, err := fakeOvn.controller.kube.GetNode("node1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(node.Annotations[hotypes.HybridOverlayDRIP]).To(gomega.Equal("10.128.1.53"))

		})
		ginkgo.It("correctly configures hybridOverlay: allocates the .3 address because it is available", func() {
			app.Action = func(ctx *cli.Context) error {
				config.HybridOverlay.Enabled = true
				namespaceT := *newNamespace("namespace1")
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitch{
							UUID:  "ls-uuid",
							Name:  "node1",
							Ports: []string{},
						},
					},
				}

				testNode := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
					},
				}

				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							testNode,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{},
					},
				)
				fakeOvn.controller.lsManager.AddNode("node1", "ls-uuid", []*net.IPNet{ovntest.MustParseIPNet("10.128.1.0/24")})

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				err = fakeOvn.controller.WatchPods()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				expectData := []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  "ls-uuid",
						Name:  "node1",
						Ports: []string{},
					},
				}

				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(expectData))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			node, err := fakeOvn.controller.kube.GetNode("node1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(node.Annotations[hotypes.HybridOverlayDRIP]).To(gomega.Equal("10.128.1.3"))
			err = fakeOvn.controller.lsManager.AllocateIPs("node1", []*net.IPNet{ovntest.MustParseIPNet("10.128.1.3/32")})
			gomega.Expect(err).To(gomega.Equal(ipallocator.ErrAllocated))

		})
		ginkgo.It("correctly configures hybridOverlay: allocates an address because the .3 is not available", func() {
			app.Action = func(ctx *cli.Context) error {
				config.HybridOverlay.Enabled = true
				namespaceT := *newNamespace("namespace1")
				t1 := newTPod(
					"node1",
					"10.128.1.0/24",
					"10.128.1.2",
					"10.128.1.1",
					"myPod1",
					"10.128.1.3",
					"0a:58:0a:80:01:03",
					namespaceT.Name,
				)
				testNode := v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
					},
				}
				initialDB = libovsdbtest.TestSetup{
					NBData: []libovsdbtest.TestData{
						&nbdb.LogicalSwitchPort{
							UUID:      t1.portUUID,
							Name:      util.GetLogicalPortName(t1.namespace, t1.podName, "", defaultNetNameInfo),
							Addresses: []string{t1.podMAC, t1.podIP},
							ExternalIDs: map[string]string{
								"pod":       "true",
								"namespace": t1.namespace,
							},
							Options: map[string]string{
								"requested-chassis": t1.nodeName,
							},
							PortSecurity: []string{fmt.Sprintf("%s %s", t1.podMAC, t1.podIP)},
						},
						&nbdb.LogicalSwitch{
							Name:  "node1",
							Ports: []string{t1.portUUID},
						},
					},
				}
				// update TestPod to check nbdb lsp later
				t1.noIfaceIdVer = true

				pod1 := newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				fakeOvn.startWithDBSetup(initialDB,
					&v1.NamespaceList{
						Items: []v1.Namespace{
							namespaceT,
						},
					},
					&v1.PodList{
						Items: []v1.Pod{
							*pod1,
						},
					},
					&v1.NodeList{
						Items: []v1.Node{
							testNode,
						},
					},
				)
				t1.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				// pod annotations and lsp exist now

				err := fakeOvn.controller.WatchNamespaces()
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				fakeOvn.controller.WatchPods()

				// check db values are updated to correlate with test pods settings
				gomega.Eventually(fakeOvn.nbClient).Should(
					libovsdbtest.HaveData(getExpectedDataPodsAndSwitches([]testPod{t1}, []string{"node1"})))
				// check annotations are preserved
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t1.namespace, t1.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t1.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			node, err := fakeOvn.controller.kube.GetNode("node1")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(node.Annotations[hotypes.HybridOverlayDRIP]).To(gomega.Equal("10.128.1.4"))
			err = fakeOvn.controller.lsManager.AllocateIPs("node1", []*net.IPNet{ovntest.MustParseIPNet("10.128.1.4/32")})
			gomega.Expect(err).To(gomega.Equal(ipallocator.ErrAllocated))
		})
	})
})
