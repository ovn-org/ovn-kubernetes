package util

import (
	"fmt"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/stretchr/testify/assert"
	"reflect"
	"testing"
)

func TestMarshalPodSmartNicConnDetails(t *testing.T) {
	tests := []struct {
		desc           string
		scd            *SmartNICConnectionDetails
		netName        string
		inpAnnotation  map[string]string
		expectedOutput map[string]string
	}{
		{
			desc:           "Set default smartNicConnectionDetails annotation with no fields set",
			netName:        types.DefaultNetworkName,
			inpAnnotation:  nil,
			scd:            &SmartNICConnectionDetails{},
			expectedOutput: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"","vfId":"","sandboxId":""}}`},
		},
		{
			desc:           "Set default smartNicConnectionDetails Annotation with correct fields set",
			netName:        types.DefaultNetworkName,
			inpAnnotation:  nil,
			scd:            &SmartNICConnectionDetails{"1", "4", "35b82dbe2c3976"},
			expectedOutput: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3976"}}`},
		},
		{
			desc:           "Set default smartNicConnectionDetails Annotation with legacy annoation already set",
			netName:        types.DefaultNetworkName,
			inpAnnotation:  map[string]string{SmartNicConnectionDetailsAnnot: `{"pfId": "0", "vfId": "3", "sandboxId": "35b82dbe2c3976"}`},
			scd:            &SmartNICConnectionDetails{"1", "4", "35b82dbe2c3976"},
			expectedOutput: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3976"}}`},
		},
		{
			desc:           "Set non-default smartNicConnectionDetails Annotation with correct fields set, with default annotation current not set",
			netName:        "non-default",
			inpAnnotation:  nil,
			scd:            &SmartNICConnectionDetails{"2", "5", "35b82dbe2c3977"},
			expectedOutput: map[string]string{SmartNicConnectionDetailsAnnot: `{"non-default":{"pfId":"2","vfId":"5","sandboxId":"35b82dbe2c3977"}}`},
		},
		{
			desc:           "Set non-default smartNicConnectionDetails Annotation with correct fields set, with default annoation set",
			netName:        "non-default",
			inpAnnotation:  map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3976"}}`},
			scd:            &SmartNICConnectionDetails{"2", "5", "35b82dbe2c3977"},
			expectedOutput: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3976"},"non-default":{"pfId":"2","vfId":"5","sandboxId":"35b82dbe2c3977"}}`},
		},
		{
			desc:           "Set default smartNicConnectionDetails Annotation with legacy anntoation already set",
			netName:        "non-default",
			inpAnnotation:  map[string]string{SmartNicConnectionDetailsAnnot: `{"pfId": "0", "vfId": "3", "sandboxId": "35b82dbe2c3976"}`},
			scd:            &SmartNICConnectionDetails{"1", "4", "35b82dbe2c3977"},
			expectedOutput: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"0","vfId":"3","sandboxId":"35b82dbe2c3976"},"non-default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3977"}}`},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			e := MarshalPodSmartNicConnDetails(&tc.inpAnnotation, tc.scd, tc.netName)
			t.Log(tc.inpAnnotation, e)
			assert.True(t, reflect.DeepEqual(tc.inpAnnotation, tc.expectedOutput))
		})
	}
}

func TestUnmarshalPodSmartNicConnDetails(t *testing.T) {
	tests := []struct {
		desc        string
		inpAnnotMap map[string]string
		netName     string
		errAssert   bool
		scd         SmartNICConnectionDetails
	}{
		{
			desc:        "verify `OVN pod annotation not found` error thrown",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: nil,
			errAssert:   true,
		},
		{
			desc:        "verify json unmarshal error",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":null,"vfId":"3","sandboxId":"}}`}, //removed a quote to force json unmarshal error
			errAssert:   true,
		},
		{
			desc:        "verify successful unmarshal of smartNicConnectionDetails annotation for default network",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3976"}}`},
			scd:         SmartNICConnectionDetails{"1", "4", "35b82dbe2c3976"},
		},
		{
			desc:        "verify successful unmarshal of legacy smartNicConnectionDetails annotation for default network",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnectionDetailsAnnot: `{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3976"}`},
			scd:         SmartNICConnectionDetails{"1", "4", "35b82dbe2c3976"},
		},
		{
			desc:        "verify unmarshal error of legacy smartNicConnectionDetails annotation for non-default network",
			netName:     "non-default",
			inpAnnotMap: map[string]string{SmartNicConnectionDetailsAnnot: `{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3976"}`},
			errAssert:   true,
		},
		{
			desc:        "verify successful unmarshal of multiple network smartNicConnectionDetails annotation for default network",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"0","vfId":"3","sandboxId":"35b82dbe2c3976"},"non-default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3977"}}`},
			scd:         SmartNICConnectionDetails{"0", "3", "35b82dbe2c3976"},
		},
		{
			desc:        "verify successful unmarshal of multiple network smartNicConnectionDetails annotation for non-default network",
			netName:     "non-default",
			inpAnnotMap: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"0","vfId":"3","sandboxId":"35b82dbe2c3976"},"non-default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3977"}}`},
			scd:         SmartNICConnectionDetails{"1", "4", "35b82dbe2c3977"},
		},
		{
			desc:        "verify unmarshal error of multiple network smartNicConnectionDetails annotation for non-existent network",
			netName:     "missing-network",
			inpAnnotMap: map[string]string{SmartNicConnectionDetailsAnnot: `{"default":{"pfId":"0","vfId":"3","sandboxId":"35b82dbe2c3976"},"non-default":{"pfId":"1","vfId":"4","sandboxId":"35b82dbe2c3977"}}`},
			errAssert:   true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := UnmarshalPodSmartNicConnDetails(tc.inpAnnotMap, tc.netName)
			t.Log(res, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else {
				t.Log(res)
				assert.NotNil(t, res)
				assert.True(t, reflect.DeepEqual(*res, tc.scd))
			}
		})
	}
}

