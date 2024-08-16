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

	return clearPodBandwidthForPorts(portList, sandboxID)
}

func clearPodBandwidthForPorts(portList []string, sandboxID string) error {
	// Clear the QoS for any ports of this sandbox
	for _, port := range portList {
		if err := ovsClear("port", port, "qos"); err != nil {
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

func getOvsPortBandwidth(ifname string, dir direction) (int64, error) {
	// note pod ingress == OVS egress and vice versa
	// so we ingress_policing_rate is egress and max-rate is ingress from the pod's
	// perspective

	// ingressBPS
	if dir == Ingress {
		return getInterfaceIngressBandwith(ifname)
	}
	// egreessBPS
	return getInterfaceEgressBandwith(ifname)
}

func getInterfaceIngressBandwith(ifname string) (int64, error) {
	qos_id, err := ovsGet("port", ifname, "qos", "")
	if err != nil {
		return 0, fmt.Errorf("failed to get qos for port %s: %w", ifname, err)
	}
	if len(qos_id) == 0 {
		return 0, BandwidthNotFound
	}
	maxRate, err := ovsGet("qos", qos_id, "other_config", "max-rate")
	if err != nil {
		return 0, fmt.Errorf("failed to get max-rate for qos_id %s: %w", qos_id, err)
	}
	if len(maxRate) == 0 {
		return 0, BandwidthNotFound
	}
	maxRate = strings.ReplaceAll(maxRate, "\"", "")
	ingressBPS, err := strconv.ParseInt(maxRate, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse qos max rate for %s: %w", ifname, err)
	}
	return ingressBPS, nil
}

func getInterfaceEgressBandwith(ifname string) (int64, error) {
	// egressBPS
	out, err := ovsGet("interface", ifname, "ingress_policing_rate", "")
	if err != nil {
		return 0, fmt.Errorf("failed to get ingress_policing_rate for interface %s: %w", ifname, err)
	}
	if len(out) == 0 {
		return 0, BandwidthNotFound
	}
	egressValue, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse ingress_policing_rate for interface %s from %q: %w", ifname, out, err)
	}
	if egressValue == 0 { // 0 is the default value so we return not found
		return 0, BandwidthNotFound
	}

	return egressValue * 1000, nil
}
