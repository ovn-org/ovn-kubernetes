package services

import (
	"fmt"
	"regexp"
	"strings"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdbops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
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

// len returns the number of chasis on which this Template variable is
// instantiated (has a value).
func (t *Template) len() int {
	return len(t.Value)
}

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

// makeLBNodeIPTemplateName creates a template name for the node IP (per family)
func makeLBNodeIPTemplateName(family corev1.IPFamily) string {
	return fmt.Sprintf("%s_%v", LBVipNodeTemplate, family)
}

// isLBNodeIPTemplateName returns true if 'name' is the node IP template name
// for any IP family.
func isLBNodeIPTemplateName(name string) bool {
	return name == makeLBNodeIPTemplateName(corev1.IPv4Protocol) ||
		name == makeLBNodeIPTemplateName(corev1.IPv6Protocol)
}

// makeLBTargetTemplateName builds a load balancer target template name.
func makeLBTargetTemplateName(service *corev1.Service, proto corev1.Protocol, port int32,
	family corev1.IPFamily, scope string) string {
	return makeTemplateName(
		makeLBName(service, proto,
			fmt.Sprintf("%d_%s_%v", port, scope, family)))
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