func TestMarshalPodSmartNicConnStatus(t *testing.T) {
	tests := []struct {
		desc           string
		scs            *SmartNICConnectionStatus
		netName        string
		inpAnnotation  map[string]string
		expectedOutput map[string]string
	}{
		{
			desc:           "Set default smartNicConnectionStatus annotation with no fields set",
			netName:        types.DefaultNetworkName,
			inpAnnotation:  nil,
			scs:            &SmartNICConnectionStatus{},
			expectedOutput: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":""}}`},
		},
		{
			desc:           "Set default smartNicConnectionStatus Annotation with ready status set",
			netName:        types.DefaultNetworkName,
			inpAnnotation:  nil,
			scs:            &SmartNICConnectionStatus{Status: "Ready"},
			expectedOutput: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"}}`},
		},
		{
			desc:           "Set default smartNicConnectionStatus Annotation with error status set",
			netName:        types.DefaultNetworkName,
			inpAnnotation:  nil,
			scs:            &SmartNICConnectionStatus{Status: "Error", Reason: "bad-things-happened"},
			expectedOutput: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Error","Reason":"bad-things-happened"}}`},
		},
		{
			desc:           "Set default smartNicConnectionStatus Annotation with legacy annoation already set",
			netName:        types.DefaultNetworkName,
			inpAnnotation:  map[string]string{SmartNicConnetionStatusAnnot: `{"Status":"Ready"}`},
			scs:            &SmartNICConnectionStatus{Status: "Error", Reason: "bad-things-happened"},
			expectedOutput: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Error","Reason":"bad-things-happened"}}`},
		},
		{
			desc:           "Set non-default smartNicConnectionStatus Annotation with correct fields set, with default annotation current not set",
			netName:        "non-default",
			inpAnnotation:  nil,
			scs:            &SmartNICConnectionStatus{Status: "Ready"},
			expectedOutput: map[string]string{SmartNicConnetionStatusAnnot: `{"non-default":{"Status":"Ready"}}`},
		},
		{
			desc:           "Set non-default smartNicConnectionStatus Annotation with correct fields set, with default annoation set",
			netName:        "non-default",
			inpAnnotation:  map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"}}`},
			scs:            &SmartNICConnectionStatus{Status: "Error", Reason: "bad-things-happened"},
			expectedOutput: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"},"non-default":{"Status":"Error","Reason":"bad-things-happened"}}`},
		},
		{
			desc:           "Set default smartNicConnectionStatus Annotation with legacy annotation already set",
			netName:        "non-default",
			inpAnnotation:  map[string]string{SmartNicConnetionStatusAnnot: `{"Status":"Ready"}`},
			scs:            &SmartNICConnectionStatus{Status: "Error", Reason: "bad-things-happened"},
			expectedOutput: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"},"non-default":{"Status":"Error","Reason":"bad-things-happened"}}`},
		},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			e := MarshalPodSmartNicConnStatus(&tc.inpAnnotation, tc.scs, tc.netName)
			t.Log(tc.inpAnnotation, e)
			assert.True(t, reflect.DeepEqual(tc.inpAnnotation, tc.expectedOutput))
		})
	}
}

