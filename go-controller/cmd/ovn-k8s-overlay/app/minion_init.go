package app

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/ovn"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli"
)

const (
	argMinionSwitchSubnet string = "minion-switch-subnet"
)

// InitMinionCmd initializes k8s minion node.
var InitMinionCmd = cli.Command{
	Name:  "minion-init",
	Usage: "Initialize k8s minion node",
	Flags: newFlags(
		cli.StringFlag{
			Name:  argMinionSwitchSubnet,
			Usage: "The smaller subnet just for this minion.",
		},
	),
	Action: func(context *cli.Context) error {
		if err := initMinion(context); err != nil {
			return fmt.Errorf("failed init minion: %v", err)
		}
		return nil
	},
}

func initMinion(context *cli.Context) error {
	minionSwitchSubnet, err := util.StringArg(context, argMinionSwitchSubnet)
	if err != nil {
		return err
	}

	clusterIPSubnet, err := util.StringArg(context, argClusterIPSubnet)
	if err != nil {
		return err
	}

	nodeName, err := util.StringArg(context, argNodeName)
	if err != nil {
		return err
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Fetch OVN central database's ip address.
	if err := fetchOVNNB(context); err != nil {
		return err
	}

	if runtime.GOOS != "win32" {
		cniPluginPath, err := exec.LookPath(config.CniPlugin)
		if err != nil {
			return fmt.Errorf("No cni plugin %v found", config.CniPlugin)
		}

		_, err = os.Stat(config.CniLinkPath)
		if err != nil && !os.IsExist(err) {
			err = os.MkdirAll(config.CniLinkPath, os.ModeDir)
			if err != nil {
				return err
			}
		}
		cniFile := config.CniLinkPath + "/ovn_cni"
		_, err = os.Stat(cniFile)
		if err != nil && !os.IsExist(err) {
			_, err = exec.Command("ln", "-s", cniPluginPath, cniFile).CombinedOutput()
			if err != nil {
				return err
			}
		}

		_, err = os.Stat(config.CniConfPath)
		if err != nil && !os.IsExist(err) {
			err = os.MkdirAll(config.CniConfPath, os.ModeDir)
			if err != nil {
				return err
			}
		}

		// Always create the CNI config for consistency.
		cniConf := config.CniConfPath + "/10-net.conf"
		f, err := os.OpenFile(cniConf, os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write([]byte("{\"name\":\"ovn-cni\", \"type\":\"ovn_cni\"}"))
		if err != nil {
			return err
		}
	}

	return ovn.CreateManagementPort(nodeName, minionSwitchSubnet, clusterIPSubnet)
}
