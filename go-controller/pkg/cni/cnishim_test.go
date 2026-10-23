// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

func TestCmdAdd_PrivilegedMode(t *testing.T) {
	// Setup: CNI server returns non-nil Result
	p := &Plugin{}

	// Patch doCNI to return a response with non-nil Result that indicates server wired
	result := &current.Result{Interfaces: []*current.Interface{{Name: "serverWired"}}}

	kubeAuth := &KubeAPIAuth{
		Kubeconfig:       config.Kubernetes.Kubeconfig,
		KubeAPIServer:    config.Kubernetes.APIServer,
		KubeAPIToken:     config.Kubernetes.Token,
		KubeAPITokenFile: config.Kubernetes.TokenFile,
	}
	body, _ := json.Marshal(&Response{
		Result:    result,
		PodIFInfo: &PodInterfaceInfo{},
		KubeAuth:  kubeAuth,
	})
	p.doCNIFunc = func(_ string, _ interface{}) ([]byte, error) {
		return body, nil
	}

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	args := &skel.CmdArgs{
		StdinData:   []byte(`{"cniVersion":"1.1.0","name":"mynet","type":"ovn-k8s-cni-overlay"}`),
		ContainerID: "cid",
		Netns:       "/var/run/netns/test",
		IfName:      "eth0",
	}
	err := p.CmdAdd(args)
	require.NoError(t, err)
	w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()
	var got, want map[string]any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	expected := `{
    "cniVersion": "1.1.0",
    "interfaces": [
        {
            "name": "serverWired"
        }
    ]
}`
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatalf("failed to unmarshal expected: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected CmdAdd output:\n got=%v\nwant=%v", got, want)
	}
}

func TestCmdAdd_UnprivilegedMode(t *testing.T) {
	withCNIEnv(t, func() {
		p := &Plugin{}
		err := config.PrepareTestConfig()
		require.NoError(t, err, "failed to prepare test config")
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true

		setupShimOVS(t)
		stub := &podRequestInterfaceOpsStub{}
		podRequestInterfaceOps = stub
		defer func() { podRequestInterfaceOps = &defaultPodRequestInterfaceOps{} }()
		pr := PodRequest{
			PodNamespace: "foo-ns",
			PodName:      "bar-pod",
			timestamp:    time.Time{},
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pr.PodName,
				Namespace: pr.PodNamespace,
				Annotations: map[string]string{
					"k8s.ovn.org/pod-networks": `{"default":{"ip_addresses":["100.10.10.3/24","fd44::33/64"],"mac_address":
"0a:58:fd:98:00:01", "role":"infrastructure-locked"}, "foo-ns/meganet":{"ip_addresses":["10.10.10.30/24","fd10::3/64"],
"mac_address":"02:03:04:05:06:07", "role":"primary"}}`,
				},
			},
		}
		defaultPodNADAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, "default")
		require.NoError(t, err)
		udnPodNADAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, "foo-ns/meganet")
		require.NoError(t, err)

		// Fake Response: no Result, but PrimaryUDNPodInfo + PrimaryUDNPodRequest populated
		resp := &Response{
			Result: nil,
			PodIFInfo: &PodInterfaceInfo{
				PodAnnotation: *defaultPodNADAnnotation,
				MTU:           1400,
				NetName:       "default",
				NADKey:        "foo-ns/default",
			},
			PrimaryUDNPodInfo: &PodInterfaceInfo{
				PodAnnotation: *udnPodNADAnnotation,
				MTU:           1400,
				NetName:       "tenantred",
				NADKey:        "foo-ns/meganet",
			},
			PrimaryUDNPodReq: &PodRequest{
				PodNamespace: "default",
				PodName:      "testpod",
				IfName:       "dummy1",
			},
			KubeAuth: &KubeAPIAuth{},
		}

		body, _ := json.Marshal(resp)

		// Mock doCNI to return our fake response
		p.doCNIFunc = func(_ string, _ interface{}) ([]byte, error) {
			return body, nil
		}

		// Capture stdout (CmdAdd prints)
		oldStdout := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		defer func() {
			os.Stdout = oldStdout
		}()

		args := &skel.CmdArgs{
			StdinData:   []byte(`{"cniVersion":"1.1.0","name":"mynet","type":"ovn-k8s-cni-overlay"}`),
			ContainerID: "cid",
			Netns:       "/var/run/netns/test",
			IfName:      "eth0",
		}
		err = p.CmdAdd(args)
		require.NoError(t, err)
		require.Len(t, stub.configureOVSClients, 2)
		require.NotNil(t, stub.configureOVSClients[0])
		require.Same(t, stub.configureOVSClients[0], stub.configureOVSClients[1])
		require.False(t, stub.configureOVSClients[0].Connected())

		// Collect stdout
		w.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		output := buf.String()

		// Unmarshal output for comparison
		var got map[string]any
		if err := json.Unmarshal([]byte(output), &got); err != nil {
			t.Fatalf("failed to unmarshal output: %v", err)
		}

		// Expected output includes both interfaces wired by CNIShim
		expected := `{
  "cniVersion": "1.1.0",
  "interfaces": [
    {
      "name": "host_eth0"
    },
    {
      "name": "eth0",
      "sandbox": "/var/run/netns/test-ns_test-pod"
    },
    {
      "name": "host_dummy1"
    },
    {
      "name": "dummy1",
      "sandbox": "/var/run/netns/default_testpod"
    }
  ],
  "ips": [
    { "address": "100.10.10.3/24", "interface": 1 },
    { "address": "fd44::33/64", "interface": 1 },
    { "address": "10.10.10.30/24", "interface": 3 },
    { "address": "fd10::3/64", "interface": 3 }
  ]
}`

		var want map[string]any
		if err := json.Unmarshal([]byte(expected), &want); err != nil {
			t.Fatalf("failed to unmarshal expected: %v", err)
		}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unexpected CmdAdd output:\n got=%v\nwant=%v", got, want)
		}
	})
}