func TestUnmarshalPodSmartNicConnStatus(t *testing.T) {
	tests := []struct {
		desc        string
		inpAnnotMap map[string]string
		netName     string
		errAssert   bool
		scs         SmartNICConnectionStatus
	}{
		{
			desc:        "verify `OVN pod annotation not found` error thrown",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: nil,
			errAssert:   true,
		},
		{
			desc:        "verify json unmarshal error",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready}}`}, //removed a quote to force json unmarshal error
			errAssert:   true,
		},
		{
			desc:        "verify successful unmarshal of smartNicConnectionStatus annotation for default network",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"}}`},
			scs:         SmartNICConnectionStatus{Status: "Ready"},
		},
		{
			desc:        "verify successful unmarshal of legacy smartNicConnectionStatus annotation for default network",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnetionStatusAnnot: `{"Status":"Ready"}`},
			scs:         SmartNICConnectionStatus{Status: "Ready"},
		},
		{
			desc:        "verify unmarshal error of legacy smartNicConnectionStatus annotation for non-default network",
			netName:     "non-default",
			inpAnnotMap: map[string]string{SmartNicConnetionStatusAnnot: `{"Status":"Ready"}`},
			errAssert:   true,
		},
		{
			desc:        "verify successful unmarshal of multiple network smartNicConnectionStatus annotation for default network",
			netName:     types.DefaultNetworkName,
			inpAnnotMap: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"},"non-default":{"Status":"Error","Reason":"bad-things-happened"}}`},
			scs:         SmartNICConnectionStatus{Status: "Ready"},
		},
		{
			desc:        "verify successful unmarshal of multiple network smartNicConnectionStatus annotation for non-default network",
			netName:     "non-default",
			inpAnnotMap: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"},"non-default":{"Status":"Error","Reason":"bad-things-happened"}}`},
			scs:         SmartNICConnectionStatus{"Error", "bad-things-happened"},
		},
		{
			desc:        "verify unmarshal error of multiple network smartNicConnectionStatus annotation for non-existent network",
			netName:     "missing-network",
			inpAnnotMap: map[string]string{SmartNicConnetionStatusAnnot: `{"default":{"Status":"Ready"},"non-default":{"Status":"Error","Reason":"bad-things-happened"}}`},
			errAssert:   true,
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			res, e := UnmarshalPodSmartNicConnStatus(tc.inpAnnotMap, tc.netName)
			t.Log(res, e)
			if tc.errAssert {
				assert.Error(t, e)
			} else {
				t.Log(res)
				assert.NotNil(t, res)
				assert.True(t, reflect.DeepEqual(*res, tc.scs))
			}
		})
	}
}
