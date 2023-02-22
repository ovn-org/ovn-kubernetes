package cni

// contains code for cnishim - one that gets called as the cni Plugin
// This does not do the real cni work. This is just the client to the cniserver
// that does the real work.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
	kapi "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

// Plugin is the structure to hold the endpoint information and the corresponding
// functions to use it
type Plugin struct {
	socketPath string
}

// NewCNIPlugin creates the internal Plugin object
func NewCNIPlugin(socketPath string) *Plugin {
	if len(socketPath) == 0 {
		socketPath = serverSocketPath
	}
	return &Plugin{socketPath: socketPath}
}

// Create and fill a Request with this Plugin's environment and stdin which
// contain the CNI variables and configuration
func newCNIRequest(args *skel.CmdArgs) *Request {
	envMap := make(map[string]string)
	for _, item := range os.Environ() {
		idx := strings.Index(item, "=")
		if idx > 0 {
			envMap[strings.TrimSpace(item[:idx])] = item[idx+1:]
		}
	}

	return &Request{
		Env:    envMap,
		Config: args.StdinData,
	}
}

// Send a CNI request to the CNI server via JSON + HTTP over a root-owned unix socket,
// and return the result
func (p *Plugin) doCNI(url string, req interface{}) ([]byte, error) {
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

	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
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

func setupLogging(conf *ovntypes.NetConf) {
	var err error
	var level klog.Level

	if conf.LogLevel != "" {
		if err = level.Set(conf.LogLevel); err != nil {
			klog.Warningf("Failed to set klog log level to %s: %v", conf.LogLevel, err)
		}
	}
	if conf.LogFile != "" {
		klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
		klog.InitFlags(klogFlags)
		if err := klogFlags.Set("logtostderr", "false"); err != nil {
			klog.Warningf("Error setting klog logtostderr: %v", err)
		}
		if err := klogFlags.Set("alsologtostderr", "true"); err != nil {
			klog.Warningf("Error setting klog alsologtostderr: %v", err)
		}
		klog.SetOutput(&lumberjack.Logger{
			Filename:   conf.LogFile,
			MaxSize:    conf.LogFileMaxSize, // megabytes
			MaxBackups: conf.LogFileMaxBackups,
			MaxAge:     conf.LogFileMaxAge, // days
			Compress:   true,
		})
	}
}

// report the CNI request processing time to CNI server. This is used for the cni_request_duration_seconds metrics
func (p *Plugin) postMetrics(startTime time.Time, cmd command, err error) {
	elapsedTime := time.Since(startTime).Seconds()
	_, _ = p.doCNI("http://dummy/metrics", &CNIRequestMetrics{
		Command:     cmd,
		ElapsedTime: elapsedTime,
		HasErr:      err != nil,
	})
}

func shimClientsetFromConfig(auth *KubeAPIAuth) (*shimClientset, error) {
	if auth.Kubeconfig == "" && auth.KubeAPIServer == "" {
		return nil, nil
	}

	var caData []byte
	var err error
	if auth.KubeCAData != "" {
		caData, err = base64.StdEncoding.DecodeString(auth.KubeCAData)
		if err != nil {
			return nil, fmt.Errorf("failed to decode Kube API CA data: %v", err)
		}
	}
	kubeconfig := &config.KubernetesConfig{
		Kubeconfig: auth.Kubeconfig,
		APIServer:  auth.KubeAPIServer,
		Token:      auth.KubeAPIToken,
		TokenFile:  auth.KubeAPITokenFile,
		CAData:     caData,
	}

	kclient, err := util.NewKubernetesClientset(kubeconfig)
	if err != nil {
		return nil, err
	}

	return &shimClientset{
		kclient: kclient,
	}, nil
}

type shimClientset struct {
	PodInfoGetter
	kclient kubernetes.Interface
}

func (c *shimClientset) getPod(namespace, name string) (*kapi.Pod, error) {
	return c.kclient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
}

// CmdAdd is the callback for 'add' cni calls from skel
func (p *Plugin) CmdAdd(args *skel.CmdArgs) error {
	var err error

	startTime := time.Now()
	defer func() {
		p.postMetrics(startTime, CNIAdd, err)
	}()

	// read the config stdin args to obtain cniVersion
	conf, errC := config.ReadCNIConfig(args.StdinData)
	if errC != nil {
		err = fmt.Errorf("invalid stdin args %v", errC)
		return err
	}
	setupLogging(conf)

	req := newCNIRequest(args)

	body, errB := p.doCNI("http://dummy/", req)
	if errB != nil {
		err = errB
		klog.Error(err.Error())
		return err
	}

	response := &Response{}
	if err = json.Unmarshal(body, response); err != nil {
		err = fmt.Errorf("failed to unmarshal response '%s': %v", string(body), err)
		klog.Error(err.Error())
		return err
	}

	clientset, errK := shimClientsetFromConfig(response.KubeAuth)
	if errK != nil {
		err = errK
		return err
	}

	var result *current.Result
	if response.Result != nil {
		// Return the full CNI result from ovnkube-node if it configured the pod interface
		result = response.Result
	} else {
		// Use the IPAM details from ovnkube-node to configure the pod interface
		pr, err := cniRequestToPodRequest(req)
		if err != nil {
			err = fmt.Errorf("failed to create pod request: %v", err)
			klog.Error(err.Error())
			return err
		}
		defer pr.cancel()

		result, err = pr.getCNIResult(clientset, response.PodIFInfo)
		if err != nil {
			err = fmt.Errorf("failed to get CNI Result from pod interface info %v: %v", response.PodIFInfo, err)
			klog.Error(err.Error())
			return err
		}
	}

	return types.PrintResult(result, conf.CNIVersion)
}

// CmdDel is the callback for 'teardown' cni calls from skel
func (p *Plugin) CmdDel(args *skel.CmdArgs) error {
	var err error
	var body []byte
	var pr *PodRequest
	var conf *ovntypes.NetConf

	startTime := time.Now()
	defer func() {
		p.postMetrics(startTime, CNIDel, err)
		if err != nil {
			klog.Errorf(err.Error())
		}
	}()

	// read the config stdin args
	conf, err = config.ReadCNIConfig(args.StdinData)
	if err != nil {
		return err
	}
	setupLogging(conf)

	req := newCNIRequest(args)
	body, err = p.doCNI("http://dummy/", req)
	if err != nil {
		return err
	}

	response := &Response{}
	err = json.Unmarshal(body, response)
	if err != nil {
		err = fmt.Errorf("cmdDel: failed to unmarshal response '%s': %v", string(body), err)
		return err
	}

	// if Result is nil, then ovnkube-node is running in unprivileged mode so unconfigure the Interface from here.
	if response.Result == nil {
		pr, err = cniRequestToPodRequest(req)
		if err != nil {
			err = fmt.Errorf("failed to create pod request: %v", err)
			return err
		}
		defer pr.cancel()
		err = pr.UnconfigureInterface(response.PodIFInfo)
	}
	return err
}

// CmdCheck is the callback for 'checking' container's networking is as expected.
func (p *Plugin) CmdCheck(args *skel.CmdArgs) error {
	// noop...CMD check is not considered useful, and has a considerable performance impact
	// to pod bring up times with CRIO. This is due to the fact that CRIO currently calls check
	// after CNI ADD before it finishes bringing the container up
	return nil
}
