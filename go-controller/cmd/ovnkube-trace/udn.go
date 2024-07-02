package main

import (
	"context"
	"encoding/json"
	"fmt"

	types "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

func findUserDefinedNetworkVRFTableIDs(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace string) (string, error) {
	nodeList, err := coreclient.Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	nodesTableIDs := map[string]map[string]uint{}
	for _, node := range nodeList.Items {
		networks, err := findNetworks(&node)
		if err != nil {
			return "", err
		}
		networksTableIDs := map[string]uint{}
		for _, networkName := range networks {
			tableID, err := findUserDefinedNetworkVRFTableID(coreclient, restconfig, &node, ovnNamespace, networkName)
			if err != nil {
				return "", err
			}
			networksTableIDs[networkName] = tableID
		}
		nodesTableIDs[node.Name] = networksTableIDs
	}
	nodesTableIDsJSON, err := json.Marshal(&nodesTableIDs)
	if err != nil {
		return "", err
	}
	return string(nodesTableIDsJSON), nil
}

func findUserDefinedNetworkVRFTableID(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, node *corev1.Node, ovnNamespace, networkName string) (uint, error) {
	ovnKubePodName, err := getOvnKubePodOnNode(coreclient, ovnNamespace, node.Name)
	if err != nil {
		return 0, err
	}

	mgmtPortLinkName := util.GetSecondaryNetworkPrefix(networkName) + types.K8sMgmtIntfName
	ipLinkCmd := "ip -j link show dev " + mgmtPortLinkName
	stdout, stderr, err := execInPod(coreclient, restconfig, ovnNamespace, ovnKubePodName, ovnKubeNodePodContainers[1], ipLinkCmd, "")
	if err != nil {
		return 0, fmt.Errorf("%s: %s: %w", stdout, stderr, err)
	}
	links := []struct {
		Index uint `json:"ifindex"`
	}{}
	if err := json.Unmarshal([]byte(stdout), &links); err != nil {
		return 0, err
	}
	if len(links) < 1 {
		return 0, fmt.Errorf("link '%s' not found", mgmtPortLinkName)
	}
	return uint(util.CalculateRouteTableID(int(links[0].Index))), nil
}

func findNetworks(node *corev1.Node) ([]string, error) {
	annotation, ok := node.Annotations["k8s.ovn.org/network-ids"]
	if !ok {
		return []string{}, nil
	}
	networkIDs := make(map[string]json.RawMessage)
	if err := json.Unmarshal([]byte(annotation), &networkIDs); err != nil {
		return nil, err
	}
	networks := []string{}
	for networkName := range networkIDs {
		networks = append(networks, networkName)
	}
	return networks, nil
}
