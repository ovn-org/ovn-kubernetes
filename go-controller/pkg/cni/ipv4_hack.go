package cni

// cnishim hack to add an IPv4 interface to pods that need IPv4 access
// in nominally single-stack IPv6 clusters on dual-stack cloud hosts.
//
// Although bare metal is the only supported platform for single-stack
// IPv6 on OCP, we need to have more-or-less working single-stack IPv6
// on some cloud platform for CI and developer testing. None of our
// supported clouds actually support single-stack IPv6 operation though;
// eg, the Azure and AWS APIs are only available via IPv4 endpoints,
// which are not accessible to ordinary pods in a single-stack IPv6
// cluster.
//
// This hack uses the CNI "bridge" plugin to add an IPv4 interface to
// selected pods so that they can reach external IPv4 hosts. (It has the
// side effect of giving them IPv4 access to other ipv4-hacked pods on
// the same node as well, but this doesn't get used.) We chose to put
// the entire hack (including the list of affected pods) in
// ovn-kubernetes because it was simple; if this hack is going to stay
// around long term and we end up needing to add more pods then we might
// want to redesign it somewhat. (Note that changing it to be based on a
// pod annotation would require having some of the code in cniserver
// rather than cnishim, and then modifying the API between cniserver and
// cnishim, creating much more potential for merge conflicts with
// upstream in the future.)
//
// NOTE THAT THE IPV4 BRIDGE CODE IS ONLY USED ON UNSUPPORTED CLUSTERS:
// on single-stack IPv4 and (eventually) dual-stack clusters,
// maybeAddIPv4Hack will return right away after examining result.IPs.
// On single-stack IPv6 bare-metal clusters it will return after parsing
// /proc/cmdline. The hack is only used for single-stack IPv6 on clouds,
// which is not supported for customers.

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