func TestCmdDel_PrivilegedMode(t *testing.T) {
	p := &Plugin{}

	// Patch doCNI to return a response with non-nil Result that indicates server handled delete
	result := &current.Result{}

	kubeAuth := &KubeAPIAuth{
		Kubeconfig:       config.Kubernetes.Kubeconfig,
		KubeAPIServer:    config.Kubernetes.APIServer,
		KubeAPIToken:     config.Kubernetes.Token,
		KubeAPITokenFile: config.Kubernetes.TokenFile,
	}
	body, _ := json.Marshal(&Response{
		Result:    result,
		PodIFInfo: &PodInterfaceInfo{},
		KubeAuth:  kubeAuth,
	})

	// Mock a doCNI that succeeds for CmdDel
	p.doCNIFunc = func(_ string, _ interface{}) ([]byte, error) {
		return body, nil
	}

	args := &skel.CmdArgs{
		StdinData:   []byte(`{"cniVersion":"1.1.0","name":"mynet","type":"ovn-k8s-cni-overlay"}`),
		ContainerID: "cid",
		Netns:       "/var/run/netns/test",
		IfName:      "eth0",
	}
	err := p.CmdDel(args)
	require.NoError(t, err)
}

func TestCmdDel_UnprivilegedMode(t *testing.T) {
	withCNIEnv(t, func() {
		p := &Plugin{}
		setupShimOVS(t)
		config.OVNKubernetesFeature.EnableMultiNetwork = true
		config.OVNKubernetesFeature.EnableNetworkSegmentation = true

		stub := &podRequestInterfaceOpsStub{}
		podRequestInterfaceOps = stub
		defer func() { podRequestInterfaceOps = &defaultPodRequestInterfaceOps{} }()

		resp := &Response{
			Result: nil,
			PodIFInfo: &PodInterfaceInfo{
				NetName: "default",
				NADKey:  "foo-ns/default",
			},
			PrimaryUDNPodInfo: &PodInterfaceInfo{
				NetName: "tenantred",
				NADKey:  "foo-ns/meganet",
			},
			PrimaryUDNPodReq: &PodRequest{
				PodNamespace: "default",
				PodName:      "testpod",
				IfName:       "dummy1",
			},
			KubeAuth: &KubeAPIAuth{},
		}

		body, _ := json.Marshal(resp)
		p.doCNIFunc = func(_ string, _ interface{}) ([]byte, error) {
			return body, nil
		}

		args := &skel.CmdArgs{
			StdinData:   []byte(`{"cniVersion":"1.1.0","name":"mynet","type":"ovn-k8s-cni-overlay"}`),
			ContainerID: "cid",
			Netns:       "/var/run/netns/test",
			IfName:      "eth0",
		}
		err := p.CmdDel(args)
		require.NoError(t, err)
		require.Len(t, stub.unconfigureOVSClients, 1)
		require.NotNil(t, stub.unconfigureOVSClients[0])
		require.False(t, stub.unconfigureOVSClients[0].Connected())

		if len(stub.unconfiguredInterfaces) <= 0 {
			t.Fatalf("no unconfigured interfaces found")
		}
	})
}

