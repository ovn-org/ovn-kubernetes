package zone_tracker

import (
	"context"
	"fmt"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sync/atomic"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	informerfactory "k8s.io/client-go/informers"
)

func getNodeWithZone(nodeName, zoneName string) *corev1.Node {
	annotations := map[string]string{}
	if zoneName != UnknownZone {
		annotations[util.OvnNodeZoneName] = zoneName
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: annotations,
		},
	}
}

var _ = Describe("Cluster Manager Zone Tracker", func() {
	var (
		zoneTracker      *ZoneTracker
		subscribeCounter atomic.Uint64
		fakeClient       *util.OVNClusterManagerClientset
		factoryStopChan  chan struct{}
	)

	const (
		node1 = "node1"
		node2 = "node2"
		node3 = "node3"
		zone1 = "zone1"
		zone2 = "zone2"
	)

	checkZones := func(expectedZones ...string) {
		Eventually(func() bool {
			zoneTracker.zonesLock.RLock()
			defer zoneTracker.zonesLock.RUnlock()
			return zoneTracker.zones.Equal(sets.New[string](expectedZones...))
		}).Should(BeTrue(), fmt.Sprintf("expected zones %v", expectedZones))
	}

	checkUnknownZoneNodes := func(expectedNodes ...string) {
		Eventually(func() bool {
			zoneTracker.zonesLock.RLock()
			defer zoneTracker.zonesLock.RUnlock()
			nodeNames := sets.New[string]()
			for nodeName := range zoneTracker.unknownZoneNodes {
				nodeNames.Insert(nodeName)
			}
			return nodeNames.Equal(sets.New[string](expectedNodes...))
		}).Should(BeTrue(), fmt.Sprintf("expected unknown nodes %v", expectedNodes))
	}

	checkSubscribeCounter := func(expected int) {
		Eventually(subscribeCounter.Load).Should(BeEquivalentTo(expected), fmt.Sprintf("expected %v subscriber calls", expected))
	}

	createNode := func(node *corev1.Node) {
		_, err := fakeClient.KubeClient.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	updateNode := func(node *corev1.Node) {
		_, err := fakeClient.KubeClient.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	deleteNode := func(nodeName string) {
		err := fakeClient.KubeClient.CoreV1().Nodes().Delete(context.TODO(), nodeName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())
	}

	BeforeEach(func() {
		fakeClient = util.GetOVNClientset().GetClusterManagerClientset()
		coreFactory := informerfactory.NewSharedInformerFactory(fakeClient.KubeClient, time.Second)
		zoneTracker = NewZoneTracker(coreFactory.Core().V1().Nodes(), func(newZones sets.Set[string]) {
			subscribeCounter.Add(1)
		})
		// reduce timeout for testing
		zoneTracker.unknownZoneTimeout = 2 * time.Second

		// start
		subscribeCounter.Store(0)
		factoryStopChan = make(chan struct{})
		coreFactory.Start(factoryStopChan)
		err := zoneTracker.Start()
		Expect(err).ToNot(HaveOccurred())

		// subscriber is updated on Subscribe call
		checkSubscribeCounter(1)
		Expect(subscribeCounter.Load()).To(BeEquivalentTo(1))
	})

	AfterEach(func() {
		close(factoryStopChan)
		zoneTracker.Stop()
	})

	It("handles multiple nodes in the same zone", func() {
		createNode(getNodeWithZone(node1, zone1))
		checkZones(zone1)
		checkSubscribeCounter(2)

		createNode(getNodeWithZone(node2, zone1))
		checkZones(zone1)
		// zones didn't change, no callback
		checkSubscribeCounter(2)

		deleteNode(node1)
		checkZones(zone1)
		// zones didn't change, no callback
		checkSubscribeCounter(2)

		deleteNode(node2)
		checkZones()
		checkSubscribeCounter(3)
	})

	It("handles multiple nodes in the different zones", func() {
		createNode(getNodeWithZone(node1, zone1))
		checkZones(zone1)
		checkSubscribeCounter(2)

		createNode(getNodeWithZone(node2, zone2))
		checkZones(zone1, zone2)
		checkSubscribeCounter(3)

		deleteNode(node1)
		checkZones(zone2)
		checkSubscribeCounter(4)

		deleteNode(node2)
		checkZones()
		checkSubscribeCounter(5)
	})

	It("handles UnknownZone node that is never updated", func() {
		createNode(getNodeWithZone(node1, UnknownZone))
		checkZones(UnknownZone)
		checkSubscribeCounter(2)
		time.Sleep(1 * time.Second)

		// update node without zone change, this shouldn't change anything
		updateNode(getNodeWithZone(node1, UnknownZone))
		checkZones(UnknownZone)
		checkSubscribeCounter(2)

		// if in unknownZoneTimeout seconds node zone is not set, unknownZone should be removed
		// subtract 1 second we have already waited, add 100 milliseconds to ensure no race
		time.Sleep(zoneTracker.unknownZoneTimeout - 1*time.Second + 100*time.Millisecond)
		checkZones()
		checkUnknownZoneNodes(node1)
		checkSubscribeCounter(3)

		// another update shouldn't change zone list
		updateNode(getNodeWithZone(node1, UnknownZone))
		checkZones()
		checkUnknownZoneNodes(node1)
		checkSubscribeCounter(3)

		// now cleanup
		deleteNode(node1)
		checkZones()
		checkUnknownZoneNodes()
		// zones didn't change, no callback
		checkSubscribeCounter(3)
	})

	It("handles UnknownZone node that is updated before timeout", func() {
		createNode(getNodeWithZone(node1, UnknownZone))
		checkZones(UnknownZone)
		checkUnknownZoneNodes(node1)
		checkSubscribeCounter(2)

		// update node zone
		updateNode(getNodeWithZone(node1, zone1))
		checkZones(zone1)
		checkUnknownZoneNodes()
		checkSubscribeCounter(3)

		// make sure in unknownZoneTimeout seconds node won't be added to the unknown list
		// add 100 milliseconds to ensure no race
		time.Sleep(zoneTracker.unknownZoneTimeout + 100*time.Millisecond)
		checkZones(zone1)
		checkUnknownZoneNodes()
		// zones didn't change, no callback
		checkSubscribeCounter(3)

		// now cleanup
		deleteNode(node1)
		checkZones()
		checkUnknownZoneNodes()
		checkSubscribeCounter(4)
	})

	It("handles UnknownZone node that is updated after timeout", func() {
		createNode(getNodeWithZone(node1, UnknownZone))
		checkZones(UnknownZone)
		checkSubscribeCounter(2)

		// wait for node to be added to unknownZoneNodes
		time.Sleep(zoneTracker.unknownZoneTimeout + 100*time.Millisecond)
		checkZones()
		checkUnknownZoneNodes(node1)
		checkSubscribeCounter(3)

		// update node zone, it should be removed from the unknownZoneNodes
		updateNode(getNodeWithZone(node1, zone1))
		checkZones(zone1)
		checkUnknownZoneNodes()
		checkSubscribeCounter(4)

		// now cleanup
		deleteNode(node1)
		checkZones()
		checkUnknownZoneNodes()
		checkSubscribeCounter(5)
	})

	It("handles UnknownZone node that is re-created", func() {
		createNode(getNodeWithZone(node1, UnknownZone))
		checkZones(UnknownZone)
		checkUnknownZoneNodes(node1)
		checkSubscribeCounter(2)

		// re-create node 500ms before timeout
		time.Sleep(zoneTracker.unknownZoneTimeout - 500*time.Millisecond)
		deleteNode(node1)
		checkZones()
		checkSubscribeCounter(3)
		createNode(getNodeWithZone(node1, UnknownZone))

		// wait for the first timeout (500 ms + 100)and ensure unknownZone is still present
		time.Sleep(500*time.Millisecond + 100*time.Millisecond)
		checkZones(UnknownZone)
		checkUnknownZoneNodes(node1)
		checkSubscribeCounter(4)

		// wait for the second timeout and ensure unknownZone is removed
		time.Sleep(zoneTracker.unknownZoneTimeout)
		checkZones()
		checkUnknownZoneNodes(node1)
		checkSubscribeCounter(5)
	})

	It("handles regular cluster start up", func() {
		createNode(getNodeWithZone(node1, UnknownZone))
		createNode(getNodeWithZone(node2, UnknownZone))
		createNode(getNodeWithZone(node3, UnknownZone))
		checkZones(UnknownZone)
		checkSubscribeCounter(2)

		updateNode(getNodeWithZone(node1, zone1))
		checkZones(UnknownZone, zone1)
		checkSubscribeCounter(3)

		updateNode(getNodeWithZone(node2, zone2))
		checkZones(UnknownZone, zone1, zone2)
		checkSubscribeCounter(4)

		updateNode(getNodeWithZone(node3, zone2))
		checkZones(zone1, zone2)
		checkSubscribeCounter(5)
	})

	It("adds node that already exists without triggering subscribers", func() {
		createNode(getNodeWithZone(node1, zone1))
		checkZones(zone1)
		checkSubscribeCounter(2)

		updateNode(getNodeWithZone(node1, zone1))
		// zones didn't change, no callback
		checkZones(zone1)
		checkSubscribeCounter(2)
	})

	It("deletes node that doesn't exists without triggering subscribers", func() {
		err := zoneTracker.reconcileNode(node1)
		Expect(err).ToNot(HaveOccurred())
		checkZones()
		checkUnknownZoneNodes()
		// zones didn't change, no callback
		checkSubscribeCounter(1)
	})
})
