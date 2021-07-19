package libovsdb

import (
	"fmt"
	"reflect"

	"github.com/onsi/gomega"
	gomegaformat "github.com/onsi/gomega/format"
	gomegatypes "github.com/onsi/gomega/types"
	libovsdbclient "github.com/ovn-org/libovsdb/client"
)

// If x and y are structs, interfaces that contain struct or pointers to struct,
// it will perform deep equal ignoring any type field tagged with `ovsdb:"_uuid"`
// in those structs. Otherwise it will perform a standard deep equal
func testDataDeepEqualIgnoringUUID(x, y interface{}) bool {
	if x == nil || y == nil {
		return x == y
	}
	v1 := reflect.ValueOf(x)
	v2 := reflect.ValueOf(y)
	if !v1.IsValid() || !v2.IsValid() {
		return v1.IsValid() == v2.IsValid()
	}
	if v1.Type() != v2.Type() {
		return false
	}
	return testDataDeepValueEqualIgnoringUUID(v1, v2, true)
}

func testDataDeepValueEqualIgnoringUUID(v1, v2 reflect.Value, ignoreUUID bool) bool {
	switch v1.Kind() {
	case reflect.Ptr:
		if v1.Pointer() == v2.Pointer() {
			return true
		}
		if ignoreUUID {
			return testDataDeepValueEqualIgnoringUUID(v1.Elem(), v2.Elem(), false)
		}
		return reflect.DeepEqual(v1.Elem().Interface(), v2.Elem().Interface())
	case reflect.Interface:
		if v1.IsNil() || v2.IsNil() {
			return v1.IsNil() == v2.IsNil()
		}
		if ignoreUUID {
			return testDataDeepValueEqualIgnoringUUID(v1.Elem(), v2.Elem(), false)
		}
		return reflect.DeepEqual(v1.Elem().Interface(), v2.Elem().Interface())
	case reflect.Struct:
		for i, n := 0, v1.NumField(); i < n; i++ {
			if tag := v1.Type().Field(i).Tag.Get("ovsdb"); tag == "_uuid" {
				continue
			}
			if !reflect.DeepEqual(v1.Field(i).Interface(), v2.Field(i).Interface()) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(v1.Interface(), v2.Interface())
}

func HaveTestData(expected ...TestData) gomegatypes.GomegaMatcher {
	return haveTestData(false, expected)
}

func HaveTestDataIgnoringUUIDs(expected ...TestData) gomegatypes.GomegaMatcher {
	return haveTestData(true, expected)
}

func HaveEmptyTestData() gomegatypes.GomegaMatcher {
	transform := func(client libovsdbclient.Client) []TestData {
		return getTestDataFromClientCache(client)
	}
	return gomega.WithTransform(transform, gomega.BeEmpty())
}

func haveTestData(ignoreUUID bool, expected []TestData) gomegatypes.GomegaMatcher {
	if e, ok := expected[0].([]TestData); len(expected) == 1 && ok {
		// flatten
		expected = e
	}
	matchers := []gomegatypes.GomegaMatcher{}
	for _, e := range expected {
		matchers = append(matchers, matchTestData(ignoreUUID, e))
	}
	transform := func(client libovsdbclient.Client) []TestData {
		return getTestDataFromClientCache(client)
	}
	return gomega.WithTransform(transform, gomega.ContainElements(matchers))
}

func MatchTestDataIgnoringUUID(expected TestData) gomegatypes.GomegaMatcher {
	return matchTestData(true, expected)
}

func MatchTestData(expected TestData) gomegatypes.GomegaMatcher {
	return matchTestData(false, expected)
}

func matchTestData(ignoreUUID bool, expected TestData) gomegatypes.GomegaMatcher {
	return &testDataMatcher{
		expected:   expected,
		ignoreUUID: ignoreUUID,
	}
}

type testDataMatcher struct {
	expected   TestData
	ignoreUUID bool
}

func (matcher *testDataMatcher) Match(actual interface{}) (bool, error) {
	data, ok := actual.(TestData)
	if !ok {
		return false, fmt.Errorf("MatchServerData matcher expects an libovsdb.ServerData")
	}
	if matcher.ignoreUUID {
		return testDataDeepEqualIgnoringUUID(data, matcher.expected), nil
	}
	return reflect.DeepEqual(data, matcher.expected), nil
}

func (matcher *testDataMatcher) FailureMessage(actual interface{}) string {
	return gomegaformat.Message(actual, "to equal (except uuid)", matcher.expected)
}

func (matcher *testDataMatcher) NegatedFailureMessage(actual interface{}) string {
	return gomegaformat.Message(actual, "not to equal (except uuid)", matcher.expected)
}
