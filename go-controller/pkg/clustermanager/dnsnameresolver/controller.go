package dnsnameresolver

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net"
	"reflect"
	"strings"
	"sync"

	"github.com/miekg/dns"
	ocpnetworkapiv1alpha1 "github.com/openshift/api/network/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressfirewall "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/retry"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	hashutil "k8s.io/kubernetes/pkg/util/hash"
)

// Controller holds the information of the DNS names and the corresponding
// DNSNameResolver objects. The controller maintains the DNSNameResolver
// objects in the cluster.
type Controller struct {
	lock         sync.Mutex
	kube         *kube.KubeOVN
	stopChan     chan struct{}
	wg           *sync.WaitGroup
	watchFactory *factory.WatchFactory

	// retry framework for egress firewall
	retryEgressFirewalls *retry.RetryFramework
	// retry framework for dns name resolver
	retryDNSNameResolvers *retry.RetryFramework
	// egressFirewallHandler events factory handler
	egressFirewallHandler *factory.Handler
	// dnsNameResolver events factory handler
	dnsNameResolverHandler *factory.Handler
	// maps to hold the details of the DNS names and the corresponding
	// DNSNameResolver objects.
	dnsNameObj map[string]resolverDetails
	objDNSName sets.Set[string]

	// map to hold the current DNS names used in a namespace corresponding
	// to an EgressFirewall.
	efNamespaceDNSNames map[string]sets.Set[string]
}

// resolverDetails holds the information regarding the DNSNameResolver
// object corresponding to a DNS name.
type resolverDetails struct {
	// objName gives the name of the DNSNameResolver object corresponding
	// to the DNS name.
	objName string
	// namespaces stores those namespaces where the DNS name is being used.
	namespaces sets.Set[string]
	// collisionCount is used during the creation of the DNSNameResolver
	// object to ensure that its name does not have a collision with any
	// existing DNSNameResolver objects.
	collisionCount int
}

// NewController returns an instance of the Controller. The retry framework is also
// initialized before returning the Controller intance.
func NewController(ovnClient *util.OVNClusterManagerClientset, wf *factory.WatchFactory) *Controller {
	wg := &sync.WaitGroup{}
	c := &Controller{
		kube: &kube.KubeOVN{
			NetworkClient: ovnClient.NetworkClient,
		},
		stopChan:            make(chan struct{}),
		wg:                  wg,
		watchFactory:        wf,
		dnsNameObj:          make(map[string]resolverDetails),
		objDNSName:          sets.New[string](),
		efNamespaceDNSNames: make(map[string]sets.Set[string]),
	}
	c.initRetryFramework()
	return c
}

// initRetryFramework initializes the retry frameworks for the different resource
// types related to DNSNameResolver.
func (c *Controller) initRetryFramework() {
	c.retryEgressFirewalls = c.newRetryFramework(factory.EgressFirewallType)
	c.retryDNSNameResolvers = c.newRetryFramework(factory.DNSNameResolverType)
}

// newRetryFramework builds and returns a retry framework for the input resource
// type and assigns the resource handler containing the event handler in the
// returned struct which will be used by the retry logic in the retry package
// when WatchResource() is called.
func (c *Controller) newRetryFramework(
	objectType reflect.Type) *retry.RetryFramework {
	eventHandler := &EventHandler{
		objType:      objectType,
		watchFactory: c.watchFactory,
		c:            c,
	}
	resourceHandler := &retry.ResourceHandler{
		HasUpdateFunc:          true,
		NeedsUpdateDuringRetry: true,
		ObjType:                objectType,
		EventHandler:           eventHandler,
	}
	r := retry.NewRetryFramework(
		c.stopChan,
		c.wg,
		c.watchFactory,
		resourceHandler,
	)
	return r
}

// Start initializes the handlers for EgressFirewall and DNSNameResolver
// by watching the corresponding resource types.
func (c *Controller) Start() error {
	var err error
	if c.egressFirewallHandler, err = c.WatchEgressFirewalls(); err != nil {
		return fmt.Errorf("unable to watch egress firewalls %w", err)
	}
	if c.dnsNameResolverHandler, err = c.WatchDNSNameResolvers(); err != nil {
		return fmt.Errorf("unable to watch dns name resolvers %w", err)
	}
	return nil
}

// WatchEgressFirewalls starts the watching of egress firewall
// resource and calls back the appropriate handler logic.
func (c *Controller) WatchEgressFirewalls() (*factory.Handler, error) {
	return c.retryEgressFirewalls.WatchResource()
}

