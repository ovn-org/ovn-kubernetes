package ops

import (
	"fmt"
	"reflect"

	"github.com/ovn-org/libovsdb/client"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func getUUID(model model.Model) string {
	switch t := model.(type) {
	case *nbdb.ACL:
		return t.UUID
	case *nbdb.AddressSet:
		return t.UUID
	case *nbdb.BFD:
		return t.UUID
	case *nbdb.Copp:
		return t.UUID
	case *nbdb.GatewayChassis:
		return t.UUID
	case *nbdb.LoadBalancer:
		return t.UUID
	case *nbdb.LoadBalancerGroup:
		return t.UUID
	case *nbdb.LogicalRouter:
		return t.UUID
	case *nbdb.LogicalRouterPolicy:
		return t.UUID
	case *nbdb.LogicalRouterPort:
		return t.UUID
	case *nbdb.LogicalRouterStaticRoute:
		return t.UUID
	case *nbdb.LogicalSwitch:
		return t.UUID
	case *nbdb.LogicalSwitchPort:
		return t.UUID
	case *nbdb.NAT:
		return t.UUID
	case *nbdb.PortGroup:
		return t.UUID
	case *nbdb.NBGlobal:
		return t.UUID
	case *nbdb.MeterBand:
		return t.UUID
	case *nbdb.Meter:
		return t.UUID
	case *nbdb.Sample:
		return t.UUID
	case *nbdb.SampleCollector:
		return t.UUID
	case *nbdb.SamplingApp:
		return t.UUID
	case *nbdb.StaticMACBinding:
		return t.UUID
	case *sbdb.Chassis:
		return t.UUID
	case *sbdb.ChassisPrivate:
		return t.UUID
	case *sbdb.IGMPGroup:
		return t.UUID
	case *sbdb.Encap:
		return t.UUID
	case *sbdb.PortBinding:
		return t.UUID
	case *sbdb.SBGlobal:
		return t.UUID
	case *nbdb.QoS:
		return t.UUID
	case *nbdb.ChassisTemplateVar:
		return t.UUID
	case *nbdb.DHCPOptions:
		return t.UUID
	default:
		panic(fmt.Sprintf("getUUID: unknown model %T", t))
	}
}

func setUUID(model model.Model, uuid string) {
	switch t := model.(type) {
	case *nbdb.ACL:
		t.UUID = uuid
	case *nbdb.AddressSet:
		t.UUID = uuid
	case *nbdb.BFD:
		t.UUID = uuid
	case *nbdb.Copp:
		t.UUID = uuid
	case *nbdb.GatewayChassis:
		t.UUID = uuid
	case *nbdb.LoadBalancer:
		t.UUID = uuid
	case *nbdb.LoadBalancerGroup:
		t.UUID = uuid
	case *nbdb.LogicalRouter:
		t.UUID = uuid
	case *nbdb.LogicalRouterPolicy:
		t.UUID = uuid
	case *nbdb.LogicalRouterPort:
		t.UUID = uuid
	case *nbdb.LogicalRouterStaticRoute:
		t.UUID = uuid
	case *nbdb.LogicalSwitch:
		t.UUID = uuid
	case *nbdb.LogicalSwitchPort:
		t.UUID = uuid
	case *nbdb.NAT:
		t.UUID = uuid
	case *nbdb.PortGroup:
		t.UUID = uuid
	case *nbdb.NBGlobal:
		t.UUID = uuid
	case *nbdb.MeterBand:
		t.UUID = uuid
	case *nbdb.Meter:
		t.UUID = uuid
	case *nbdb.Sample:
		t.UUID = uuid
	case *nbdb.SampleCollector:
		t.UUID = uuid
	case *nbdb.SamplingApp:
		t.UUID = uuid
	case *nbdb.StaticMACBinding:
		t.UUID = uuid
	case *sbdb.Chassis:
		t.UUID = uuid
	case *sbdb.ChassisPrivate:
		t.UUID = uuid
	case *sbdb.IGMPGroup:
		t.UUID = uuid
	case *sbdb.Encap:
		t.UUID = uuid
	case *sbdb.PortBinding:
		t.UUID = uuid
	case *sbdb.SBGlobal:
		t.UUID = uuid
	case *nbdb.QoS:
		t.UUID = uuid
	case *nbdb.ChassisTemplateVar:
		t.UUID = uuid
	case *nbdb.DHCPOptions:
		t.UUID = uuid
	default:
		panic(fmt.Sprintf("setUUID: unknown model %T", t))
	}
}

func copyIndexes(model model.Model) model.Model {
	switch t := model.(type) {
	case *nbdb.ACL:
		return &nbdb.ACL{
			UUID: t.UUID,
			ExternalIDs: map[string]string{
				types.PrimaryIDKey: t.ExternalIDs[types.PrimaryIDKey],
			},
		}
	case *nbdb.AddressSet:
		return &nbdb.AddressSet{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.BFD:
		return &nbdb.BFD{
			UUID:        t.UUID,
			LogicalPort: t.LogicalPort,
			DstIP:       t.DstIP,
		}
	case *nbdb.Copp:
		return &nbdb.Copp{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.GatewayChassis:
		return &nbdb.GatewayChassis{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.LoadBalancer:
		return &nbdb.LoadBalancer{
			UUID: t.UUID,
			// client index
			Name: t.Name,
		}
	case *nbdb.LoadBalancerGroup:
		return &nbdb.LoadBalancerGroup{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.LogicalRouter:
		return &nbdb.LogicalRouter{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.LogicalRouterPolicy:
		return &nbdb.LogicalRouterPolicy{
			UUID: t.UUID,
		}
	case *nbdb.LogicalRouterPort:
		return &nbdb.LogicalRouterPort{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.LogicalRouterStaticRoute:
		return &nbdb.LogicalRouterStaticRoute{
			UUID: t.UUID,
		}
	case *nbdb.LogicalSwitch:
		return &nbdb.LogicalSwitch{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.LogicalSwitchPort:
		return &nbdb.LogicalSwitchPort{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.NAT:
		return &nbdb.NAT{
			UUID: t.UUID,
		}
	case *nbdb.PortGroup:
		return &nbdb.PortGroup{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.NBGlobal:
		return &nbdb.NBGlobal{
			UUID: t.UUID,
		}
	case *nbdb.MeterBand:
		return &nbdb.MeterBand{
			UUID: t.UUID,
		}
	case *nbdb.Meter:
		return &nbdb.Meter{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.Sample:
		return &nbdb.Sample{
			UUID:     t.UUID,
			Metadata: t.Metadata,
		}
	case *nbdb.SampleCollector:
		return &nbdb.SampleCollector{
			UUID: t.UUID,
			ID:   t.ID,
		}
	case *nbdb.SamplingApp:
		return &nbdb.SamplingApp{
			UUID: t.UUID,
			Type: t.Type,
		}
	case *nbdb.StaticMACBinding:
		return &nbdb.StaticMACBinding{
			UUID:        t.UUID,
			LogicalPort: t.LogicalPort,
			IP:          t.IP,
		}
	case *sbdb.Chassis:
		return &sbdb.Chassis{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *sbdb.ChassisPrivate:
		return &sbdb.ChassisPrivate{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *sbdb.IGMPGroup:
		return &sbdb.IGMPGroup{
			UUID: t.UUID,
		}
	case *sbdb.Encap:
		return &sbdb.Encap{
			UUID:        t.UUID,
			Type:        t.Type,
			IP:          t.IP,
			ChassisName: t.ChassisName,
		}
	case *sbdb.PortBinding:
		return &sbdb.PortBinding{
			UUID:        t.UUID,
			LogicalPort: t.LogicalPort,
			Datapath:    t.Datapath,
			TunnelKey:   t.TunnelKey,
		}
	case *sbdb.SBGlobal:
		return &sbdb.SBGlobal{
			UUID: t.UUID,
		}
	case *nbdb.QoS:
		return &nbdb.QoS{
			UUID: t.UUID,
			ExternalIDs: map[string]string{
				types.PrimaryIDKey: t.ExternalIDs[types.PrimaryIDKey],
			},
		}
	case *nbdb.ChassisTemplateVar:
		return &nbdb.ChassisTemplateVar{
			UUID:    t.UUID,
			Chassis: t.Chassis,
		}
	case *nbdb.DHCPOptions:
		return &nbdb.DHCPOptions{
			UUID:        t.UUID,
			ExternalIDs: copyExternalIDs(t.ExternalIDs, types.PrimaryIDKey),
		}
	default:
		panic(fmt.Sprintf("copyIndexes: unknown model %T", t))
	}
}

func getListFromModel(model model.Model) interface{} {
	switch t := model.(type) {
	case *nbdb.ACL:
		return &[]*nbdb.ACL{}
	case *nbdb.AddressSet:
		return &[]*nbdb.AddressSet{}
	case *nbdb.BFD:
		return &[]*nbdb.BFD{}
	case *nbdb.Copp:
		return &[]*nbdb.Copp{}
	case *nbdb.GatewayChassis:
		return &[]*nbdb.GatewayChassis{}
	case *nbdb.LoadBalancer:
		return &[]*nbdb.LoadBalancer{}
	case *nbdb.LoadBalancerGroup:
		return &[]*nbdb.LoadBalancerGroup{}
	case *nbdb.LogicalRouter:
		return &[]*nbdb.LogicalRouter{}
	case *nbdb.LogicalRouterPolicy:
		return &[]*nbdb.LogicalRouterPolicy{}
	case *nbdb.LogicalRouterPort:
		return &[]*nbdb.LogicalRouterPort{}
	case *nbdb.LogicalRouterStaticRoute:
		return &[]*nbdb.LogicalRouterStaticRoute{}
	case *nbdb.LogicalSwitch:
		return &[]*nbdb.LogicalSwitch{}
	case *nbdb.LogicalSwitchPort:
		return &[]*nbdb.LogicalSwitchPort{}
	case *nbdb.NAT:
		return &[]*nbdb.NAT{}
	case *nbdb.PortGroup:
		return &[]*nbdb.PortGroup{}
	case *nbdb.NBGlobal:
		return &[]*nbdb.NBGlobal{}
	case *nbdb.MeterBand:
		return &[]*nbdb.MeterBand{}
	case *nbdb.Meter:
		return &[]*nbdb.Meter{}
	case *nbdb.Sample:
		return &[]*nbdb.Sample{}
	case *nbdb.SampleCollector:
		return &[]*nbdb.SampleCollector{}
	case *nbdb.SamplingApp:
		return &[]*nbdb.SamplingApp{}
	case *nbdb.StaticMACBinding:
		return &[]*nbdb.StaticMACBinding{}
	case *sbdb.Chassis:
		return &[]*sbdb.Chassis{}
	case *sbdb.ChassisPrivate:
		return &[]*sbdb.ChassisPrivate{}
	case *sbdb.IGMPGroup:
		return &[]*sbdb.IGMPGroup{}
	case *sbdb.Encap:
		return &[]*sbdb.Encap{}
	case *sbdb.PortBinding:
		return &[]*sbdb.PortBinding{}
	case *nbdb.QoS:
		return &[]nbdb.QoS{}
	case *nbdb.ChassisTemplateVar:
		return &[]*nbdb.ChassisTemplateVar{}
	case *nbdb.DHCPOptions:
		return &[]nbdb.DHCPOptions{}
	default:
		panic(fmt.Sprintf("getModelList: unknown model %T", t))
	}
}

// onModels applies the provided function to a collection of
// models presented in different ways:
// - a single model (pointer to a struct)
// - a slice of models or pointer to slice of models
// - a slice of structs or pointer to a slice of structs
// If the provided function returns an error, iteration stops and
// that error is returned.
func onModels(models interface{}, do func(interface{}) error) error {
	v := reflect.ValueOf(models)
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice:
		switch v.Type().Elem().Kind() {
		case reflect.Struct:
			for i := 0; i < v.Len(); i++ {
				model := v.Index(i).Addr().Interface()
				err := do(model)
				if err != nil {
					return err
				}
			}
		case reflect.Interface:
			fallthrough
		case reflect.Ptr:
			for i := 0; i < v.Len(); i++ {
				model := v.Index(i).Interface()
				err := do(model)
				if err != nil {
					return err
				}
			}
		default:
			panic(fmt.Sprintf("Expected slice of pointers or structs but got %s", v.Type().Elem().Kind()))
		}
	case reflect.Struct:
		err := do(models)
		if err != nil {
			return err
		}
	default:
		panic(fmt.Sprintf("Expected slice or struct but got %s", v.Kind()))
	}
	return nil
}

// buildFailOnDuplicateOps builds a wait operation on a condition that will fail
// if a duplicate to the provided model is considered to be found. We use this
// to avoid duplicates on certain unknown scenarios that are still to be tracked
// down. See: https://bugzilla.redhat.com/show_bug.cgi?id=2042001.
// When no specific operation is required for the provided model, returns an empty
// array for convenience.
func buildFailOnDuplicateOps(c client.Client, m model.Model) ([]ovsdb.Operation, error) {
	// Right now we mostly consider models with a "Name" field that is not an
	// index for which we don't expect duplicate names.
	// A duplicate Name field that is an index will fail without the
	// need of this wait operation.
	// Some models that require a complex condition to detect duplicates are not
	// considered for the time being due to the performance hit (e.g ACLs).
	timeout := types.OVSDBWaitTimeout
	var field interface{}
	var value string
	switch t := m.(type) {
	case *nbdb.LogicalRouter:
		field = &t.Name
		value = t.Name
	case *nbdb.LogicalSwitch:
		field = &t.Name
		value = t.Name
	case *nbdb.LogicalRouterPolicy:
		condPriority := model.Condition{
			Field:    &t.Priority,
			Function: ovsdb.ConditionEqual,
			Value:    t.Priority,
		}
		condMatch := model.Condition{
			Field:    &t.Match,
			Function: ovsdb.ConditionEqual,
			Value:    t.Match,
		}
		condExtID := model.Condition{
			Field:    &t.ExternalIDs,
			Function: ovsdb.ConditionIncludes,
			Value:    t.ExternalIDs,
		}
		return c.WhereAll(t, condPriority, condMatch, condExtID).Wait(
			ovsdb.WaitConditionNotEqual,
			&timeout,
			t,
			&t.Priority,
			&t.Match,
		)
	default:
		return []ovsdb.Operation{}, nil
	}

	cond := model.Condition{
		Field:    field,
		Function: ovsdb.ConditionEqual,
		Value:    value,
	}
	return c.WhereAny(m, cond).Wait(ovsdb.WaitConditionNotEqual, &timeout, m, field)
}

// ModelUpdateField enumeration represents fields that can be updated on the supported models
type ModelUpdateField int

const (
	LogicalSwitchPortAddresses ModelUpdateField = iota
	LogicalSwitchPortType
	LogicalSwitchPortTagRequest
	LogicalSwitchPortOptions
	LogicalSwitchPortPortSecurity
	LogicalSwitchPortEnabled

	PortGroupACLs
	PortGroupPorts
	PortGroupExternalIDs
)

// getFieldsToUpdate gets a model and a list of ModelUpdateField and returns a list of their related interface{} fields.
func getFieldsToUpdate(model model.Model, fieldNames []ModelUpdateField) []interface{} {
	var fields []interface{}
	switch t := model.(type) {
	case *nbdb.LogicalSwitchPort:
		for _, field := range fieldNames {
			switch field {
			case LogicalSwitchPortAddresses:
				fields = append(fields, &t.Addresses)
			case LogicalSwitchPortType:
				fields = append(fields, &t.Type)
			case LogicalSwitchPortTagRequest:
				fields = append(fields, &t.TagRequest)
			case LogicalSwitchPortOptions:
				fields = append(fields, &t.Options)
			case LogicalSwitchPortPortSecurity:
				fields = append(fields, &t.PortSecurity)
			case LogicalSwitchPortEnabled:
				fields = append(fields, &t.Enabled)
			default:
				panic(fmt.Sprintf("getFieldsToUpdate: unknown or unsupported field %q for LogicalSwitchPort", field))
			}
		}
	case *nbdb.PortGroup:
		for _, field := range fieldNames {
			switch field {
			case PortGroupACLs:
				fields = append(fields, &t.ACLs)
			case PortGroupPorts:
				fields = append(fields, &t.Ports)
			case PortGroupExternalIDs:
				fields = append(fields, &t.ExternalIDs)
			default:
				panic(fmt.Sprintf("getFieldsToUpdate: unknown or unsupported field %q for PortGroup", field))
			}
		}
	default:
		panic(fmt.Sprintf("getFieldsToUpdate: unknown model type %T", t))
	}
	return fields
}

// getAllUpdatableFields returns a list of all of the columns/fields that can be updated for a model
func getAllUpdatableFields(model model.Model) []interface{} {
	switch t := model.(type) {
	case *nbdb.LogicalSwitchPort:
		return []interface{}{&t.Addresses, &t.Type, &t.TagRequest, &t.Options, &t.PortSecurity}
	case *nbdb.PortGroup:
		return []interface{}{&t.ACLs, &t.Ports, &t.ExternalIDs}
	default:
		panic(fmt.Sprintf("getAllUpdatableFields: unknown model %T", t))
	}
}

func copyExternalIDs(externalIDs map[string]string, keys ...string) map[string]string {
	var externalIDsCopy map[string]string
	for _, key := range keys {
		value, ok := externalIDs[key]
		if ok {
			if externalIDsCopy == nil {
				externalIDsCopy = map[string]string{}
			}
			externalIDsCopy[key] = value
		}
	}
	return externalIDsCopy
}
