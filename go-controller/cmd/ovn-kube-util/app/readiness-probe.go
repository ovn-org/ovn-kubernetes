package app

import (
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
	kexec "k8s.io/utils/exec"
)

type readinessFunc func(string) error

var callbacks = map[string]readinessFunc{
	"ovn-controller": ovnControllerReadiness,
	"ovnnb-db":       ovnNBDBReadiness,
	"ovnsb-db":       ovnSBDBReadiness,
	"ovn-northd":     ovnNorthdReadiness,
	"ovn-nbctld":     ovnNbCtldReadiness,
	"ovs-daemons":    ovsDaemonsReadiness,
}

func ovnControllerReadiness(target string) error {
	// Check if ovn-controller is connected to OVN SB
	output, _, err := util.RunOVSAppctlWithTimeout(5, "-t", target, "connection-status")
	if err != nil {
		return fmt.Errorf("failed getting connection status of %q: (%v)", target, err)
	} else if output != "connected" {
		return fmt.Errorf("%q is not connected to OVN SB database, status: (%s)", target, output)
	}

	// Ensure that the ovs-vswitchd and ovsdb-server processes that ovn-controller
	// dependent on are running and you need to use ovs-appctl via the unix control path
	ovsdbPid, err := ioutil.ReadFile("/var/run/openvswitch/ovsdb-server.pid")
	if err != nil {
		return fmt.Errorf("failed to get pid for osvdb-server process: %v", err)
	}
	ctlFile := fmt.Sprintf("/var/run/openvswitch/ovsdb-server.%s.ctl", strings.Trim(string(ovsdbPid), " \n"))
	_, _, err = util.RunOVSAppctlWithTimeout(5, "-t", ctlFile, "ovsdb-server/list-dbs")
	if err != nil {
		return fmt.Errorf("failed retrieving list of databases from ovsdb-server: %v", err)
	}

	ovsPid, err := ioutil.ReadFile("/var/run/openvswitch/ovs-vswitchd.pid")
	if err != nil {
		return fmt.Errorf("failed to get pid for ovs-vswitchd process: %v", err)
	}
	ctlFile = fmt.Sprintf("/var/run/openvswitch/ovs-vswitchd.%s.ctl", strings.Trim(string(ovsPid), " \n"))
	_, _, err = util.RunOVSAppctlWithTimeout(5, "-t", ctlFile, "ofproto/list")
	if err != nil {
		return fmt.Errorf("failed to retrieve ofproto instances from ovs-vswitchd: %v", err)
	}
	return nil
}

func ovnNBDBReadiness(target string) error {
	var err error
	var output string

	// 1. Check if the OVN NB process is running.
	// 2. Check if OVN NB process is listening on the port that it is supposed to
	_, _, err = util.RunOVNAppctlWithTimeout(5, "-t", fmt.Sprintf("%s/ovnnb_db.ctl", util.GetOvnRunDir()),
		"ovsdb-server/list-dbs")
	if err != nil {
		return fmt.Errorf("failed connecting to %q: (%v)", target, err)
	}
	output, _, err = util.RunOVNNbctlWithTimeout(5, "--data=bare", "--no-heading", "--columns=target",
		"find", "connection", "target!=_")
	if err != nil {
		return fmt.Errorf("%s is not ready: (%v)", target, err)
	}

	if strings.HasPrefix(output, "ptcp") || strings.HasPrefix(output, "pssl") {
		return nil
	}
	return fmt.Errorf("%s is not setup for passive connection: %v", target, output)
}

func ovnSBDBReadiness(target string) error {
	var err error
	var output string

	// 1. Check if the OVN SB process is running.
	// 2. Check if OVN SB process is listening on the port that it is supposed to
	_, _, err = util.RunOVNAppctlWithTimeout(5, "-t", fmt.Sprintf("%s/ovnsb_db.ctl", util.GetOvnRunDir()),
		"ovsdb-server/list-dbs")
	if err != nil {
		return fmt.Errorf("failed connecting to %q: (%v)", target, err)
	}
	output, _, err = util.RunOVNSbctlWithTimeout(5, "--data=bare", "--no-heading", "--columns=target",
		"find", "connection", "target!=_")
	if err != nil {
		return fmt.Errorf("%s is not ready: (%v)", target, err)
	}

	if strings.HasPrefix(output, "ptcp") || strings.HasPrefix(output, "pssl") {
		return nil
	}
	return fmt.Errorf("%s is not setup for passive connection: %v", target, output)
}

func ovnNorthdReadiness(target string) error {
	_, _, err := util.RunOVNAppctlWithTimeout(5, "-t", target, "version")
	if err != nil {
		return fmt.Errorf("failed to get version from %s: (%v)", target, err)
	}
	return nil
}

func ovnNbCtldReadiness(target string) error {
	_, _, err := util.RunOVNAppctlWithTimeout(5, "-t", "ovn-nbctl", "version")
	if err != nil {
		return fmt.Errorf("failed to get version from %s: (%v)", target, err)
	}
	return nil
}

func ovsDaemonsReadiness(target string) error {
	_, _, err := util.RunOVSAppctlWithTimeout(5, "-t", "ovsdb-server", "ovsdb-server/list-dbs")
	if err != nil {
		return fmt.Errorf("failed retrieving list of databases from ovsdb-server: %v", err)
	}
	_, _, err = util.RunOVSAppctlWithTimeout(5, "-t", "ovs-vswitchd", "ofproto/list")
	if err != nil {
		return fmt.Errorf("failed to retrieve ofproto instances from ovs-vswitchd: %v", err)
	}
	return nil
}

// ReadinessProbeCommand runs readiness probes against various targets
var ReadinessProbeCommand = cli.Command{
	Name:  "readiness-probe",
	Usage: "check readiness of the specified target daemon",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "target, t",
			Usage: "target daemon to check for readiness",
		},
	},
	Action: func(ctx *cli.Context) error {
		target := ctx.String("target")
		if err := util.SetExec(kexec.New()); err != nil {
			return err
		}

		return callbacks[target](target)
	},
}
