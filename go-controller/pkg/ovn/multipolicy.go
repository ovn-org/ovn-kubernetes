package ovn

import (
	"fmt"
	"strings"

	multinetworkpolicy "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	knet "k8s.io/api/networking/v1"
	"k8s.io/klog/v2"
)

const PolicyForAnnotation = "k8s.v1.cni.cncf.io/policy-for"

func (oc *Controller) syncMultiNetworkPolicies(multiPolicies []interface{}) {
	oc.syncWithRetry("syncMultiNetworkPolicies", func() error { return oc.syncMultiNetworkPoliciesRetriable(multiPolicies) })
}

func (oc *Controller) syncMultiNetworkPoliciesRetriable(multiPolicies []interface{}) error {
	expectedPolicies := make(map[string]map[string]bool)
	for _, npInterface := range multiPolicies {
		policy, ok := npInterface.(*multinetworkpolicy.MultiNetworkPolicy)
		if !ok {
			return fmt.Errorf("spurious object in syncMultiNetworkPolicies: %v", npInterface)
		}
		if !oc.shouldApplyMultiPolicy(policy) {
			klog.V(5).Infof("[controller(%s)] skipping syncing policy %s/%s",
				oc.nadInfo.NetName, policy.Namespace, policy.Name)
			continue
		}
		if nsMap, ok := expectedPolicies[policy.Namespace]; ok {
			nsMap[policy.Name] = true
		} else {
			expectedPolicies[policy.Namespace] = map[string]bool{
				policy.Name: true,
			}
		}
	}

	return oc.syncNetworkPoliciesRetriableCommon(expectedPolicies)
}

func (oc *Controller) shouldApplyMultiPolicy(mpolicy *multinetworkpolicy.MultiNetworkPolicy) bool {
	policyForAnnot, ok := mpolicy.Annotations[PolicyForAnnotation]
	if !ok {
		// should never happen.
		return false
	}
	policyForAnnot = strings.ReplaceAll(policyForAnnot, " ", "")
	policyForNetworks := strings.Split(policyForAnnot, ",")
	for _, networkName := range policyForNetworks {
		networkNamespace := mpolicy.Namespace
		a := strings.Split(networkName, "/")
		if len(a) > 1 {
			networkName = a[1]
			networkNamespace = a[0]
		}
		if _, ok := oc.nadInfo.NetAttachDefs.Load(util.GetNadKeyName(networkNamespace, networkName)); ok {
			return true
		}
	}
	return false
}

func convertMultiNetPolicyToNetPolicy(mpolicy *multinetworkpolicy.MultiNetworkPolicy) *knet.NetworkPolicy {
	var policy knet.NetworkPolicy
	var ipb *knet.IPBlock

	policy.Name = mpolicy.Name
	policy.Namespace = mpolicy.Namespace
	policy.Spec.PodSelector = mpolicy.Spec.PodSelector
	policy.Annotations = mpolicy.Annotations
	policy.Spec.Ingress = make([]knet.NetworkPolicyIngressRule, len(mpolicy.Spec.Ingress))
	for i, mingress := range mpolicy.Spec.Ingress {
		var ingress knet.NetworkPolicyIngressRule
		ingress.Ports = make([]knet.NetworkPolicyPort, len(mingress.Ports))
		for j, mport := range mingress.Ports {
			ingress.Ports[j] = knet.NetworkPolicyPort{
				Protocol: mport.Protocol,
				Port:     mport.Port,
			}
		}
		ingress.From = make([]knet.NetworkPolicyPeer, len(mingress.From))
		for j, mfrom := range mingress.From {
			ipb = nil
			if mfrom.IPBlock != nil {
				ipb = &knet.IPBlock{CIDR: mfrom.IPBlock.CIDR, Except: mfrom.IPBlock.Except}
			}
			ingress.From[j] = knet.NetworkPolicyPeer{
				PodSelector:       mfrom.PodSelector,
				NamespaceSelector: mfrom.NamespaceSelector,
				IPBlock:           ipb,
			}
		}
		policy.Spec.Ingress[i] = ingress
	}
	policy.Spec.Egress = make([]knet.NetworkPolicyEgressRule, len(mpolicy.Spec.Egress))
	for i, megress := range mpolicy.Spec.Egress {
		var egress knet.NetworkPolicyEgressRule
		egress.Ports = make([]knet.NetworkPolicyPort, len(megress.Ports))
		for j, mport := range megress.Ports {
			egress.Ports[j] = knet.NetworkPolicyPort{
				Protocol: mport.Protocol,
				Port:     mport.Port,
			}
		}
		egress.To = make([]knet.NetworkPolicyPeer, len(megress.To))
		for j, mto := range megress.To {
			ipb = nil
			if mto.IPBlock != nil {
				ipb = &knet.IPBlock{CIDR: mto.IPBlock.CIDR, Except: mto.IPBlock.Except}
			}
			egress.To[j] = knet.NetworkPolicyPeer{
				PodSelector:       mto.PodSelector,
				NamespaceSelector: mto.NamespaceSelector,
				IPBlock:           ipb,
			}
		}
		policy.Spec.Egress[i] = egress
	}
	policy.Spec.PolicyTypes = make([]knet.PolicyType, len(mpolicy.Spec.PolicyTypes))
	for i, mpolicytype := range mpolicy.Spec.PolicyTypes {
		policy.Spec.PolicyTypes[i] = knet.PolicyType(mpolicytype)
	}
	return &policy
}
