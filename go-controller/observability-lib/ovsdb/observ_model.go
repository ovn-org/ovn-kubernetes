package ovsdb

import "github.com/ovn-org/libovsdb/model"

// ObservDatabaseModel returns the DatabaseModel object to be used by observability library.
func ObservDatabaseModel() (model.ClientDBModel, error) {
	return model.NewClientDBModel("Open_vSwitch", map[string]model.Model{
		"Bridge":                    &Bridge{},
		"Flow_Sample_Collector_Set": &FlowSampleCollectorSet{},
		"Interface":                 &Interface{},
	})
}
