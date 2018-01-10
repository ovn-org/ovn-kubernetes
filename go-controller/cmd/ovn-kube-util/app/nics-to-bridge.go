package app

import (
	"fmt"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
	"k8s.io/apimachinery/pkg/util/errors"
)

// NicsToBridgeCommand creates ovs bridge for provided nic interfaces.
var NicsToBridgeCommand = cli.Command{
	Name:  "nics-to-bridge",
	Usage: "Create ovs bridge for nic interfaces",
	Flags: []cli.Flag{},
	Action: func(context *cli.Context) error {
		args := context.Args()
		if len(args) == 0 {
			return fmt.Errorf("Please specify list of nic interfaces")
		}

		var errorList []error
		for _, nic := range args {
			if err := util.NicToBridge(nic); err != nil {
				errorList = append(errorList, err)
			}
		}

		return errors.NewAggregate(errorList)
	},
}
