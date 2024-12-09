package udnenabledsvc

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestUDNEnabledServices(t *testing.T) {

	const (
		service1Namespace  = "default"
		service1Name       = "kubernetes"
		service1NsName     = service1Namespace + "/" + service1Name
		service2Namespace  = "dns"
		service2Name       = "dns"
		service2NsName     = service2Namespace + "/" + service2Name
		service1ClusterIP1 = "10.96.0.1"
		service1ClusterIP2 = "10.96.0.20"
		service2ClusterIP1 = "10.96.1.5"
	)

	tests := []struct {
		name               string            // test description
		initialServices    []runtime.Object  // initial services populated within k8 api before controller is running
		initialClusterIPs  []string          // initial cluster IPs added to the address set before the controller is running
		afterRunServices   []*corev1.Service // services added after controller is running
		expectedClusterIPs sets.Set[string]  // expected set of clusterIPs that we expect to be consistently present shortly after run controller is invoked
		udnEnabledServices []string
	}{
		{
			name:               "no services are specified or selected",
			expectedClusterIPs: sets.New[string](),
		},
		{
			name: "add services before runtime",
			initialServices: []runtime.Object{
				getService(service1Namespace, service1Name, service1ClusterIP1, service1ClusterIP2),
				getService(service2Namespace, service2Name, service2ClusterIP1),
			},
			expectedClusterIPs: sets.New[string](service1ClusterIP1, service1ClusterIP2),
			udnEnabledServices: []string{service1NsName},
		},
		{
			name:               "add services before and after runtime",
			initialServices:    []runtime.Object{getService(service1Namespace, service1Name, service1ClusterIP1, service1ClusterIP2)},
			afterRunServices:   []*corev1.Service{getService(service2Namespace, service2Name, service2ClusterIP1)},
			expectedClusterIPs: sets.New[string](service1ClusterIP1, service1ClusterIP2, service2ClusterIP1),
			udnEnabledServices: []string{service1NsName, service2NsName},
		},
		{
			name:               "add services at runtime",
			initialServices:    []runtime.Object{},
			afterRunServices:   []*corev1.Service{getService(service2Namespace, service2Name, service2ClusterIP1)},
			expectedClusterIPs: sets.New[string](service2ClusterIP1),
			udnEnabledServices: []string{service1NsName, service2NsName},
		},
		{
			name:               "update service at runtime",
			initialServices:    []runtime.Object{getService(service1Namespace, service1Name, service1ClusterIP1)},
			afterRunServices:   []*corev1.Service{getService(service1Namespace, service1Name, service1ClusterIP1, service1ClusterIP2)},
			expectedClusterIPs: sets.New[string](service1ClusterIP1, service1ClusterIP2),
			udnEnabledServices: []string{service1NsName, service2NsName},
		},
		{
			name:               "cleans up stale entries",
			initialClusterIPs:  []string{service2ClusterIP1}, // service doesn't exist for this VIP
			initialServices:    []runtime.Object{getService(service1Namespace, service1Name, service1ClusterIP1, service1ClusterIP2)},
			expectedClusterIPs: sets.New[string](service1ClusterIP1, service1ClusterIP2),
			udnEnabledServices: []string{service1NsName, service2NsName},
		},
		{
			name:               "removes clusterIP",
			initialServices:    []runtime.Object{getService(service1Namespace, service1Name, service1ClusterIP1)},
			afterRunServices:   []*corev1.Service{getService(service1Namespace, service1Name)},
			expectedClusterIPs: sets.New[string](),
			udnEnabledServices: []string{service1NsName, service2NsName},
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d: %s", i, tt.name), func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			// setup fake NB DB
			var initialDB []libovsdbtest.TestData
			if len(tt.initialClusterIPs) > 0 {
				v4AS, v6AS, err := getAddressSets(tt.initialClusterIPs)
				if err != nil {
					t.Fatalf("failed to create NB DB address sets: %v", err)
				}
				initialDB = []libovsdbtest.TestData{
					v4AS,
					v6AS,
				}
			}
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: initialDB}, nil)
			if err != nil {
				t.Fatalf("failed to create new NB test harness: %v", err)
			}
			defer cleanup.Cleanup()
			asf := addressset.NewOvnAddressSetFactory(nbClient, true, true)
			t.Logf("adding services to kapi before controller is executed")
			ovnClient := util.GetOVNClientset(tt.initialServices...).GetOVNKubeControllerClientset()
			factoryMock, err := factory.NewOVNKubeControllerWatchFactory(ovnClient)
			if err != nil {
				t.Fatalf("failed to create new OVN kube controller watch factory: %v", err)
			}
			if err = factoryMock.Start(); err != nil {
				t.Fatalf("failed to start watch factory: %v", err)
			}
			c := NewController(nbClient, asf, factoryMock.ServiceCoreInformer(), tt.udnEnabledServices)
			stopCh := make(chan struct{})
			wg := &sync.WaitGroup{}
			// start running the controller
			t.Logf("starting the controller")
			wg.Add(1)
			go func() {
				if err := c.Run(stopCh); err != nil {
					t.Logf("Run() controller failed: %v", err)
				}
				wg.Done()
			}()
			defer func() {
				close(stopCh)
				wg.Wait()
			}()

			// block until address set is created
			g.Eventually(c.IsAddressSetAvailable).Should(gomega.BeTrue())
			// create the services post run. If service already exists, update it.
			t.Logf("add services to kapi at runtime")
			if err = createOrUpdateServices(ovnClient, tt.afterRunServices); err != nil {
				t.Fatalf("failed to create or update Kubernetes services: %v", err)
			}
			t.Logf("ensure address set has the correct set of clusterIPs")
			// ensure expected clusterIPs are present within the OVN address set
			asName1, asName2 := c.getAddressSetHashNames()
			g.Eventually(func() error {
				clusterIPs, err := getAllAddressesFromAddressSets(nbClient, asName1, asName2)
				if err != nil {
					t.Fatalf("failed to get all address set addresses: %v", err)
				}
				if !tt.expectedClusterIPs.Equal(clusterIPs) {
					return fmt.Errorf("expected: %v, actual: %v, diff: %v", tt.expectedClusterIPs.UnsortedList(),
						clusterIPs.UnsortedList(), tt.expectedClusterIPs.Difference(clusterIPs))
				}
				return nil
			}).WithTimeout(10 * time.Second).Should(gomega.Succeed())
			t.Logf("delete all services and ensure address map is empty")
			for ns, name := range map[string]string{service1Namespace: service1Name, service2Namespace: service2Name} {
				err = ovnClient.KubeClient.CoreV1().Services(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
				if err != nil && !errors.IsNotFound(err) {
					t.Fatalf("failed to ensure service %s/%s is deleted: %v", ns, name, err)
				}
			}
			t.Logf("ensure address set is empty")
			g.Eventually(func() int {
				clusterIPs, err := getAllAddressesFromAddressSets(nbClient, asName1, asName2)
				if err != nil {
					t.Fatalf("failed to get all address set addresses: %v", err)
				}
				return clusterIPs.Len()
			}).WithTimeout(10 * time.Second).Should(gomega.BeZero())
		})
	}
}

