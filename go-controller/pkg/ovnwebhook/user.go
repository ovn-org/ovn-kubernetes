package ovnwebhook

import (
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/csrapprover"
	authenticationv1 "k8s.io/api/authentication/v1"
)

func ovnkubeNodeIdentity(user authenticationv1.UserInfo) (string, bool) {
	if !strings.HasPrefix(user.Username, csrapprover.NamePrefix) {
		return "", false
	}

	// Trim prefix and the last colon
	nodeName := strings.TrimPrefix(user.Username, csrapprover.NamePrefix+":")
	return nodeName, true
}
