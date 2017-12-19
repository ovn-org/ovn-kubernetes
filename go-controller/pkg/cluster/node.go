package cluster

import (
	"fmt"
	"os"

	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/config"
	"github.com/openvswitch/ovn-kubernetes/go-controller/pkg/util"
)

const (
	windowsOS = "windows"
)

// StartClusterNode learns the subnet assigned to it by the master controller
// and calls the SetupNode script which establishes the logical switch
func StartNode(controller OvnClusterController, name string) error {
	if err := controller.NodeInit(name); err != nil {
		return err
	}

	if err := setupOVNNode(name, config.Kubernetes.APIServer, config.Kubernetes.Token); err != nil {
		return err
	}

	err := util.RestartOvnController()
	if err != nil {
		return err
	}

	if err := controller.NodeStart(name); err != nil {
		return err
	}

	// Install the CNI config file after all initialization is done
	// MkdirAll() returns no error if the path already exists
	err = os.MkdirAll(config.CNI.ConfDir, os.ModeDir)
	if err != nil {
		return err
	}

	// Always create the CNI config for consistency.
	cniConf := config.CNI.ConfDir + "/10-ovn-kubernetes.conf"

	var f *os.File
	f, err = os.OpenFile(cniConf, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	confJSON := fmt.Sprintf("{\"name\":\"ovn-kubernetes\", \"type\":\"%s\"}", config.CNI.Plugin)
	_, err = f.Write([]byte(confJSON))

	return err
}
