package ovnwebhook

import (
	"reflect"
	"testing"
)

func Test_mapDiff(t *testing.T) {
	tests := []struct {
		name     string
		old      map[string]string
		new      map[string]string
		expected map[string]annotationChange
	}{
		{
			name:     "no changes",
			old:      map[string]string{"key1": "value1", "key2": "value2"},
			new:      map[string]string{"key1": "value1", "key2": "value2"},
			expected: map[string]annotationChange{},
		},
		{
			name: "value removed",
			old:  map[string]string{"key1": "value1", "key2": "value2"},
			new:  map[string]string{"key1": "value1"},
			expected: map[string]annotationChange{
				"key2": {action: removed, value: "value2"},
			},
		},
		{
			name: "value changed",
			old:  map[string]string{"key1": "value1", "key2": "value2"},
			new:  map[string]string{"key1": "newvalue1", "key2": "value2"},
			expected: map[string]annotationChange{
				"key1": {action: changed, value: "newvalue1"},
			},
		},
		{
			name: "new value",
			old:  map[string]string{"key1": "value1"},
			new:  map[string]string{"key1": "value1", "key2": "value2"},
			expected: map[string]annotationChange{
				"key2": {action: added, value: "value2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if gotChanges := mapDiff(tt.old, tt.new); !reflect.DeepEqual(gotChanges, tt.expected) {
				t.Errorf("mapDiff() = %v, want %v", gotChanges, tt.expected)
			}
		})
	}
}
