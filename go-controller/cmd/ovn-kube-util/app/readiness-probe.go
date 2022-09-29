package app

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	kexec "k8s.io/utils/exec"
)

type readinessFunc func(string) error

var callbacks = map[string]readinessFunc{
	"ovn-controller": ovnControllerReadiness,
	"ovnnb-db":       ovnNBDBReadiness,
	"ovnsb-db":       ovnSBDBReadiness,
	"ovn-northd":     ovnNorthdReadiness,
	"ovs-daemons":    ovsDaemonsReadiness,
	"ovnkube-node":   ovnNodeReadiness,
	"ovnnb-db-raft":  ovnNBDBRaftReadiness,
	"ovnsb-db-raft":  ovnSBDBRaftReadiness,
}

func ovnControllerReadiness(target string) error {
	// Check if ovn-controller is connected to OVN SB
	output, _, err := util.RunOVSAppctlWithTimeout(5, "-t", target, "connection-status")
	if err != nil {
		return fmt.Errorf("failed getting connection status of %q: (%v)", target, err)
	} else if output != "connected" {
		return fmt.Errorf("%q is not connected to OVN SB database, status: (%s)", target, output)
	}
	result, _, err := util.RunOVSAppctlWithTimeout(5, "-t", target, "coverage/read-counter", "lflow_run")
	if err != nil {
		return fmt.Errorf("failed getting coverage/show of %q: (%v)", target, err)
	} else if result == "0" {
		return fmt.Errorf("%q has not completed logical flows processing yet", target)
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
	_, _, err = util.RunOVNNBAppCtlWithTimeout(5, "ovsdb-server/list-dbs")
	if err != nil {
		return fmt.Errorf("failed connecting to %q: (%v)", target, err)
	}
	output, _, err = util.RunOVNNbctlWithTimeout(5, "--data=bare", "--no-heading", "--columns=target",
		"find", "connection", "target!=_")
	if err != nil {
		return fmt.Errorf("%s is not ready: (%v)", target, err)
	}

	if strings.HasPrefix(output, "ptcp") || strings.HasPrefix(output, "pssl") || output == "" {
		return nil
	}
	return fmt.Errorf("%s is not setup for passive connection: %v", target, output)
}

func ovnSBDBReadiness(target string) error {
	var err error
	var output string

	// 1. Check if the OVN SB process is running.
	// 2. Check if OVN SB process is listening on the port that it is supposed to
	_, _, err = util.RunOVNSBAppCtl("--timeout=5", "ovsdb-server/list-dbs")
	if err != nil {
		return fmt.Errorf("failed connecting to %q: (%v)", target, err)
	}
	output, _, err = util.RunOVNSbctlWithTimeout(5, "--data=bare", "--no-heading", "--columns=target",
		"find", "connection", "target!=_")
	if err != nil {
		return fmt.Errorf("%s is not ready: (%v)", target, err)
	}

	if strings.HasPrefix(output, "ptcp") || strings.HasPrefix(output, "pssl") || output == "" {
		return nil
	}
	return fmt.Errorf("%s is not setup for passive connection: %v", target, output)
}

func ovnNorthdReadiness(target string) error {
	stdout, _, err := util.RunOVNAppctlWithTimeout(5, "-t", target, "status")
	if err != nil {
		return fmt.Errorf("failed to get status from %s: (%v)", target, err)
	} else if strings.HasPrefix(stdout, "Status") {
		output := strings.Split(stdout, ":")
		status := strings.TrimSpace(output[1])
		if status != "active" && status != "paused" && status != "standby" {
			return fmt.Errorf("%s status is not active or passive or standby", target)
		}
	} else {
		return fmt.Errorf("failed to get status from %s", target)
	}
	nbConnectionStatus, _, err := util.RunOVNAppctlWithTimeout(5, "-t", target, "nb-connection-status")
	if err != nil {
		return fmt.Errorf("failed to get nb-connection-status from %s: (%v)", target, err)
	} else if nbConnectionStatus != "connected" {
		return fmt.Errorf("%s nb-connection-status is %s", target, nbConnectionStatus)
	}
	sbConnectionStatus, _, err := util.RunOVNAppctlWithTimeout(5, "-t", target, "sb-connection-status")
	if err != nil {
		return fmt.Errorf("failed to get sb-connection-status from %s: (%v)", target, err)
	} else if sbConnectionStatus != "connected" {
		return fmt.Errorf("%s sb-connection-status is %s", target, sbConnectionStatus)
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

func ovnNodeReadiness(target string) error {
	// Inside the pod we always use `/etc/cni/net.d` folder even if kubelet
	// was started with a different conf directory
	confFile := "/etc/cni/net.d/10-ovn-kubernetes.conf"
	_, err := os.Stat(confFile)
	if os.IsNotExist(err) {
		return fmt.Errorf("OVN Kubernetes config file %q doesn't exist", confFile)
	}
	return nil
}

func ovnNBDBRaftReadiness(target string) error {
	status, err := util.GetOVNDBServerInfo(15, "nb", "OVN_Northbound")
	if err != nil {
		return err
	}
	if !status.Connected {
		return fmt.Errorf("ovsdb-server managing OVN_Northbound is not in contact with a majority of its cluster")
	}
	return nil
}

func ovnSBDBRaftReadiness(target string) error {
	status, err := util.GetOVNDBServerInfo(15, "sb", "OVN_Southbound")
	if err != nil {
		return err
	}
	if !status.Connected {
		return fmt.Errorf("ovsdb-server managing OVN_Southbound is not in contact with a majority of its cluster")
	}
	return nil
}

// ReadinessProbeCommand runs readiness probes against various targets
var ReadinessProbeCommand = cli.Command{
	Name:  "readiness-probe",
	Usage: "check readiness of the specified target daemon",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "target",
			Aliases: []string{"t"},
			Usage:   "target daemon to check for readiness",
		},
	},
	Action: func(ctx *cli.Context) error {
		target := ctx.String("target")
		if err := util.SetExec(kexec.New()); err != nil {
			return err
		}
		if cbfunc, ok := callbacks[target]; ok {
			return cbfunc(target)
		}
		return fmt.Errorf("incorrect target specified")
	},
}
