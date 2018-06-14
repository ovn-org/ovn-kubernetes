package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/containernetworking/cni/pkg/types"
)

// OVNNetConf is the Go structure representing configuration for the
// ovn-kubernetes CNI plugin
type OVNNetConf struct {
	types.NetConf

	ConfigFilePath string `json:"configFilePath,omitempty"`
}

// WriteCNIConfig writes a CNI JSON config file to directory given by global config
func WriteCNIConfig(configFilePath string) error {
	bytes, err := json.Marshal(&OVNNetConf{
		NetConf: types.NetConf{
			CNIVersion: "0.3.1",
			Name:       "ovn-kubernetes",
			Type:       CNI.Plugin,
		},
		ConfigFilePath: configFilePath,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal CNI config JSON: %v", err)
	}

	// Install the CNI config file after all initialization is done
	// MkdirAll() returns no error if the path already exists
	err = os.MkdirAll(CNI.ConfDir, os.ModeDir)
	if err != nil {
		return err
	}

	// Always create the CNI config for consistency.
	confFile := filepath.Join(CNI.ConfDir, "10-ovn-kubernetes.conf")

	var f *os.File
	f, err = ioutil.TempFile(CNI.ConfDir, "ovnkube-")
	if err != nil {
		return err
	}

	_, err = f.Write(bytes)
	if err != nil {
		return err
	}
	err = f.Close()
	if err != nil {
		return err
	}

	return os.Rename(f.Name(), confFile)
}

// ReadCNIConfig unmarshals a CNI JSON config into an OVNNetConf structure
func ReadCNIConfig(bytes []byte) (*OVNNetConf, error) {
	conf := &OVNNetConf{}
	if err := json.Unmarshal(bytes, conf); err != nil {
		return nil, err
	}
	return conf, nil
}
