package testing

import (
	"fmt"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func GenerateNAD(networkName, name, namespace, topology, cidr, role string) *nadapi.NetworkAttachmentDefinition {
	return GenerateNADWithConfig(name, namespace, fmt.Sprintf(
		`
{
        "cniVersion": "0.4.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":%q,
        "subnets": %q,
        "mtu": 1300,
        "netAttachDefName": %q,
        "role": %q
}
`,
		networkName,
		topology,
		cidr,
		fmt.Sprintf("%s/%s", namespace, name),
		role,
	))
}
func GenerateNADWithoutMTU(networkName, name, namespace, topology, cidr, role string) *nadapi.NetworkAttachmentDefinition {
	return GenerateNADWithConfig(name, namespace, fmt.Sprintf(
		`
{
        "cniVersion": "0.4.0",
        "name": %q,
        "type": "ovn-k8s-cni-overlay",
        "topology":%q,
        "subnets": %q,
        "netAttachDefName": %q,
        "role": %q
}
`,
		networkName,
		topology,
		cidr,
		fmt.Sprintf("%s/%s", namespace, name),
		role,
	))
}

func GenerateNADWithConfig(name, namespace, config string) *nadapi.NetworkAttachmentDefinition {
	return &nadapi.NetworkAttachmentDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nadapi.NetworkAttachmentDefinitionSpec{Config: config},
	}
}
