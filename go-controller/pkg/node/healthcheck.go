package node

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	kapi "k8s.io/api/core/v1"
	"k8s.io/klog"
)

func countLocalEndpoints(ep *kapi.Endpoints, nodeName string) int {
	num := 0
	for i := range ep.Subsets {
		ss := &ep.Subsets[i]
		for i := range ss.Addresses {
			addr := &ss.Addresses[i]
			if addr.NodeName != nil && *addr.NodeName == nodeName {
				num++
			}
		}
	}
	return num
}

// check for OVS internal ports without any ofport assigned, they are stale ports that must be deleted
func checkForStaleOVSInterfaces(stopChan <-chan struct{}) {
	for {
		select {
		case <-time.After(60 * time.Second):
			stdout, _, err := util.RunOVSVsctl("--data=bare", "--no-headings", "--columns=name", "find",
				"interface", "ofport=-1")
			if err != nil {
				klog.Errorf("failed to list OVS interfaces with ofport set to -1")
				continue
			}
			if len(stdout) == 0 {
				continue
			}
			values := strings.Split(stdout, "\n\n")
			for _, val := range values {
				klog.Warningf("found stale interface %s, so deleting it", val)
				_, stderr, err := util.RunOVSVsctl("--if-exists", "--with-iface", "del-port", val)
				if err != nil {
					klog.Errorf("failed to delete OVS port/interface %s: stderr: %s (%v)",
						val, stderr, err)
				}
			}
		case <-stopChan:
			return
		}
	}
}

// checkDefaultOpenFlow checks for the existence of default OpenFlow rules and
// exits if the output is not as expected
func checkDefaultConntrackRules(hc *sharedGatewayHealthcheck, stopChan <-chan struct{}) {
	flowCount := fmt.Sprintf("flow_count=%d", hc.nFlows)
	for {
		select {
		case <-time.After(15 * time.Second):
			out, _, err := util.RunOVSOfctl("dump-aggregate", hc.gwBridge,
				fmt.Sprintf("cookie=%s/-1", defaultOpenFlowCookie))
			if err != nil {
				klog.Errorf("failed to dump aggregate statistics of the default OpenFlow rules: %v", err)
				continue
			}

			if !strings.Contains(out, flowCount) {
				klog.Errorf("fatal error: unexpected default OpenFlows count, expect %d output: %v\n",
					hc.nFlows, out)
				os.Exit(1)
			}

			// it could be that the ovn-controller recreated the patch between the host OVS bridge and
			// the integration bridge, as a result the ofport number changed for that patch interface
			curOfportPatch, stderr, err := util.RunOVSVsctl("--if-exists", "get", "Interface", hc.patchPort, "ofport")
			if err != nil {
				klog.Errorf("Failed to get ofport of %s, stderr: %q, error: %v", hc.patchPort, stderr, err)
				continue
			}
			if hc.ofPortPatch != curOfportPatch {
				klog.Errorf("fatal error: ofport of %s has changed from %s to %s",
					hc.patchPort, hc.ofPortPatch, curOfportPatch)
				os.Exit(1)
			}

			// it could be that someone removed the physical interface and added it back on the OVS host
			// bridge, as a result the ofport number changed for that physical interface
			curOfportPhys, stderr, err := util.RunOVSVsctl("--if-exists", "get", "interface", hc.gwIntf, "ofport")
			if err != nil {
				klog.Errorf("Failed to get ofport of %s, stderr: %q, error: %v", hc.gwIntf, stderr, err)
				continue
			}
			if hc.ofPortPhys != curOfportPhys {
				klog.Errorf("fatal error: ofport of %s has changed from %s to %s",
					hc.gwIntf, hc.ofPortPhys, curOfportPhys)
				os.Exit(1)
			}
		case <-stopChan:
			return
		}
	}
}