func getAllAddressesFromAddressSets(nbClient client.Client, asNames ...string) (sets.Set[string], error) {
	addressSets, err := libovsdbops.FindAddressSetsWithPredicate(nbClient, func(set *nbdb.AddressSet) bool {
		for _, asName := range asNames {
			if asName == set.Name {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get address sets from NB DB: %v", err)
	}
	// compare addresses found with expected address - success is when they are equal
	asClusterIPs := sets.New[string]()
	for _, addressSet := range addressSets {
		asClusterIPs.Insert(addressSet.Addresses...)
	}
	return asClusterIPs, nil
}

func createOrUpdateServices(client *util.OVNKubeControllerClientset, services []*corev1.Service) error {
	// create the services post run. If service already exists, update it.
	for _, service := range services {
		_, err := client.KubeClient.CoreV1().Services(service.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				_, err = client.KubeClient.CoreV1().Services(service.Namespace).Create(context.Background(), service, metav1.CreateOptions{})
				if err != nil {
					return fmt.Errorf("failed to create service %s/%s: %v", service.Namespace, service.Name, err)
				}
			} else {
				return fmt.Errorf("failed to check if service is already created: %v", err)
			}
		} else {
			_, err = client.KubeClient.CoreV1().Services(service.Namespace).Update(context.Background(), service, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update service %s/%s: %v", service.Namespace, service.Name, err)
			}
		}
	}
	return nil
}

func getService(namespace, name string, clusterIPs ...string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.ServiceSpec{
			ClusterIPs: clusterIPs,
		},
	}
}

func getAddressSets(addresses []string) (*nbdb.AddressSet, *nbdb.AddressSet, error) {
	v4Addresses, err := util.MatchAllIPStringFamily(false, addresses)
	if err != nil {
		if err != util.ErrorNoIP {
			return nil, nil, err
		}
		v4Addresses = make([]string, 0)
	}
	v6Addresses, err := util.MatchAllIPStringFamily(true, addresses)
	if err != nil {
		if err != util.ErrorNoIP {
			return nil, nil, err
		}
		v6Addresses = make([]string, 0)
	}

	v4DBIDs := GetAddressSetDBIDs()
	v4DBIDs = v4DBIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.IPFamilyKey: "v4"})

	v4AS := &nbdb.AddressSet{
		UUID:        v4DBIDs.String(),
		Addresses:   v4Addresses,
		ExternalIDs: v4DBIDs.GetExternalIDs(),
		Name:        v4DBIDs.String(),
	}

	v6DBIDs := GetAddressSetDBIDs()
	v6DBIDs = v6DBIDs.AddIDs(map[libovsdbops.ExternalIDKey]string{libovsdbops.IPFamilyKey: "v6"})

	v6AS := &nbdb.AddressSet{
		UUID:        v6DBIDs.String(),
		Addresses:   v6Addresses,
		ExternalIDs: v6DBIDs.GetExternalIDs(),
		Name:        v6DBIDs.String(),
	}
	return v4AS, v6AS, nil
}
