package ovn

import (
	"fmt"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

const (
	// Default Meters created on GRs.
	OVNARPRateLimiter              = "arp"
	OVNARPResolveRateLimiter       = "arp-resolve"
	OVNBFDRateLimiter              = "bfd"
	OVNControllerEventsRateLimiter = "event-elb"
	OVNICMPV4ErrorsRateLimiter     = "icmp4-error"
	OVNICMPV6ErrorsRateLimiter     = "icmp6-error"
	OVNRejectRateLimiter           = "reject"
	OVNTCPRSTRateLimiter           = "tcp-reset"
	OVNServiceMonitorLimiter       = "svc-monitor"

	// Default COPP object name
	defaultCOPPName = "ovnkube-default"
)

var defaultProtocolNames = [...]string{
	OVNARPRateLimiter,
	OVNARPResolveRateLimiter,
	OVNBFDRateLimiter,
	OVNControllerEventsRateLimiter,
	OVNICMPV4ErrorsRateLimiter,
	OVNICMPV6ErrorsRateLimiter,
	OVNRejectRateLimiter,
	OVNTCPRSTRateLimiter,
	OVNServiceMonitorLimiter,
}

func getMeterNameForProtocol(protocol string) string {
	// format: <OVNSupportedProtocolName>-rate-limiter
	return protocol + "-" + types.OvnRateLimitingMeter
}

func getMeterBand(rate int) *nbdb.MeterBand {
	return &nbdb.MeterBand{
		Action: types.MeterAction,
		Rate:   rate,
	}
}

// EnsureDefaultCOPP creates the default COPP that needs to be added to each GR
// if not already present. Also cleans up old COPP entries if required.
func EnsureDefaultCOPP(nbClient libovsdbclient.Client) (string, error) {
	p := func(item *nbdb.Copp) bool {
		return item.Name == ""
	}
	ops, err := libovsdbops.DeleteCOPPsWithPredicateOps(nbClient, nil, p)
	if err != nil {
		return "", fmt.Errorf("failed to delete duplicate COPPs: %w", err)
	}

	defaultBand := getMeterBand(types.DefaultRateLimit)
	bfdBand := getMeterBand(types.BFDRateLimit)
	bfdBand.ExternalIDs = map[string]string{getMeterNameForProtocol(OVNBFDRateLimiter): "true"}
	ops, err = libovsdbops.CreateOrUpdateMeterBandOps(nbClient, ops, []*nbdb.MeterBand{defaultBand, bfdBand})
	if err != nil {
		return "", fmt.Errorf("can't create meter bands %v/%v: %v", defaultBand, bfdBand, err)
	}

	meterNames := make(map[string]string, len(defaultProtocolNames))
	meterFairness := true
	for _, protocol := range defaultProtocolNames {
		// format: <OVNSupportedProtocolName>-rate-limiter
		meterName := getMeterNameForProtocol(protocol)
		meterNames[protocol] = meterName

		meter := &nbdb.Meter{
			Name: meterName,
			Fair: &meterFairness,
			Unit: types.PacketsPerSecond,
		}
		requiredBand := defaultBand
		if protocol == OVNBFDRateLimiter {
			requiredBand = bfdBand
		}
		ops, err = libovsdbops.CreateOrUpdateMeterOps(nbClient, ops, meter, []*nbdb.MeterBand{requiredBand},
			&meter.Bands, &meter.Fair, &meter.Unit)
		if err != nil {
			return "", fmt.Errorf("can't create meter %v: %v", meter, err)
		}
	}

	defaultCOPP := &nbdb.Copp{
		Name:   defaultCOPPName,
		Meters: meterNames,
	}
	ops, err = libovsdbops.CreateOrUpdateCOPPsOps(nbClient, ops, defaultCOPP)
	if err != nil {
		return "", fmt.Errorf("failed to create/update default COPP: %w", err)
	}

	if _, err := libovsdbops.TransactAndCheckAndSetUUIDs(nbClient, defaultCOPP, ops); err != nil {
		return "", fmt.Errorf("failed to transact default COPP: %w", err)
	}

	return defaultCOPP.UUID, nil
}