// WatchDNSNameResolvers starts the watching of dns name resolver
// resource in config.Kubernetes.OVNConfigNamespace namespaces and
// calls back the appropriate handler logic.
func (c *Controller) WatchDNSNameResolvers() (*factory.Handler, error) {
	return c.retryDNSNameResolvers.WatchResourceFiltered(config.Kubernetes.OVNConfigNamespace, nil)
}

// Stop gracefully stops the controller. The handlers for EgressFirewall
// and DNSNameResolver are removed.
func (c *Controller) Stop() {
	close(c.stopChan)
	c.wg.Wait()
	if c.egressFirewallHandler != nil {
		c.watchFactory.RemoveEgressFirewallHandler(c.egressFirewallHandler)
	}
	if c.dnsNameResolverHandler != nil {
		c.watchFactory.RemoveDNSNameResolverHandler(c.dnsNameResolverHandler)
	}
}

// syncDNSNames syncs the existing EgressFirewall and DNSNameResolver objects
// after a restart and updates the dnsNameObj and objDNSName maps. After the
// sync these two maps will only contain the details of the DNS names which
// are used in at least one namespace.
func (c *Controller) syncDNSNames(dnsNameResolvers []interface{}) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Iterate through the existing DNSNameResolver objects and add the
	// corresponding details to the dnsNameObj and objDNSName maps.
	for _, dnsNameResolverInterface := range dnsNameResolvers {
		dnsNameResolverObj, ok := dnsNameResolverInterface.(*ocpnetworkapiv1alpha1.DNSNameResolver)
		if !ok {
			klog.Errorf("Could not cast %T object to *ocpnetworkapiv1alpha1.DNSNameResolver", dnsNameResolverObj)
			continue
		}
		c.dnsNameObj[string(dnsNameResolverObj.Spec.Name)] = resolverDetails{
			objName:    dnsNameResolverObj.Name,
			namespaces: sets.New[string](),
		}
		c.objDNSName.Insert(dnsNameResolverObj.Name)
	}

	// Fetch the existing EgressFirewall objects.
	egressFirewalls, err := c.watchFactory.GetEgressFirewalls()
	if err != nil {
		return fmt.Errorf("syncDNSNames unable to get Egress Firewalls: %w", err)
	}

	// Iterate through the existing EgressFirewall objects. For each
	// EgressFirewall object iterate through the DNS names in it and
	// add the namespace of the EgressFirewall to the list of
	// namespaces of each DNS name. Also add the DNS names to the set
	// of DNS names mapped to each namespace.
	for _, egressFirewall := range egressFirewalls {
		existingDNSNames := sets.New[string]()
		getDNSNames(egressFirewall, existingDNSNames, c.watchFactory)
		dnsNames, exists := c.efNamespaceDNSNames[egressFirewall.Namespace]
		if !exists {
			dnsNames = sets.New[string]()
		}
		for dnsName := range existingDNSNames {
			if objDetails, exists := c.dnsNameObj[dnsName]; exists {
				objDetails.namespaces.Insert(egressFirewall.Namespace)
				c.dnsNameObj[dnsName] = objDetails
				dnsNames.Insert(dnsName)
			}
		}
		c.efNamespaceDNSNames[egressFirewall.Namespace] = dnsNames
	}

	// Delete the details of the DNS names from the dnsNameObj and
	// the objDNSName if the DNS name is not used in any namespace.
	for dnsName, objDetails := range c.dnsNameObj {
		if objDetails.namespaces.Len() == 0 {
			c.objDNSName.Delete(objDetails.objName)
			delete(c.dnsNameObj, dnsName)
		}
	}

	return nil
}

// egressFirewallChanged reconciles an EgressFirewall object. Based on the
// create/update/delete event, newly added and deleted DNS names are
// obtained. For the newly added DNS names, the addDNSName function is
// called and for the deleted DNS names, the deleteDNSName function is
// called.
func (c *Controller) egressFirewallChanged(oldEf, newEf *egressfirewall.EgressFirewall) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	oldDNSNames := sets.New[string]()
	newDNSNames := sets.New[string]()

	// For the update or delete event, get the DNS names which are used in
	// old EgressFirewall object.
	if oldEf != nil {
		getDNSNames(oldEf, oldDNSNames, c.watchFactory)
	}
	// For the update or create event, get the DNS names which are used in
	// new EgressFirewall object.
	if newEf != nil {
		// If it is an add event, add all the existing DNS names corresponding
		// to the EgressFirewall object's namespace, if any, to oldDNSNames.
		if oldEf == nil {
			if dnsNames, exists := c.efNamespaceDNSNames[newEf.Namespace]; exists {
				oldDNSNames.Insert(dnsNames.UnsortedList()...)
			}
		}
		getDNSNames(newEf, newDNSNames, c.watchFactory)
	}

	// Get the newly added DNS names.
	addedDNSNames := newDNSNames.Difference(oldDNSNames)
	// Get the deleted DNS names.
	deletedDNSNames := oldDNSNames.Difference(newDNSNames)

	var errorList []error
	// Iterated through each newly added DNS name and call the addDNSName.
	for addedDNSName := range addedDNSNames {
		if err := c.addDNSName(addedDNSName, newEf.Namespace); err != nil {
			errorList = append(errorList, err)
		}
	}

	// Iterated through each newly added DNS name and call the deleteDNSName.
	for deletedDNSName := range deletedDNSNames {
		if err := c.deleteDNSName(deletedDNSName, oldEf.Namespace); err != nil {
			errorList = append(errorList, err)
		}
	}

	return errors.NewAggregate(errorList)
}

