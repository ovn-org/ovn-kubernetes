package ovn

import (
	"context"
	"os"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	haLeaderLockName = "ovn-kubernetes-master"
)

// HAMasterController is the object holder for managing the HA master
// cluster
type HAMasterController struct {
	kubeClient    kubernetes.Interface
	ovnController *Controller
	nodeName      string
	isLeader      bool
	leaderElector *leaderelection.LeaderElector
}

// NewHAMasterController creates a new HA Master controller
func NewHAMasterController(kubeClient kubernetes.Interface, wf *factory.WatchFactory,
	nodeName string) *HAMasterController {
	ovnController := NewOvnController(kubeClient, wf)
	return &HAMasterController{
		kubeClient:    kubeClient,
		ovnController: ovnController,
		nodeName:      nodeName,
		isLeader:      false,
		leaderElector: nil,
	}
}

// StartHAMasterController runs the replication controller
func (hacontroller *HAMasterController) StartHAMasterController() error {
	// Set up leader election process first
	rl, err := resourcelock.New(
		resourcelock.ConfigMapsResourceLock,
		config.Kubernetes.OVNConfigNamespace,
		haLeaderLockName,
		hacontroller.kubeClient.CoreV1(),
		nil,
		resourcelock.ResourceLockConfig{
			Identity:      hacontroller.nodeName,
			EventRecorder: nil,
		})

	if err != nil {
		return err
	}

	hacontrollerOnStoppedLeading := func() {
		//This node was leader and it lost the election.
		// Whenever the node transitions from leader to follower,
		// we need to handle the transition properly like clearing
		// the cache. It is better to exit for now.
		// kube will restart and this will become a follower.
		logrus.Infof("I (" + hacontroller.nodeName + ") am no longer a leader. Exiting")
		os.Exit(1)
	}

	hacontrollerNewLeader := func(nodeName string) {
		logrus.Infof(nodeName + " is the new leader")
		wasLeader := hacontroller.isLeader

		if hacontroller.nodeName == nodeName {
			// Configure as leader.
			logrus.Infof(" I (" + hacontroller.nodeName + ") won the election. In active mode")
			err = hacontroller.ConfigureAsActive(nodeName)
			if err != nil {
				logrus.Errorf(err.Error())
				panic(err.Error())
			}
			hacontroller.isLeader = true
		} else if wasLeader {
			hacontrollerOnStoppedLeading()
			// should not be reached
			panic("This should not happen.")
		} else {
			// Configure as standby.
			logrus.Infof(" I (" + hacontroller.nodeName + ") lost the election. In Standby mode")
		}
	}

	lec := leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: time.Duration(config.MasterHA.ElectionLeaseDuration) * time.Second,
		RenewDeadline: time.Duration(config.MasterHA.ElectionRenewDeadline) * time.Second,
		RetryPeriod:   time.Duration(config.MasterHA.ElectionRetryPeriod) * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {},
			OnStoppedLeading: hacontrollerOnStoppedLeading,
			OnNewLeader:      hacontrollerNewLeader,
		},
	}

	hacontroller.leaderElector, err = leaderelection.NewLeaderElector(lec)
	if err != nil {
		return err
	}
	go hacontroller.leaderElector.Run(context.Background())

	return nil
}

// ConfigureAsActive configures the node as active.
func (hacontroller *HAMasterController) ConfigureAsActive(masterNodeName string) error {
	// run the cluster controller to init the master
	err := hacontroller.ovnController.StartClusterMaster(hacontroller.nodeName)
	if err != nil {
		return err
	}

	return hacontroller.ovnController.Run()
}
