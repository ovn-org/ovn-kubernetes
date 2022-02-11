package libovsdbops

import (
	"context"
	"fmt"
	"reflect"

	libovsdbclient "github.com/ovn-org/libovsdb/client"

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
)

func GetMeterNameForProtocol(protocol string) string {
	return protocol + "-" + types.OvnRateLimitingMeter
}

// CreateDefaultCOPP creates the default COPP that needs to be added to each GR
func CreateDefaultCOPP(nbClient libovsdbclient.Client) (string, error) {
	coppUUIDResult := ""
	defaultProtocolNames := []string{
		OVNARPRateLimiter,
		OVNARPResolveRateLimiter,
		OVNBFDRateLimiter,
		OVNControllerEventsRateLimiter,
		OVNICMPV4ErrorsRateLimiter,
		OVNICMPV6ErrorsRateLimiter,
		OVNRejectRateLimiter,
		OVNTCPRSTRateLimiter,
	}
	metersToAdd := make(map[string]string, len(defaultProtocolNames))
	for _, protocol := range defaultProtocolNames {
		// format: <OVNSupportedProtocolName>-rate-limiter
		metersToAdd[protocol] = GetMeterNameForProtocol(protocol)
	}

	ctx, cancel := context.WithTimeout(context.Background(), types.OVSDBTimeout)
	defer cancel()
	controlPlaneProtection := []nbdb.Copp{}
	// TODO(tssurya): querying based on maps is not efficient, fix this to use name field when
	// https://bugzilla.redhat.com/show_bug.cgi?id=2040852 is fixed
	err := nbClient.WhereCache(func(copp *nbdb.Copp) bool { return reflect.DeepEqual(copp.Meters, metersToAdd) }).List(ctx, &controlPlaneProtection)
	if err != nil && err != libovsdbclient.ErrNotFound {
		return coppUUIDResult, fmt.Errorf("error while creating default COPP %+v:%v", controlPlaneProtection, err)
	} else if len(controlPlaneProtection) == 0 {
		// no copp found
		// Create a meter band with 25 pktps rate.
		meterBand := &nbdb.MeterBand{
			Action: types.MeterAction,
			Rate:   int(25), // hard-coding for now. TODO(tssurya): make this configurable if needed
		}

		// we cannot have a standalone meterBand, always needs to be created with a meter
		// so the first meter is created with the band and rest of the meters then reference
		// the same band.
		meterFairness := true
		meter := &nbdb.Meter{
			Name: GetMeterNameForProtocol(defaultProtocolNames[0]),
			Fair: &meterFairness,
			Unit: types.PacketsPerSecond,
		}
		results, err := CreateMeterWithBand(nbClient, meter, meterBand)
		if err != nil {
			return coppUUIDResult, fmt.Errorf("error while creating default COPP %+v:%v", controlPlaneProtection, err)
		}
		meterBandUUID := results[0].UUID.GoUUID

		// Create default meters for the various proto types with the above band
		var opModels []OperationModel
		for i := 1; i < len(defaultProtocolNames); i++ {
			meter := &nbdb.Meter{
				Name:  GetMeterNameForProtocol(defaultProtocolNames[i]),
				Fair:  &meterFairness,
				Unit:  types.PacketsPerSecond,
				Bands: []string{meterBandUUID},
			}
			opModels = append(opModels, []OperationModel{
				{
					Model: meter,
				},
			}...)
		}

		// Create a COPP with the above meter and protocols
		copp := &nbdb.Copp{Meters: metersToAdd}
		opModels = append(opModels, []OperationModel{
			{
				Model: copp,
			},
		}...)
		m := NewModelClient(nbClient)
		results, err = m.CreateOrUpdate(opModels...)
		if err != nil {
			return coppUUIDResult, fmt.Errorf("error while creating default COPP %+v:%v", controlPlaneProtection, err)
		}
		// results will have default meters and copp at last index
		coppUUIDResult = results[len(results)-1].UUID.GoUUID
	} else {
		// copp exists
		coppUUIDResult = controlPlaneProtection[0].UUID
	}

	return coppUUIDResult, nil
}