// dnsNameResolverChanged reconciles a DNSNameResolver object. If an object
// was deleted, but it was not supposed to, then it is recreated. If an
// object is created, but it was not supposed to, then it is deleted.
func (c *Controller) dnsNameResolverChanged(oldDnr, newDnr *ocpnetworkapiv1alpha1.DNSNameResolver) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// DNSNameResolver object was deleted. If it is not supposed to be deleted,
	// recreate it with the existing status.
	if oldDnr != nil && newDnr == nil {
		dnsName := string(oldDnr.Spec.Name)
		if objDetails, exists := c.dnsNameObj[dnsName]; exists && objDetails.objName == oldDnr.Name {
			// Check if the DNS name is used in any of the namespaces. If not,
			// then remove the details of the DNS name and the object.
			if objDetails.namespaces.Len() == 0 {
				delete(c.dnsNameObj, dnsName)
				c.objDNSName.Delete(objDetails.objName)
				return nil
			}

			// Recreate the DNSNameResolver object.
			klog.Warningf("Recreating deleted dns name resolver object %s for dns name %s", oldDnr.Name, dnsName)
			return c.createDNSNameResolver(oldDnr.Name, dnsName, oldDnr.Status)
		}
	}

	// DNSNameResolver object was added/updated. If it is not supposed to exist,
	// delete the object.
	if newDnr != nil {
		dnsName := string(newDnr.Spec.Name)
		if objDetails, exists := c.dnsNameObj[dnsName]; !exists || objDetails.objName != newDnr.Name {
			// Delete the DNSNameResolver object.
			klog.Warningf("Deleting additional dns name resolver object %s for dns name %s", newDnr.Name, dnsName)
			return c.deleteDNSNameResolver(newDnr.Name)
		}
	}

	return nil
}

// addDNSName creates the corresponding DNSNameResolver object for the DNS name
// if not already created. The namespace is added to the list of namespaces where
// the DNS name is used.
func (c *Controller) addDNSName(dnsName, namespace string) error {
	objDetails, exists := c.dnsNameObj[dnsName]
	// Create the DNSNameResolver object if its details are not available.
	if !exists || objDetails.objName == "" {
		// The collisionCount variable for the DNS name is incremented a maximum
		// of 10 times for generating a name for the corresponding dns name
		// resolver object without any collision.
		maxRetry := objDetails.collisionCount + 10
		for ; objDetails.collisionCount < maxRetry; objDetails.collisionCount++ {
			objDetails.objName = "dns-" + computeHash(dnsName, objDetails.collisionCount)
			// Check if the generated object name matches with any of the existing
			// object's name.
			if _, found := c.objDNSName[objDetails.objName]; !found {
				break
			}
		}
		// If the maximum retry is reached, then return an error so that it can be
		// retried again via the retry framework.
		if objDetails.collisionCount == maxRetry {
			objDetails.objName = ""
			c.dnsNameObj[dnsName] = objDetails
			return fmt.Errorf("encountered collision in name while creating dns name resolver object for %s", dnsName)
		}
		if err := c.createDNSNameResolver(objDetails.objName, dnsName, ocpnetworkapiv1alpha1.DNSNameResolverStatus{}); err != nil {
			return err
		}
		if objDetails.namespaces == nil {
			objDetails.namespaces = sets.New[string]()
		}
	}

	// Add the DNS name to the set of DNS names belonging
	// to the namespace.
	dnsNames, exists := c.efNamespaceDNSNames[namespace]
	if !exists {
		dnsNames = sets.New[string]()
	}
	dnsNames.Insert(dnsName)
	c.efNamespaceDNSNames[namespace] = dnsNames

	objDetails.namespaces.Insert(namespace)

	c.dnsNameObj[dnsName] = objDetails
	c.objDNSName.Insert(objDetails.objName)

	return nil
}

