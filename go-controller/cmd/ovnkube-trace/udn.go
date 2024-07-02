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

type Link struct {
	Index uint `json:"ifindex"`
}

func findUserDefinedNetworkVRFTableIDs(coreclient *corev1client.CoreV1Client, restconfig *rest.Config, ovnNamespace, networkName string) (map[string]uint, error) {
	nodeList, err := coreclient.Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	nodesTableID := map[string]uint{}
	for _, node := range nodeList.Items {
		tableID, err := findUserDefinedNetworkVRFTableID(coreclient, restconfig, &node, ovnNamespace, networkName)
		if err != nil {
			return nil, err
		}
		nodesTableID[node.Name] = tableID
	}
	return nodesTableID, nil
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
	links := []Link{}
	if err := json.Unmarshal([]byte(stdout), &links); err != nil {
		return 0, err
	}
	if len(links) < 1 {
		return 0, fmt.Errorf("link '%s' not found", mgmtPortLinkName)
	}
	return links[0].Index, nil
}
