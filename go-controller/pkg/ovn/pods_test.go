package ovn

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v2"

	ovstypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"

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
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
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
	noIfaceIdVer bool
}

func newTPod(nodeName, nodeSubnet, nodeMgtIP, nodeGWIP, podName, podIP, podMAC, namespace string) (to testPod) {
	to = testPod{
		nodeName:   nodeName,
		nodeSubnet: nodeSubnet,
		nodeMgtIP:  nodeMgtIP,
		nodeGWIP:   nodeGWIP,
		podName:    podName,
		podIP:      podIP,
		podMAC:     podMAC,
		namespace:  namespace,
		portName:   util.GetLogicalPortName(namespace, podName),
		portUUID:   libovsdbops.BuildNamedUUID(),
	}
	return
}

func (p testPod) populateLogicalSwitchCache(fakeOvn *FakeOVN, uuid string) {
	gomega.Expect(p.nodeName).NotTo(gomega.Equal(""))
	fakeOvn.controller.lsManager.AddNode(p.nodeName, uuid, []*net.IPNet{ovntest.MustParseIPNet(p.nodeSubnet)})
}

func (p testPod) getAnnotationsJson() string {
	return `{"default": {"ip_addresses":["` + p.podIP + `/24"], "mac_address":"` + p.podMAC + `", 
		"gateway_ips": ["` + p.nodeGWIP + `"], "ip_address":"` + p.podIP + `/24", "gateway_ip": "` + p.nodeGWIP + `"}}`
}

func setPodAnnotations(podObj *v1.Pod, testPod testPod) {
	podAnnot := map[string]string{
		util.OvnPodAnnotationName: testPod.getAnnotationsJson(),
	}
	podObj.Annotations = podAnnot
}