func setupShimOVS(t *testing.T) {
	t.Helper()
	ovsClient, _ := newShimOVSTestServer(t, []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{"br_int"}},
		&vswitchd.Bridge{UUID: "br_int", Name: "br-int"},
	})
	oldRunDir := config.OvsPaths.RunDir
	config.OvsPaths.RunDir = t.TempDir()
	t.Cleanup(func() { config.OvsPaths.RunDir = oldRunDir })
	require.NoError(t, os.Symlink(strings.TrimPrefix(ovsClient.CurrentEndpoint(), "unix:"),
		filepath.Join(config.OvsPaths.RunDir, "db.sock")))
}

func TestShimOVSClientLifecycle(t *testing.T) {
	for _, cmd := range []command{CNIAdd, CNIDel} {
		for _, scenario := range []string{"DPU host", "OVS unavailable", "interface operation fails"} {
			t.Run(string(cmd)+"/"+scenario, func(t *testing.T) {
				withCNIEnv(t, func() {
					require.NoError(t, os.Setenv("CNI_COMMAND", string(cmd)))
					oldRunDir := config.OvsPaths.RunDir
					config.OvsPaths.RunDir = t.TempDir()
					t.Cleanup(func() { config.OvsPaths.RunDir = oldRunDir })
					stub := &podRequestInterfaceOpsStub{}
					oldOps := podRequestInterfaceOps
					podRequestInterfaceOps = stub
					t.Cleanup(func() { podRequestInterfaceOps = oldOps })
					if scenario == "interface operation fails" {
						setupShimOVS(t)
						stub.configureErr = errors.New("interface operation failed")
						stub.unconfigureErr = stub.configureErr
					}
					body, err := json.Marshal(&Response{
						PodIFInfo: &PodInterfaceInfo{IsDPUHostMode: scenario == "DPU host"},
						KubeAuth:  &KubeAPIAuth{},
					})
					require.NoError(t, err)
					p := &Plugin{doCNIFunc: func(_ string, _ interface{}) ([]byte, error) { return body, nil }}
					args := &skel.CmdArgs{
						StdinData: []byte(`{"cniVersion":"1.1.0","name":"mynet","type":"ovn-k8s-cni-overlay"}`),
					}
					var clients []libovsdbclient.Client
					if cmd == CNIAdd {
						err = p.CmdAdd(args)
						clients = stub.configureOVSClients
					} else {
						err = p.CmdDel(args)
						clients = stub.unconfigureOVSClients
					}
					switch scenario {
					case "DPU host":
						require.NoError(t, err)
						require.Equal(t, []libovsdbclient.Client{nil}, clients)
					case "OVS unavailable":
						require.ErrorContains(t, err, "failed to initialize OVS client")
						require.Empty(t, clients)
					case "interface operation fails":
						require.ErrorContains(t, err, "interface operation failed")
						require.Len(t, clients, 1)
						require.NotNil(t, clients[0])
						require.False(t, clients[0].Connected())
					}
				})
			})
		}
	}
}

func TestCmdGC(t *testing.T) {
	p := &Plugin{}
	args := &skel.CmdArgs{
		StdinData: []byte(`{"cniVersion":"1.1.0","name":"mynet","type":"ovn-k8s-cni-overlay"}`),
	}

	err := p.CmdGC(args)
	require.NoError(t, err)
}

func withCNIEnv(t *testing.T, fn func()) {
	t.Helper()

	vars := map[string]string{
		"CNI_COMMAND":     "ADD",
		"CNI_CONTAINERID": "dummy-containerid",
		"CNI_NETNS":       "/var/run/netns/testns",
		"CNI_IFNAME":      "eth0",
		"CNI_PATH":        "/opt/cni/bin",
		"CNI_ARGS":        "K8S_POD_NAME=test-pod;K8S_POD_NAMESPACE=test-ns;K8S_POD_UID=12345",
	}

	old := make(map[string]string, len(vars))
	for k := range vars {
		old[k] = os.Getenv(k)
		_ = os.Setenv(k, vars[k])
	}

	fn()

	for k, v := range old {
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
}
