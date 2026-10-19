// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package kubevirt

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/nbdb"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestDefaultNetworkMigrationRouting(t *testing.T) {
	source := runningKubevirtPod(0)
	source.Name, source.UID, source.Spec.NodeName = "source", "source", "source-node"
	source.Annotations[kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation] = ""
	source.Labels[kubevirtv1.NodeNameLabel] = source.Spec.NodeName
	target := source.DeepCopy()
	target.Name, target.UID, target.Spec.NodeName = "target", "target", "target-node"
	target.CreationTimestamp = metav1.NewTime(source.CreationTimestamp.Add(time.Minute))
	readyTarget := target.DeepCopy()
	readyTarget.Annotations[kubevirtv1.MigrationTargetReadyTimestamp] = "ready"
	preCopyTarget := target.DeepCopy()
	preCopyTarget.Labels[kubevirtv1.NodeNameLabel] = target.Spec.NodeName
	completedSource := source.DeepCopy()
	completedSource.Status.Phase = corev1.PodSucceeded
	failedTarget := target.DeepCopy()
	failedTarget.Status.Phase = corev1.PodFailed
	completedTarget := readyTarget.DeepCopy()
	completedTarget.Status.Phase = corev1.PodSucceeded
	initializingSource := source.DeepCopy()
	delete(initializingSource.Labels, kubevirtv1.NodeNameLabel)
	unscheduledSource := source.DeepCopy()
	unscheduledSource.Spec.NodeName = ""
	unannotatedSource := source.DeepCopy()
	delete(unannotatedSource.Annotations, kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation)
	hostNetworkSource := source.DeepCopy()
	hostNetworkSource.Spec.HostNetwork = true
	oldFailedTarget := failedTarget.DeepCopy()
	oldFailedTarget.Name, oldFailedTarget.UID = "old-target", "old-target"
	oldFailedTarget.CreationTimestamp = metav1.NewTime(source.CreationTimestamp.Add(-time.Minute))
	nextTarget := target.DeepCopy()
	nextTarget.Name, nextTarget.UID = "next-target", "next-target"
	nextTarget.CreationTimestamp = metav1.NewTime(target.CreationTimestamp.Add(time.Minute))
	brokenSource := source.DeepCopy()
	delete(brokenSource.Annotations, kubevirtv1.DomainAnnotation)

	for _, tc := range []struct {
		name      string
		pods      []*corev1.Pod
		reconcile []bool
		wantError bool
	}{
		{name: "initial startup", pods: []*corev1.Pod{&source}, reconcile: []bool{true}},
		{name: "initializing VM", pods: []*corev1.Pod{initializingSource}, reconcile: []bool{true}},
		{name: "unscheduled VM", pods: []*corev1.Pod{unscheduledSource}, reconcile: []bool{false}},
		{name: "unannotated VM", pods: []*corev1.Pod{unannotatedSource}, reconcile: []bool{false}},
		{name: "host network VM", pods: []*corev1.Pod{hostNetworkSource}, reconcile: []bool{false}},
		{name: "migration in progress", pods: []*corev1.Pod{&source, target}, reconcile: []bool{true, false}},
		{name: "source without readiness label", pods: []*corev1.Pod{initializingSource, target}, reconcile: []bool{true, false}},
		{name: "pre-copy ready through node label", pods: []*corev1.Pod{&source, preCopyTarget}, reconcile: []bool{false, true}},
		{name: "post-copy ready through timestamp", pods: []*corev1.Pod{&source, readyTarget}, reconcile: []bool{false, true}},
		{name: "completed source", pods: []*corev1.Pod{completedSource, readyTarget}, reconcile: []bool{false, true}},
		{name: "deleted source", pods: []*corev1.Pod{readyTarget}, reconcile: []bool{true}},
		{name: "completed source before target ready", pods: []*corev1.Pod{completedSource, target}, reconcile: []bool{false, true}},
		{name: "deleted source before target ready", pods: []*corev1.Pod{target}, reconcile: []bool{true}},
		{name: "failed migration", pods: []*corev1.Pod{&source, failedTarget}, reconcile: []bool{true, false}},
		{name: "failed migration without source readiness label", pods: []*corev1.Pod{initializingSource, failedTarget}, reconcile: []bool{true, false}},
		{name: "completed target with surviving source", pods: []*corev1.Pod{&source, completedTarget}, reconcile: []bool{true, false}},
		{name: "all pods completed", pods: []*corev1.Pod{completedSource, completedTarget}, reconcile: []bool{false, false}},
		{name: "only completed target remains", pods: []*corev1.Pod{completedTarget}, reconcile: []bool{false}},
		{name: "retry after failed migration", pods: []*corev1.Pod{oldFailedTarget, &source, readyTarget}, reconcile: []bool{false, false, true}},
		{name: "second migration in progress", pods: []*corev1.Pod{completedSource, readyTarget, nextTarget}, reconcile: []bool{false, true, false}},
		{name: "too many living pods", pods: []*corev1.Pod{&source, target, nextTarget}, wantError: true},
		{name: "missing VM identity", pods: []*corev1.Pod{brokenSource}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, config.PrepareTestConfig())
			config.IPv4Mode, config.IPv6Mode = false, true
			wf, err := factory.NewOVNKubeControllerWatchFactory(util.GetOVNClientset().GetOVNKubeControllerClientset(), "node")
			require.NoError(t, err)
			t.Cleanup(wf.Shutdown)
			pods := make([]*corev1.Pod, len(tc.pods))
			for i, original := range tc.pods {
				pod := original.DeepCopy()
				pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
					IPs: ovntest.MustParseIPNets("fd00:1::8/64"), MAC: ovntest.MustParseMAC("0a:58:0a:f4:01:08"),
				}, types.DefaultNetworkName)
				require.NoError(t, err)
				pods[i] = pod
				require.NoError(t, wf.PodCoreInformer().Informer().GetStore().Add(pod))
			}
			for i, pod := range pods {
				for _, zone := range []string{OvnLocalZone, OvnRemoteZone} {
					t.Run(pod.Name+"/"+zone, func(t *testing.T) {
						// Returning to the subnet owner removes the old VM route.
						// Non-active or ineligible pods must leave that route intact.
						subnets := ovntest.MustParseIPNets("fd00:1::/64")
						nodeName := pod.Spec.NodeName
						if nodeName == "" {
							nodeName = "node"
						}
						nodeAnnotations, err := util.UpdateNodeHostSubnetAnnotation(nil, subnets, types.DefaultNetworkName)
						require.NoError(t, err)
						require.NoError(t, wf.NodeCoreInformer().Informer().GetStore().Add(&corev1.Node{
							ObjectMeta: metav1.ObjectMeta{Name: nodeName, Annotations: nodeAnnotations},
						}))
						lsManager := logicalswitchmanager.NewLogicalSwitchManager()
						require.NoError(t, lsManager.AddOrUpdateSwitch(nodeName, subnets, nil, nil))
						route := &nbdb.LogicalRouterStaticRoute{
							UUID: "old-route", IPPrefix: "fd00:1::8", Nexthop: "fd00:2::8",
							Policy: ptr.To(nbdb.LogicalRouterStaticRoutePolicyDstIP),
							ExternalIDs: map[string]string{
								VirtualMachineExternalIDsKey: vmName,
								NamespaceExternalIDsKey:      pod.Namespace,
								OvnZoneExternalIDKey:         zone,
							},
						}
						nbClient, dbCtx, err := libovsdbtest.NewNBTestHarness(libovsdbtest.TestSetup{NBData: []libovsdbtest.TestData{
							route, &nbdb.LogicalRouter{UUID: "router", Name: types.OVNClusterRouter, StaticRoutes: []string{route.UUID}},
						}}, nil)
						require.NoError(t, err)
						t.Cleanup(dbCtx.Cleanup)
						localNodeName := nodeName
						if zone == OvnRemoteZone {
							localNodeName = "other-node"
						}
						err = EnsureDefaultNetworkForMigratablePod(wf, nbClient, lsManager, localNodeName, pod, nil)
						if tc.wantError {
							require.Error(t, err)
							return
						}
						require.NoError(t, err)
						routeRemoved := func() bool {
							var routes []nbdb.LogicalRouterStaticRoute
							return nbClient.List(context.Background(), &routes) == nil && len(routes) == 0
						}
						if tc.reconcile[i] {
							require.Eventually(t, routeRemoved, time.Second, 10*time.Millisecond)
						} else {
							require.Never(t, routeRemoved, 50*time.Millisecond, 10*time.Millisecond)
						}
					})
				}
			}
		})
	}
}
