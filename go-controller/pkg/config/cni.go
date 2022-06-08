package config

import (
	"bytes"
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
// if the file doesn't already exist, or is different than the content that would
// be written.
func WriteCNIConfig() error {
	netConf := &ovntypes.NetConf{
		NetConf: types.NetConf{
			CNIVersion: "0.4.0",
			Name:       "ovn-kubernetes",
			Type:       CNI.Plugin,
		},
		LogFile:           Logging.CNIFile,
		LogLevel:          fmt.Sprintf("%d", Logging.Level),
		LogFileMaxSize:    Logging.LogFileMaxSize,
		LogFileMaxBackups: Logging.LogFileMaxBackups,
		LogFileMaxAge:     Logging.LogFileMaxAge,
	}

	newBytes, err := json.Marshal(netConf)
	if err != nil {
		return fmt.Errorf("failed to marshal CNI config JSON: %v", err)
	}

	confFile := filepath.Join(CNI.ConfDir, CNIConfFileName)
	if existingBytes, err := os.ReadFile(confFile); err == nil {
		if bytes.Equal(newBytes, existingBytes) {
			// No changes; do nothing
			return nil
		}
	}

	// Install the CNI config file after all initialization is done
	// MkdirAll() returns no error if the path already exists
	if err := os.MkdirAll(CNI.ConfDir, os.ModeDir); err != nil {
		return err
	}

	var f *os.File
	f, err = ioutil.TempFile(CNI.ConfDir, "ovnkube-")
	if err != nil {
		return err
	}

	if _, err := f.Write(newBytes); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
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
