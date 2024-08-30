package services

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
)

const LBVipNodeTemplate string = "NODEIP"
const TemplatePrefix = "^"

// Chassis_Template_Var records store (for efficiency reasons) a 'variables'
// map for each chassis. For simpler CMS code, store templates in memory
// as a (Name, Value) tuple where Value is a map with potentially different
// values on each chassis.
type Template struct {
	// The name of the template.
	Name string

	// Per chassis template value, indexed by chassisID.
	Value map[string]string
}

type TemplateMap map[string]*Template
type ChassisTemplateVarMap map[string]*nbdb.ChassisTemplateVar

// toReferenceString returns the textual representation of a template
// reference, that is, '^<template-name>'.
func (t *Template) toReferenceString() string {
	return TemplatePrefix + t.Name
}

// isTemplateReference returns true if 'name' is a valid template reference.
func isTemplateReference(name string) bool {
	return strings.HasPrefix(name, TemplatePrefix)
}

// templateNameFromReference extracts the template name from a textual
// reference to a template.
func templateNameFromReference(template string) string {
	return strings.TrimPrefix(template, TemplatePrefix)
}

// makeTemplateName creates a valid template name by replacing invalid
// characters in the original 'name' with '_'. Existing '_' are doubled.
//
// For example:
// "Service_default/node-port-svc_UDP_30957_node_router_template_IPv4"
//
// gets mangled to:
// "Service__default_node_port_svc__UDP__30957__node__router__template__IPv4"
func makeTemplateName(name string) string {
	invalidChars := regexp.MustCompile(`[/\-$@]`)
	name = strings.Replace(name, "_", "__", -1)
	return invalidChars.ReplaceAllString(name, "_")
}

// makeTemplate intializes a named Template struct with 0 values.
func makeTemplate(name string) *Template {
	return &Template{Name: name, Value: map[string]string{}}
}

func forEachNBTemplateInMaps(templateMaps []TemplateMap, callback func(nbTemplate *nbdb.ChassisTemplateVar) bool) {
	// First flatten the maps into *nbdb.ChassisTemplateVar records:
	flattened := ChassisTemplateVarMap{}
	for _, templateMap := range templateMaps {
		for _, template := range templateMap {
			for chassisName, templateValue := range template.Value {
				nbTemplate, found := flattened[chassisName]
				if !found {
					nbTemplate = &nbdb.ChassisTemplateVar{
						Chassis:   chassisName,
						Variables: map[string]string{},
					}
					flattened[chassisName] = nbTemplate
				}
				nbTemplate.Variables[template.Name] = templateValue
			}
		}
	}

	// Now walk the flattened records:
	for _, nbTemplate := range flattened {
		if !callback(nbTemplate) {
			break
		}
	}
}

// getLoadBalancerTemplates returns the TemplateMap of templates referenced
// by the load balancer. These are pointers to Templates already read from
// the database which are stored in 'allTemplates'.
func getLoadBalancerTemplates(lb *nbdb.LoadBalancer, allTemplates TemplateMap) TemplateMap {
	result := TemplateMap{}
	for vip, backend := range lb.Vips {
		for _, s := range []string{vip, backend} {
			if isTemplateReference(s) {
				templateName := templateNameFromReference(s)
				if template, found := allTemplates[templateName]; found {
					result[template.Name] = template
				}
			}
		}
	}
	return result
}

// listSvcTemplates looks up all chassis template variables. It returns
// libovsdb.Templates values indexed by template name.
func listSvcTemplates(nbClient libovsdbclient.Client) (templatesByName TemplateMap, err error) {
	templatesByName = TemplateMap{}

	templatesList, err := libovsdbops.ListTemplateVar(nbClient)
	if err != nil {
		return
	}

	for _, nbTemplate := range templatesList {
		for name, perChassisValue := range nbTemplate.Variables {
			tv, found := templatesByName[name]
			if !found {
				tv = makeTemplate(name)
				templatesByName[name] = tv
			}
			tv.Value[nbTemplate.Chassis] = perChassisValue
		}
	}
	return
}

func svcCreateOrUpdateTemplateVarOps(nbClient libovsdbclient.Client,
	ops []libovsdb.Operation, templateVars []TemplateMap) ([]libovsdb.Operation, error) {

	var err error

	forEachNBTemplateInMaps(templateVars, func(nbTemplate *nbdb.ChassisTemplateVar) bool {
		ops, err = libovsdbops.CreateOrUpdateChassisTemplateVarOps(nbClient, ops, nbTemplate)
		return err == nil
	})
	if err != nil {
		return nil, err
	}
	return ops, nil
}

