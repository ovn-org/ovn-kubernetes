package multi_homing

import (
	"context"
)

type Controller interface {
	Start(identity string, ctx context.Context, cancel context.CancelFunc) error
	Run(ctx context.Context) error
}

//
//type MultiHomingController struct {
//	nadInfo          NetAttachDefInfo
//	netConf          ovncnitypes.NetConf
//	topologyExecutor Topology
//}
//
//func (mhc *MultiHomingController) Run(ctx context.Context) error {
//	klog.Infof("Starting all the Watchers for network %s...", mhc.nadInfo.NetName)
//	start := time.Now()
//
//	if err := mhc.WatchNodes(); err != nil {
//		return err
//	}
//
//	if err := mhc.WatchPods(); err != nil {
//		return err
//	}
//	klog.Infof("Completing all the Watchers for network %s took %v", mhc.nadInfo.NetName, time.Since(start))
//
//	return mhc.topologyExecutor.Setup()
//}
//
//func (mhc *MultiHomingController) WatchNodes() error {
//	nodeHandler, err := mhc.WatchResource(oc.retryNodes)
//	if err == nil {
//		mhc.nodeHandler = nodeHandler
//	}
//	return err
//}
//
//func (mhc *MultiHomingController) WatchPods() error {
//	podHandler, err := oc.WatchResource(oc.retryPods)
//	if err == nil {
//		mhc.podHandler = podHandler
//	}
//	return err
//}
//
//type Topology interface {
//	Setup() error
//}
//
//type RoutedTopology struct {
//}
//
//func (rt *RoutedTopology) Setup() error {
//	return fmt.Errorf("not implemented yet")
//}
//
//type SwitchedTopology struct {
//}
//
//func (st *SwitchedTopology) Setup() error {
//	return fmt.Errorf("not implemented yet")
//}
//
//type UnderlayTopology struct {
//}
//
//func (ut *UnderlayTopology) Setup() error {
//	return fmt.Errorf("not implemented yet")
//}
