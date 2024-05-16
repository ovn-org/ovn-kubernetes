package adminnetworkpolicy

import (
	"fmt"

	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/metrics"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/prometheus/client_golang/prometheus"
	anpapi "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// Descriptors used by the ANPControllerCollector below.
var (
	anpRuleCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metrics.MetricOvnkubeNamespace, metrics.MetricOvnkubeSubsystemController, "admin_network_policies_rules"),
		"The total number of rules across all admin network policies in the cluster",
		[]string{
			"direction", // direction is either "ingress" or "egress"; so cardinality is max 2 for this label
			"action",    // action is either "Pass" or "Allow" or "Deny"; so cardinality is max 3 for this label
		}, nil,
	)
	banpRuleCountDesc = prometheus.NewDesc(
		prometheus.BuildFQName(metrics.MetricOvnkubeNamespace, metrics.MetricOvnkubeSubsystemController, "baseline_admin_network_policies_rules"),
		"The total number of rules across all baseline admin network policies in the cluster",
		[]string{
			"direction", // direction is either "ingress" or "egress"; so cardinality is max 2 for this label
			"action",    // action is either "Allow" or "Deny"; so cardinality is max 2 for this label
		}, nil,
	)
)

func (c *Controller) setupMetricsCollector() {
	prometheus.MustRegister(c)
}

func (c *Controller) teardownMetricsCollector() {
	prometheus.Unregister(c)
}

// Describe is implemented with DescribeByCollect. That's possible because the
// Collect method will always return the same two metrics with the same two
// descriptors.
func (c *Controller) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

func (c *Controller) fetchANPRuleCountMetric(isBanp bool) map[string]map[string]int {
	c.RLock()
	defer c.RUnlock()
	ruleCount := map[string]map[string]int{
		string(libovsdbutil.ACLIngress): {
			string(anpapi.AdminNetworkPolicyRuleActionAllow): 0,
			string(anpapi.AdminNetworkPolicyRuleActionDeny):  0,
		},
		string(libovsdbutil.ACLEgress): {
			string(anpapi.AdminNetworkPolicyRuleActionAllow): 0,
			string(anpapi.AdminNetworkPolicyRuleActionDeny):  0,
		},
	}
	if !isBanp {
		// its ANP; so add Pass label and use the anpCache
		ruleCount[string(libovsdbutil.ACLIngress)][string(anpapi.AdminNetworkPolicyRuleActionPass)] = 0
		ruleCount[string(libovsdbutil.ACLEgress)][string(anpapi.AdminNetworkPolicyRuleActionPass)] = 0
		for _, state := range c.anpCache {
			allow, pass, deny := c.countRules(state.ingressRules)
			ruleCount[string(libovsdbutil.ACLIngress)][string(anpapi.AdminNetworkPolicyRuleActionAllow)] += allow
			ruleCount[string(libovsdbutil.ACLIngress)][string(anpapi.AdminNetworkPolicyRuleActionDeny)] += deny
			ruleCount[string(libovsdbutil.ACLIngress)][string(anpapi.AdminNetworkPolicyRuleActionPass)] += pass
			allow, pass, deny = c.countRules(state.egressRules)
			ruleCount[string(libovsdbutil.ACLEgress)][string(anpapi.AdminNetworkPolicyRuleActionAllow)] += allow
			ruleCount[string(libovsdbutil.ACLEgress)][string(anpapi.AdminNetworkPolicyRuleActionDeny)] += deny
			ruleCount[string(libovsdbutil.ACLEgress)][string(anpapi.AdminNetworkPolicyRuleActionPass)] += pass
		}
	} else {
		// its BANP; singleton use the banpCache
		state := c.banpCache
		allow, _, deny := c.countRules(state.ingressRules)
		ruleCount[string(libovsdbutil.ACLIngress)][string(anpapi.AdminNetworkPolicyRuleActionAllow)] += allow
		ruleCount[string(libovsdbutil.ACLIngress)][string(anpapi.AdminNetworkPolicyRuleActionDeny)] += deny
		allow, _, deny = c.countRules(state.egressRules)
		ruleCount[string(libovsdbutil.ACLEgress)][string(anpapi.AdminNetworkPolicyRuleActionAllow)] += allow
		ruleCount[string(libovsdbutil.ACLEgress)][string(anpapi.AdminNetworkPolicyRuleActionDeny)] += deny
	}

	return ruleCount
}

func (c *Controller) countRules(rules []*gressRule) (int, int, int) {
	var passCount, allowCount, denyCount int
	for _, rule := range rules {
		switch rule.action {
		case nbdb.ACLActionAllowRelated:
			allowCount++
		case nbdb.ACLActionDrop:
			denyCount++
		case nbdb.ACLActionPass:
			passCount++
		default:
			panic(fmt.Sprintf("Failed to count rule type: unknown acl action %s", rule.action))
		}
	}
	return allowCount, passCount, denyCount
}

// Collect first triggers the fetchANPRuleCountMetric. Then it
// creates constant metrics for each host on the fly based on the returned data.
//
// Note that Collect could be called concurrently, so we depend on
// fetchANPRuleCountMetric to be concurrency-safe.
func (c *Controller) Collect(ch chan<- prometheus.Metric) {
	aRuleCount := c.fetchANPRuleCountMetric(false)
	for direction, actions := range aRuleCount {
		for action, count := range actions {
			ch <- prometheus.MustNewConstMetric(
				anpRuleCountDesc,
				prometheus.GaugeValue,
				float64(count),
				direction,
				action,
			)
		}
	}
	bRuleCount := c.fetchANPRuleCountMetric(true)
	for direction, actions := range bRuleCount {
		for action, count := range actions {
			ch <- prometheus.MustNewConstMetric(
				banpRuleCountDesc,
				prometheus.GaugeValue,
				float64(count),
				direction,
				action,
			)
		}
	}
}