func svcDeleteTemplateVarOps(nbClient libovsdbclient.Client,
	ops []libovsdb.Operation, templateVars []TemplateMap) ([]libovsdb.Operation, error) {
	var err error

	forEachNBTemplateInMaps(templateVars, func(nbTemplate *nbdb.ChassisTemplateVar) bool {
		ops, err = libovsdbops.DeleteChassisTemplateVarVariablesOps(nbClient, ops, nbTemplate)
		return err == nil
	})
	if err != nil {
		return nil, err
	}
	return ops, nil
}

func svcCreateOrUpdateTemplateVar(nbClient libovsdbclient.Client, templateVars []TemplateMap) error {
	ops, err := svcCreateOrUpdateTemplateVarOps(nbClient, nil, templateVars)
	if err != nil {
		return err
	}

	_, err = libovsdbops.TransactAndCheck(nbClient, ops)
	return err
}

// makeLBNodeIPTemplateNamePrefix creates a template name prefix for the node IP (per family)
func makeLBNodeIPTemplateNamePrefix(family corev1.IPFamily) string {
	return fmt.Sprintf("%s_%v_", LBVipNodeTemplate, family)
}

// isLBNodeIPTemplateName returns true if 'name' is a node IP template name
// for any IP family (i.e. in the form NODEIP_IPv4_X).
func isLBNodeIPTemplateName(name string) bool {
	return strings.HasPrefix(name, makeLBNodeIPTemplateNamePrefix(corev1.IPv4Protocol)) ||
		strings.HasPrefix(name, makeLBNodeIPTemplateNamePrefix(corev1.IPv6Protocol))
}

// makeLBTargetTemplateName builds a load balancer target template name.
func makeLBTargetTemplateName(service *corev1.Service, proto corev1.Protocol, port int32,
	family corev1.IPFamily, scope string, netInfo util.NetInfo) string {
	return makeTemplateName(
		makeLBNameForNetwork(service, proto,
			fmt.Sprintf("%d_%s_%v", port, scope, family), netInfo))
}

// getTemplatesFromRulesTargets returns the map of template variables referred
// to by the targets in 'rules'.
func getTemplatesFromRulesTargets(rules []LBRule) TemplateMap {
	templates := TemplateMap{}
	for _, r := range rules {
		for _, tgt := range r.Targets {
			if tgt.Template == nil || tgt.Template.Name == "" {
				continue
			}
			templates[tgt.Template.Name] = tgt.Template
		}
		// No need to return source templates, those are managed in the
		// node-tracker.
	}
	return templates
}

// NodeIPTemplates maintains templates variables for many IP addresses per node,
// creating them in the form NODEIP_IPv4_0, NODEIP_IPv4_1, NODEIP_IPv4_2, ...
// if and when they are needed.
type NodeIPsTemplates struct {
	ipFamily  corev1.IPFamily
	templates []*Template
}

func NewNodeIPsTemplates(ipFamily corev1.IPFamily) *NodeIPsTemplates {
	return &NodeIPsTemplates{
		ipFamily:  ipFamily,
		templates: make([]*Template, 0),
	}
}

// AddIP adds a template variable for the specified chassis and ip address.
func (n *NodeIPsTemplates) AddIP(chassisID string, ip net.IP) {

	for _, template := range n.templates {
		_, ok := template.Value[chassisID]
		if !ok {
			template.Value[chassisID] = ip.String()
			return
		}
	}

	// NODEIP_IPvN_XXX is missing, creating it.
	newTemplate := makeTemplate(
		makeLBNodeIPTemplateNamePrefix(n.ipFamily) + fmt.Sprint(len(n.templates)),
	)

	// And initialize with chassisID value
	newTemplate.Value[chassisID] = ip.String()

	n.templates = append(n.templates, newTemplate)
}

func (n *NodeIPsTemplates) AsTemplateMap() TemplateMap {
	var ret TemplateMap = TemplateMap{}

	for _, t := range n.templates {
		ret[t.Name] = t
	}

	return ret
}

func (n *NodeIPsTemplates) AsTemplates() []*Template {
	return n.templates
}

func (n *NodeIPsTemplates) Len() int {
	return len(n.templates)
}
