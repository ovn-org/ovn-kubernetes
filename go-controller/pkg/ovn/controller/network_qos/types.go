package networkqos

import (
	"sync"

	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	knet "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// networkQoSState is the cache that keeps the state of a single
// network qos in the cluster with namespace+name being unique
type networkQoSState struct {
	// name of the network qos
	name      string
	namespace string

	networkAttachmentName string

	SrcAddrSet  addressset.AddressSet
	Pods        *sync.Map // pods name -> ips in the srcAddrSet
	PodSelector metav1.LabelSelector

	// egressRules stores the objects needed to track .Spec.Egress changes
	EgressRules []*GressRule
}

type GressRule struct {
	Priority   int
	Dscp       int
	Classifier *Classifier

	// bandwitdh
	Rate  *int
	Burst *int
}

type Classifier struct {
	Destinations []*Destination

	// port
	Protocol *string
	Port     *int
}

type Destination struct {
	IpBlock *knet.IPBlock

	DestAddrSet       addressset.AddressSet
	Pods              *sync.Map // pods name -> ips in the destAddrSet
	NamespaceSelector metav1.LabelSelector
	PodSelector       metav1.LabelSelector
}