func getLogicalSwitchUUID(client libovsdbclient.Client, name string) string {
	ctext, cancel := context.WithTimeout(context.Background(), ovstypes.OVSDBTimeout)
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

func getExpectedDataPodsAndSwitches(pods []testPod, lsws []*lswRef) []libovsdbtest.TestData {
	nodeslsps := make(map[string][]string)
	var logicalSwitchPorts []*nbdb.LogicalSwitchPort
	for _, pod := range pods {
		var lspUUID string
		if len(pod.portUUID) == 0 {
			lspUUID = libovsdbops.BuildNamedUUID()
		} else {
			lspUUID = pod.portUUID
		}
		podAddr := fmt.Sprintf("%s %s", pod.podMAC, pod.podIP)
		lsp := &nbdb.LogicalSwitchPort{
			UUID:      lspUUID,
			Name:      util.GetLogicalPortName(pod.namespace, pod.podName),
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
	for _, lsw := range lsws {
		logicalSwitches = append(logicalSwitches, &nbdb.LogicalSwitch{
			UUID:  lsw.uuid,
			Name:  lsw.name,
			Ports: nodeslsps[lsw.name],
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

func addPodToTestData(db libovsdbtest.TestSetup, t *testPod, options map[string]string) libovsdbtest.TestSetup {
	if len(options) == 0 {
		options = map[string]string{
			"requested-chassis": t.nodeName,
			"iface-id-ver":      t.podName,
		}
	}
	db.NBData = append(db.NBData,
		&nbdb.LogicalSwitchPort{
			UUID:      t.portUUID,
			Name:      util.GetLogicalPortName(t.namespace, t.podName),
			Addresses: []string{t.podMAC, t.podIP},
			ExternalIDs: map[string]string{
				"pod":       "true",
				"namespace": t.namespace,
			},
			Options:      options,
			PortSecurity: []string{t.podMAC, t.podIP},
		},
	)

	// Add the port to the switch
	switchFound := false
	for _, e := range db.NBData {
		lsw, ok := e.(*nbdb.LogicalSwitch)
		if !ok || lsw.Name != t.nodeName {
			continue
		}
		switchFound = true
		found := false
		for _, p := range lsw.Ports {
			if p == t.portUUID {
				found = true
				break
			}
		}
		if !found {
			lsw.Ports = append(lsw.Ports, t.portUUID)
		}
		break
	}
	gomega.Expect(switchFound).To(gomega.BeTrue())

	return db
}

type lswRef struct {
	uuid string
	name string
}

func findLSWRefs(db libovsdbtest.TestSetup, names ...string) []*lswRef {
	refs := make([]*lswRef, 0, len(names))
	for _, name := range names {
		ref := &lswRef{name: name}
		for _, e := range db.NBData {
			if lsw, ok := e.(*nbdb.LogicalSwitch); ok {
				ginkgo.GinkgoT().Logf("##### lsw %s %s", lsw.Name, lsw.UUID)
				if lsw.Name == name {
					ref.uuid = lsw.UUID
					break
				}
			}
		}
		gomega.Expect(ref.uuid).NotTo(gomega.BeEmpty())
		refs = append(refs, ref)
	}
	return refs
}

var _ = ginkgo.Describe("OVN Pod Operations", func() {
	var (
		app       *cli.App
		fakeOvn   *FakeOVN
		initialDB libovsdbtest.TestSetup
	)

	ginkgo.BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		fakeOvn = NewFakeOVN()
		initialDB = libovsdbtest.TestSetup{
			NBData: []libovsdbtest.TestData{
				&nbdb.LogicalSwitch{
					UUID: uuid.NewString(),
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
					&v1.NodeList{
						Items: []v1.Node{{ObjectMeta: newObjectMeta(t.nodeName, "")}},
					},
				)

				ctext, cancel := context.WithTimeout(context.Background(), ovstypes.OVSDBTimeout)
				defer cancel()
				lsl := []nbdb.LogicalSwitch{}
				err := fakeOvn.controller.nbClient.WhereCache(
					func(ls *nbdb.LogicalSwitch) bool {
						return ls.Name == "node1"
					}).List(ctext, &lsl)

				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Expect(len(lsl)).To(gomega.Equal(1))

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				gomega.Expect(ok).To(gomega.BeFalse())

				// Assign it and perform the update
				t.nodeName = "node1"
				t.portName = util.GetLogicalPortName(t.namespace, t.podName)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				_, err = fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Update(context.TODO(),
					newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.UpdateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))
				expectData := getExpectedDataPodsAndSwitches([]testPod{t}, findLSWRefs(initialDB, "node1"))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))

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
					&v1.NodeList{
						Items: []v1.Node{{ObjectMeta: newObjectMeta(t.nodeName, "")}},
					},
				)

				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

				pod, _ := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(pod).To(gomega.BeNil())

				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(),
					newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				expectData := getExpectedDataPodsAndSwitches([]testPod{t}, findLSWRefs(initialDB, "node1"))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))
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
					&v1.NodeList{
						Items: []v1.Node{{ObjectMeta: newObjectMeta(t.nodeName, "")}},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Delete(context.TODO(), t.podName, metav1.DeleteOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				expectData := getExpectedDataPodsAndSwitches([]testPod{t}, findLSWRefs(initialDB, "node1"))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))
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

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

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

				expectData := getExpectedDataPodsAndSwitches([]testPod{t}, findLSWRefs(initialDB, "node1"))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))
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
				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

				// Add pod before namespace; pod will be annotated
				// but namespace address set will not exist
				_, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Create(context.TODO(), newPod(t.namespace, t.podName, t.nodeName, t.podIP), metav1.CreateOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				gomega.Eventually(func() string { return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName) }, 2).Should(gomega.MatchJSON(podJSON))

				// Add Pod logical port should succeed even without namespace
				gomega.Expect(getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)).Should(gomega.MatchJSON(podJSON))

				expectData := getExpectedDataPodsAndSwitches([]testPod{t}, findLSWRefs(initialDB, "node1"))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))
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

				initialDB = addPodToTestData(initialDB, &t, nil)
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
					&v1.NodeList{
						Items: []v1.Node{{ObjectMeta: newObjectMeta(t.nodeName, "")}},
					},
				)
				t.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				// pod exists, networks annotations don't
				pod, err := fakeOvn.fakeClient.KubeClient.CoreV1().Pods(t.namespace).Get(context.TODO(), t.podName, metav1.GetOptions{})
				gomega.Expect(err).NotTo(gomega.HaveOccurred())

				_, ok := pod.Annotations[util.OvnPodAnnotationName]
				gomega.Expect(ok).To(gomega.BeFalse())

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

				// check that after start networks annotations and nbdb will be updated
				gomega.Eventually(func() string {
					return getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				}, 2).Should(gomega.MatchJSON(t.getAnnotationsJson()))

				expectData := getExpectedDataPodsAndSwitches([]testPod{t}, findLSWRefs(initialDB, t.nodeName))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))
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

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

				// check nbdb data is added
				expectData := getExpectedDataPodsAndSwitches([]testPod{t}, findLSWRefs(initialDB, "node1"))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))

				// check that the pod annotations are preserved
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t.namespace, t.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})

		ginkgo.It("reconciles an existing pod with an existing logical switch port", func() {
			app.Action = func(ctx *cli.Context) error {
				namespaceT := *newNamespace("namespace1")

				initialDB.NBData = append(initialDB.NBData,
					&nbdb.LogicalSwitch{
						UUID: uuid.NewString(),
						Name: "node2",
					},
				)

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
				t1.portUUID = uuid.NewString()
				initialDB = addPodToTestData(initialDB, &t1, map[string]string{
					// check requested-chassis will be updated to correct t1.nodeName value
					"requested-chassis": t1.nodeName,
					// check old value for iface-id-ver will be updated to pod.UID
					"iface-id-ver": "wrong_value",
				})

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
				t2.portUUID = uuid.NewString()
				t2.noIfaceIdVer = true
				initialDB = addPodToTestData(initialDB, &t2, map[string]string{
					"requested-chassis": t2.nodeName,
					//"iface-id-ver": is empty to check that it won't be set on update
				})

				pod1 := newPod(t1.namespace, t1.podName, t1.nodeName, t1.podIP)
				setPodAnnotations(pod1, t1)
				pod2 := newPod(t2.namespace, t2.podName, t2.nodeName, t2.podIP)
				setPodAnnotations(pod2, t2)
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
						},
					},
				)
				t1.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node1"))
				t2.populateLogicalSwitchCache(fakeOvn, getLogicalSwitchUUID(fakeOvn.controller.nbClient, "node2"))
				// pod annotations and lsp exist now

				fakeOvn.controller.WatchNamespaces()
				fakeOvn.InitAndRunPodController()

				// check db values are updated to correlate with test pods settings
				expectData := getExpectedDataPodsAndSwitches([]testPod{t1, t2}, findLSWRefs(initialDB, "node1", "node2"))
				gomega.Eventually(fakeOvn.nbClient).Should(libovsdbtest.HaveData(expectData))
				// check annotations are preserved
				// makes sense only when handling is finished, therefore check after nbdb is updated
				annotations := getPodAnnotations(fakeOvn.fakeClient.KubeClient, t1.namespace, t1.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t1.getAnnotationsJson()))
				annotations = getPodAnnotations(fakeOvn.fakeClient.KubeClient, t2.namespace, t2.podName)
				gomega.Expect(annotations).To(gomega.MatchJSON(t2.getAnnotationsJson()))

				return nil
			}

			err := app.Run([]string{app.Name})
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
