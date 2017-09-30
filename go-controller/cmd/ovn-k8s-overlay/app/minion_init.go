package app

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/urfave/cli"
)

// InitMinionCmd initializes k8s minion node.
var InitMinionCmd = cli.Command{
	Name:  "minion-init",
	Usage: "Initialize k8s minion node",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "cluster-ip-subnet",
			Usage: "The cluster wide larger subnet of private ip addresses.",
		},
		cli.StringFlag{
			Name:  "minion-switch-subnet",
			Usage: "The smaller subnet just for this master.",
		},
		cli.StringFlag{
			Name:  "node-name",
			Usage: "A unique node name.",
		},
		cli.StringFlag{
			Name:  "nb-privkey",
			Usage: "The private key used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  "nb-cert",
			Usage: "The certificate used for northbound API SSL connections.",
		},
		cli.StringFlag{
			Name:  "nb-cacert",
			Usage: "The CA certificate used for northbound API SSL connections.",
		},
	},
	Action: func(context *cli.Context) error {
		if err := initMinion(context); err != nil {
			return fmt.Errorf("failed init minion: %v", err)
		}
		return nil
	},
}

func initMinion(context *cli.Context) error {
	minionSwitchSubnet := context.String("minion-switch-subnet")
	if minionSwitchSubnet == "" {
		return fmt.Errorf("argument --minion-switch-subnet should be non-null")
	}

	clusterIPSubnet := context.String("cluster-ip-subnet")
	if clusterIPSubnet == "" {
		return fmt.Errorf("argument --cluster-ip-subnet should be non-null")
	}

	nodeName := context.String("node-name")
	if nodeName == "" {
		return fmt.Errorf("argument --node-name should be non-null")
	}

	// Fetch config file to override default values.
	config.FetchConfig()

	// Fetch OVN central database's ip address.
	err := fetchOVNNB(context)
	if err != nil {
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
		cniConfigContext := struct{ MinionSwitchSubnet string }{
			MinionSwitchSubnet: minionSwitchSubnet,
		}
		cniConfigBytes, err := parseTemplate(cniConfigTemplate, cniConfigContext)
		if err != nil {
			return fmt.Errorf("Failed to parse cniConfigTemplate")
		}

		f, err := os.OpenFile(cniConf, os.O_CREATE|os.O_WRONLY, 0666)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(cniConfigBytes)
		if err != nil {
			return err
		}
	}

	return createManagementPort(nodeName, minionSwitchSubnet, clusterIPSubnet)
}
