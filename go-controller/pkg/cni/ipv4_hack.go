package cni

// cnishim hack to add an IPv4 interface to pods that need IPv4 access
// in nominally single-stack IPv6 clusters on dual-stack cloud hosts

import (
	"context"
	"io/ioutil"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
)

var ipv4HackPodsByPlatform = map[string]map[string][]string{
	"aws": {
		// Operators that need AWS API access, which is IPv4-only
		"openshift-authentication-operator":   []string{"authentication-operator"},
		"openshift-cloud-credential-operator": []string{"cloud-credential-operator"},
		"openshift-image-registry":            []string{"cluster-image-registry-operator"},
		"openshift-ingress-operator":          []string{"ingress-operator"},
		"openshift-machine-api":               []string{"machine-api-controllers"},

		// AWS upstream DNS servers are IPv4 only
		"openshift-dns": []string{"dns-default"},

		// Needs to talk to ingress, which is IPv4-only in AWS
		"openshift-console": []string{"console"},
	},
	"azure": {
		// Operators that need Azure API access, which is IPv4-only
		"openshift-authentication-operator":   []string{"authentication-operator"},
		"openshift-cloud-credential-operator": []string{"cloud-credential-operator"},
		"openshift-image-registry":            []string{"cluster-image-registry-operator", "image-registry"},
		"openshift-ingress-operator":          []string{"ingress-operator"},
		"openshift-machine-api":               []string{"machine-api-controllers"},

		// We currently get IPv4 nameservers but this can be fixed
		// https://bugzilla.redhat.com/show_bug.cgi?id=1803832
		"openshift-dns": []string{"dns-default"},
	},
}

func maybeAddIPv4Hack(args *skel.CmdArgs, result *current.Result) error {
	// Only need IPv4 hack on single-stack IPv6
	if len(result.IPs) != 1 || result.IPs[0].Version != "6" {
		return nil
	}

	// Find hack pods for cloud platform
	bytes, err := ioutil.ReadFile("/proc/cmdline")
	if err != nil {
		return nil
	}
	var ipv4HackPods map[string][]string
	for _, arg := range strings.Split(strings.TrimSpace(string(bytes)), " ") {
		if strings.HasPrefix(arg, "ignition.platform.id=") {
			id := strings.Split(arg, "=")
			ipv4HackPods = ipv4HackPodsByPlatform[id[1]]
			break
		}
	}
	if ipv4HackPods == nil {
		return nil
	}

	cniArgs := os.Getenv("CNI_ARGS")
	mapArgs := make(map[string]string)
	for _, arg := range strings.Split(cniArgs, ";") {
		parts := strings.Split(arg, "=")
		if len(parts) != 2 {
			continue
		}
		mapArgs[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	namespace := mapArgs["K8S_POD_NAMESPACE"]
	name := mapArgs["K8S_POD_NAME"]
	if namespace == "" || name == "" {
		return nil
	}

	match := false
	for _, prefix := range ipv4HackPods[namespace] {
		if strings.HasPrefix(name, prefix) {
			match = true
			break
		}
	}
	if !match {
		return nil
	}

	v4Args := &invoke.Args{
		Command:     "ADD",
		ContainerID: args.ContainerID,
		NetNS:       args.Netns,
		IfName:      "eth4",
		Path:        "/var/lib/cni/bin",
	}
	v4Config := []byte(`{"cniVersion": "0.3.0", "name": "ipv4-hack", "type": "bridge", "bridge": "ipv4-hack", "isDefaultGateway": true, "ipMasq": true, "ipam": { "type": "host-local", "subnet": "10.192.0.0/24" } }`)
	os.Setenv("CNI_IFNAME", "eth4")
	return invoke.ExecPluginWithoutResult(context.TODO(), "/var/lib/cni/bin/bridge", v4Config, v4Args, nil)
}
