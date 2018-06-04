package cni

// contains code for cnishim - one that gets called as the cni plugin
// This does not do the real cni work. This is just the client to the cniserver
// that does the real work.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ns"
)

type cniPlugin struct {
	socketPath string
	hostNS     ns.NetNS
}

func NewCNIPlugin(socketPath string, hostNS ns.NetNS) *cniPlugin {
	return &cniPlugin{socketPath: socketPath, hostNS: hostNS}
}

// Create and fill a CNIRequest with this plugin's environment and stdin which
// contain the CNI variables and configuration
func newCNIRequest(args *skel.CmdArgs) *CNIRequest {
	envMap := make(map[string]string)
	for _, item := range os.Environ() {
		idx := strings.Index(item, "=")
		if idx > 0 {
			envMap[strings.TrimSpace(item[:idx])] = item[idx+1:]
		}
	}

	return &CNIRequest{
		Env:    envMap,
		Config: args.StdinData,
	}
}

// Send a CNI request to the CNI server via JSON + HTTP over a root-owned unix socket,
// and return the result
func (p *cniPlugin) doCNI(url string, req *CNIRequest) ([]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CNI request %v: %v", req, err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(proto, addr string) (net.Conn, error) {
				return net.Dial("unix", p.socketPath)
			},
		},
	}

	var resp *http.Response
	err = p.hostNS.Do(func(ns.NetNS) error {
		resp, err = client.Post(url, "application/json", bytes.NewReader(data))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send CNI request: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read CNI result: %v", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("CNI request failed with status %v: '%s'", resp.StatusCode, string(body))
	}

	return body, nil
}

func (p *cniPlugin) CmdAdd(args *skel.CmdArgs) error {
	req := newCNIRequest(args)

	body, err := p.doCNI("http://dummy/", req)
	if err != nil {
		return err
	}

	result, err := current.NewResult(body)
	if err != nil {
		return fmt.Errorf("failed to unmarshal response '%s': %v", string(body), err)
	}

	return result.Print()
}

func (p *cniPlugin) CmdDel(args *skel.CmdArgs) error {
	_, err := p.doCNI("http://dummy/", newCNIRequest(args))
	return err
}