// deleteDNSName removes the namespace from the list of namespaces the
// DNS name is currently used. If the DNS name, is not used in any other
// namespace, then the corresponding DNSNameResolver object is deleted
// and the details of the DNS name is removed.
func (c *Controller) deleteDNSName(dnsName, namespace string) error {
	objDetails, exists := c.dnsNameObj[dnsName]
	if exists {
		delete(objDetails.namespaces, namespace)
		if objDetails.namespaces.Len() == 0 {
			err := c.deleteDNSNameResolver(objDetails.objName)
			if err != nil {
				// Insert back the namespace when an error is encountered
				// while deleting the DNSNameResolver object.
				objDetails.namespaces.Insert(namespace)
				return err
			}
			delete(c.dnsNameObj, dnsName)
			c.objDNSName.Delete(objDetails.objName)
		}
	}

	// Remove the DNS name from the set of DNS names belonging
	// to the namespace. If the set is empty, then delete the
	// mapping to the namespace.
	dnsNames, exists := c.efNamespaceDNSNames[namespace]
	if exists {
		dnsNames.Delete(dnsName)
		if dnsNames.Len() == 0 {
			delete(c.efNamespaceDNSNames, namespace)
		} else {
			c.efNamespaceDNSNames[namespace] = dnsNames
		}
	}

	c.dnsNameObj[dnsName] = objDetails
	c.objDNSName.Insert(objDetails.objName)

	return nil
}

// createDNSNameResolver creates a DNSNameResolver object for the DNS name
// and adds the status, if any, to the object. The error, if any, encountered
// during the object creation is returned.
func (c *Controller) createDNSNameResolver(objName, dnsName string, status ocpnetworkapiv1alpha1.DNSNameResolverStatus) error {
	dnsNameResolverObj := &ocpnetworkapiv1alpha1.DNSNameResolver{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objName,
			Namespace: config.Kubernetes.OVNConfigNamespace,
		},
		Spec: ocpnetworkapiv1alpha1.DNSNameResolverSpec{
			Name: ocpnetworkapiv1alpha1.DNSName(dnsName),
		},
		Status: status,
	}
	_, err := c.kube.CreateDNSNameResolver(dnsNameResolverObj, config.Kubernetes.OVNConfigNamespace)

	return err

}

// deleteDNSNameResolver deletes a DNSNameResolver object and if an error
// is encountered, which is not IsNotFound, then it is returned.
func (c *Controller) deleteDNSNameResolver(objName string) error {
	err := c.kube.DeleteDNSNameResolver(objName, config.Kubernetes.OVNConfigNamespace)
	if err != nil && !kerrors.IsNotFound(err) {
		return err
	}
	return nil
}

// getDNSNames iterates through the egress firewall rules and returns the DNS
// names present in them after validating the rules.
func getDNSNames(ef *egressfirewall.EgressFirewall, dnsNames sets.Set[string], wf *factory.WatchFactory) {
	var dnsNameSlice []string
	for i, egressFirewallRule := range ef.Spec.Egress {
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			klog.Warningf("egressFirewall for namespace %s has too many rules, the rest will be ignored", ef.Namespace)
			break
		}

		// Validate the egress firewall rules.
		if len(egressFirewallRule.To.CIDRSelector) > 0 {
			// Validate CIDR selector.
			_, _, err := net.ParseCIDR(egressFirewallRule.To.CIDRSelector)
			if err != nil {
				return
			}

		} else if egressFirewallRule.To.NodeSelector != nil {
			// Validate node selector.
			_, err := metav1.LabelSelectorAsSelector(egressFirewallRule.To.NodeSelector)
			if err != nil {
				return
			}
			_, err = wf.GetNodesByLabelSelector(*egressFirewallRule.To.NodeSelector)
			if err != nil {
				return
			}
		} else {
			dnsName := strings.ToLower(dns.Fqdn(egressFirewallRule.To.DNSName))
			dnsNameSlice = append(dnsNameSlice, dnsName)
		}
	}

	dnsNames.Insert(dnsNameSlice...)
}

// computeHash returns a hash value calculated from dns name and a collisionCount
// to avoid hash collision. The hash will be safe encoded to avoid bad words.
func computeHash(dnsName string, collisionCount int) string {
	dnsNameHasher := fnv.New32a()
	hashutil.DeepHashObject(dnsNameHasher, dnsName)

	// Add collisionCount in the hash if it is not zero.
	if collisionCount != 0 {
		collisionCountBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(collisionCountBytes, uint32(collisionCount))
		dnsNameHasher.Write(collisionCountBytes)
	}
	return rand.SafeEncodeString(fmt.Sprint(dnsNameHasher.Sum32()))
}
