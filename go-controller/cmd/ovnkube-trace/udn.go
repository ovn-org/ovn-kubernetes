package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	types "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
)

func findUserDefinedNetworkVRFTableIDs(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace string) (string, error) {
	nodeList, err := coreclient.Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	nodesTableIDs := map[string]map[string]uint{}
	for _, node := range nodeList.Items {
		networks, err := findUDNNetworks(&node)
		if err != nil {
			return "", err
		}
		networksTableIDs := map[string]uint{}
		for networkName, networkID := range networks {
			tableID, err := findUserDefinedNetworkVRFTableID(coreclient, restconfig, &node, ovnNamespace, networkID)
			if err != nil {
				return "", err
			}
			if tableID != nil {
				networksTableIDs[networkName] = *tableID
			}
		}
		nodesTableIDs[node.Name] = networksTableIDs
	}
	nodesTableIDsJSON, err := json.Marshal(&nodesTableIDs)
	if err != nil {
		return "", err
	}
	return string(nodesTableIDsJSON), nil
}

func findUserDefinedNetworkVRFTableID(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, node *corev1.Node, ovnNamespace string, networkID string) (*uint, error) {
	ovnKubePodName, err := getOvnKubePodOnNode(coreclient, ovnNamespace, node.Name)
	if err != nil {
		return nil, err
	}

	ipLinksCmd := "ip -j link"
	stdout, stderr, err := execInPod(coreclient, restconfig, ovnNamespace, ovnKubePodName, ovnKubeNodePodContainers[1], ipLinksCmd, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", stdout, stderr, err)
	}
	links := []struct {
		Name  string `json:"ifname"`
		Index uint   `json:"ifindex"`
	}{}
	if err := json.Unmarshal([]byte(stdout), &links); err != nil {
		return nil, err
	}
	networkIDInt, err := strconv.Atoi(networkID)
	if err != nil {
		return nil, fmt.Errorf("unpexpected networkID '%s': %w", networkID, err)
	}
	mgmtPortLinkName := util.GetNetworkScopedK8sMgmtHostIntfName(uint(networkIDInt))
	for _, link := range links {
		if link.Name == mgmtPortLinkName {
			return ptr.To(uint(util.CalculateUDNRouteTableID(networkIDInt))), nil
		}
	}
	return nil, nil
}

func findUDNNetworks(node *corev1.Node) (map[string]string, error) {
	annotation, ok := node.Annotations["k8s.ovn.org/network-ids"]
	if !ok {
		return nil, nil
	}
	networks := map[string]string{}
	if err := json.Unmarshal([]byte(annotation), &networks); err != nil {
		return nil, err
	}
	delete(networks, types.DefaultNetworkName)
	return networks, nil
}
