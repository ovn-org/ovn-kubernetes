package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
)

// WriteCNIConfig writes a CNI JSON config file to directory given by global config
func WriteCNIConfig(ConfDir string, fileName string) error {
	bytes, err := json.Marshal(&types.NetConf{
		CNIVersion: "0.4.0",
		Name:       "ovn-kubernetes",
		Type:       CNI.Plugin,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal CNI config JSON: %v", err)
	}

	// Install the CNI config file after all initialization is done
	// MkdirAll() returns no error if the path already exists
	err = os.MkdirAll(ConfDir, os.ModeDir)
	if err != nil {
		return err
	}

	// Always create the CNI config for consistency.
	confFile := filepath.Join(ConfDir, fileName)

	var f *os.File
	f, err = ioutil.TempFile(ConfDir, "ovnkube-")
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

// ReadCNIConfig unmarshals a CNI JSON config into an NetConf structure
func ReadCNIConfig(bytes []byte) (*ovntypes.NetConf, error) {
	conf := &ovntypes.NetConf{}
	if err := json.Unmarshal(bytes, conf); err != nil {
		return nil, err
	}
	if conf.RawPrevResult != nil {
		if err := version.ParsePrevResult(&conf.NetConf); err != nil {
			return nil, err
		}
	}
	return conf, nil
}
