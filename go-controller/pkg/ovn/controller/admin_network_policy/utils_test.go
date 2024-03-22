package adminnetworkpolicy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/onsi/gomega"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestGetACLLoggingLevelsForANP(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    *libovsdbutil.ACLLoggingLevels
		err         string
	}{
		{
			name:        "empty annotations: logging disabled",
			annotations: map[string]string{},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "", Deny: "", Pass: "",
			},
			err: "",
		},
		{
			name: "empty string valued annotation: logging disabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: "",
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "", Deny: "", Pass: "",
			},
			err: "",
		},
		{
			name: "empty paranthesis valued annotation: logging disabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: "{}",
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "", Deny: "", Pass: "",
			},
			err: "",
		},
		{
			name: "incorrect annotation: logging disabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: "foobar",
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "", Deny: "", Pass: "",
			},
			err: "could not unmarshal ANP ACL annotation",
		},
		{
			name: "partially filled annotation; missing pass: logging enabled partially",
			annotations: map[string]string{
				util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice),
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "notice", Deny: "alert", Pass: "",
			},
			err: "",
		},
		{
			name: "correctly filled annotation for anp: logging enabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s", "pass": "%s" }`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice, nbdb.ACLSeverityInfo),
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "notice", Deny: "alert", Pass: "info",
			},
			err: "",
		},
		{
			name: "correctly filled annotation for banp: logging enabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s"}`, nbdb.ACLSeverityAlert, nbdb.ACLSeverityNotice),
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "notice", Deny: "alert", Pass: "",
			},
			err: "",
		},
		{
			name: "incorrectly filled deny value annotation: logging partially enabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s", "pass": "%s" }`, "foobar", nbdb.ACLSeverityNotice, nbdb.ACLSeverityInfo),
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "notice", Deny: "", Pass: "info",
			},
			err: "disabling deny logging due to an invalid deny annotation",
		},
		{
			name: "incorrectly filled allow value annotation: logging partially enabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s", "pass": "%s" }`, nbdb.ACLSeverityNotice, "foobar", nbdb.ACLSeverityInfo),
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "", Deny: "notice", Pass: "info",
			},
			err: "disabling allow logging due to an invalid allow annotation",
		},
		{
			name: "incorrectly filled pass value annotation: logging partially enabled",
			annotations: map[string]string{
				util.AclLoggingAnnotation: fmt.Sprintf(`{ "deny": "%s", "allow": "%s", "pass": "%s" }`, nbdb.ACLSeverityNotice, nbdb.ACLSeverityInfo, "foobar"),
			},
			expected: &libovsdbutil.ACLLoggingLevels{
				Allow: "info", Deny: "notice", Pass: "",
			},
			err: "disabling pass logging due to an invalid pass annotation",
		},
	}

	for i, tt := range tests {
		t.Run(fmt.Sprintf("%d_%s", i, tt.name), func(t *testing.T) {
			g := gomega.NewGomegaWithT(t)
			aclLogLevel, err := getACLLoggingLevelsForANP(tt.annotations)
			g.Expect(aclLogLevel).To(gomega.Equal(tt.expected))
			if err != nil {
				g.Expect(strings.Contains(err.Error(), tt.err)).To(gomega.BeTrue())
			}
		})
	}

}
