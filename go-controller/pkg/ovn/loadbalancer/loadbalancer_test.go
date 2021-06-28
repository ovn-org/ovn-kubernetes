package loadbalancer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractUUID(t *testing.T) {
	assert.Equal(t, "12341234", extractUUID(`["uuid", "12341234"]`))
}

func TestExtractMap(t *testing.T) {
	assert.Equal(t,
		map[string]string{
			"k8s.ovn.org/kind":  "Service",
			"k8s.ovn.org/owner": "default/kubernetes",
		},
		extractMap(`["map",[["k8s.ovn.org/kind","Service"],["k8s.ovn.org/owner","default/kubernetes"]]]`))
}
