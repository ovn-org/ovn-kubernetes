package cni

import (
	"fmt"
	"strconv"
	"strings"
)

func clearPodBandwidth(sandboxID string) error {
	// interfaces will have the same name as ports
	portList, err := ovsFind("interface", "name", "external-ids:sandbox="+sandboxID)
	if err != nil {
		return err
	}

	// Clear the QoS for any ports of this sandbox
	for _, port := range portList {
		if err = ovsClear("port", port, "qos"); err != nil {
			return err
		}
	}

	// Now that the QoS is unused remove it
	qosList, err := ovsFind("qos", "_uuid", "external-ids:sandbox="+sandboxID)
	if err != nil {
		return err
	}
	for _, qos := range qosList {
		if err := ovsDestroy("qos", qos); err != nil {
			return err
		}
	}

	return nil
}

func setPodBandwidth(sandboxID, ifname string, ingressBPS, egressBPS int64) error {
	// note pod ingress == OVS egress and vice versa

	if ingressBPS > 0 {
		qos, err := ovsCreate("qos", "type=linux-htb", fmt.Sprintf("other-config:max-rate=%d", ingressBPS), "external-ids=sandbox="+sandboxID)
		if err != nil {
			return err
		}
		err = ovsSet("port", ifname, fmt.Sprintf("qos=%s", qos))
		if err != nil {
			return err
		}
	}
	if egressBPS > 0 {
		// ingress_policing_rate is in Kbps
		egressKBPS := egressBPS / 1000
		err := ovsSet("interface", ifname, fmt.Sprintf("ingress_policing_rate=%d", egressKBPS))
		if err != nil {
			return err
		}
		// Set the ingress_policing_burst too per recommendation in ovsdb schema, i.e
		// 10% of the rate
		err = ovsSet("interface", ifname, fmt.Sprintf("ingress_policing_burst=%d", (egressKBPS/10)))
		if err != nil {
			return err
		}
	}

	return nil
}

// Returns bandwidth restrictions (ingress and egress in bits/sec) that were set on the port.
// If none restrictions were set, the function returns -1 for ingress and 0 (the OVS default value) for egress
func getPodBandwidth(ifname string) (ingressBPS, egressBPS int64, err error) {
	// note pod ingress == OVS egress and vice versa
	ingressBPS = int64(-1)
	egressBPS = int64(-1)
	var out string

	// ingressBPS
	qos_id, err := ovsGet("port", ifname, "qos", "")
	if err == nil && len(qos_id) > 0 {
		out, err = ovsGet("qos", qos_id, "other_config", "max-rate")
		if err == nil && len(out) > 0 {
			out = strings.ReplaceAll(out, "\"", "")
			ingressBPS, err = strconv.ParseInt(out, 10, 64)
		}
	}
	if err != nil {
		return ingressBPS, egressBPS, err
	}
	// egressBPS
	out, err = ovsGet("interface", ifname, "ingress_policing_rate", "")
	if err == nil && len(out) > 0 {
		egressBPS, err = strconv.ParseInt(out, 10, 64)
		if err == nil {
			egressBPS *= 1000
		}
	}
	return ingressBPS, egressBPS, err
}
