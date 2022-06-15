package libovsdbops

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
	case *sbdb.Chassis:
		return t.UUID
	case *sbdb.ChassisPrivate:
		return t.UUID
	case *sbdb.MACBinding:
		return t.UUID
	case *sbdb.SBGlobal:
		return t.UUID
	case *nbdb.QoS:
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
	case *sbdb.Chassis:
		t.UUID = uuid
	case *sbdb.ChassisPrivate:
		t.UUID = uuid
	case *sbdb.MACBinding:
		t.UUID = uuid
	case *sbdb.SBGlobal:
		t.UUID = uuid
	case *nbdb.QoS:
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
		}
	case *nbdb.LoadBalancerGroup:
		return &nbdb.LoadBalancerGroup{
			UUID: t.UUID,
			Name: t.Name,
		}
	case *nbdb.LogicalRouter:
		return &nbdb.LogicalRouter{
			UUID: t.UUID,
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
	case *sbdb.MACBinding:
		return &sbdb.MACBinding{
			UUID:        t.UUID,
			LogicalPort: t.LogicalPort,
			IP:          t.IP,
		}
	case *sbdb.SBGlobal:
		return &sbdb.SBGlobal{
			UUID: t.UUID,
		}
	case *nbdb.QoS:
		return &nbdb.QoS{
			UUID: t.UUID,
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
	case *sbdb.Chassis:
		return &[]*sbdb.Chassis{}
	case *sbdb.ChassisPrivate:
		return &[]*sbdb.ChassisPrivate{}
	case *sbdb.MACBinding:
		return &[]*sbdb.MACBinding{}
	case *nbdb.QoS:
		return &[]nbdb.QoS{}
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
	case *nbdb.LoadBalancer:
		field = &t.Name
		value = t.Name
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
		return c.WhereAll(t, condPriority, condMatch).Wait(
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
	return c.Where(m, cond).Wait(ovsdb.WaitConditionNotEqual, &timeout, m, field)
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
