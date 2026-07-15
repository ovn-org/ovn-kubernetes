// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"

	"github.com/urfave/cli/v2"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// NicsToBridgeCommand creates ovs bridge for provided nic interfaces.
var NicsToBridgeCommand = cli.Command{
	Name:  "nics-to-bridge",
	Usage: "Create ovs bridge for nic interfaces",
	Flags: []cli.Flag{},
	Action: func(ctx *cli.Context) error {
		args := ctx.Args()
		if args.Len() == 0 {
			return fmt.Errorf("please specify list of nic interfaces")
		}

		ovsClient, err := libovsdb.NewOVSClient(ctx.Context.Done())
		if err != nil {
			klog.Errorf("Error initializing ovs client: %v", err)
			return err
		}

		var errorList []error
		for _, nic := range args.Slice() {
			if _, err := util.NicToBridge(ovsClient, nic); err != nil {
				errorList = append(errorList, err)
			}
		}

		return utilerrors.Join(errorList...)
	},
}

// BridgesToNicCommand removes a NIC interface from OVS bridge and deletes the bridge
var BridgesToNicCommand = cli.Command{
	Name:  "bridges-to-nic",
	Usage: "Delete ovs bridge and move IP/routes to underlying NIC",
	Flags: []cli.Flag{},
	Action: func(ctx *cli.Context) error {
		args := ctx.Args()
		if args.Len() == 0 {
			return fmt.Errorf("please specify list of bridges")
		}

		ovsClient, err := libovsdb.NewOVSClient(ctx.Context.Done())
		if err != nil {
			klog.Errorf("Error initializing ovs client: %v", err)
			return err
		}

		var errorList []error
		for _, bridge := range args.Slice() {
			if err := util.BridgeToNic(ovsClient, bridge); err != nil {
				errorList = append(errorList, err)
			}
		}

		return utilerrors.Join(errorList...)
	},
}
