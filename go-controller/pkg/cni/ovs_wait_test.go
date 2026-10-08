// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

type bindingOVSClient struct {
	client.Client
	get   func(context.Context, model.Model) error
	watch func(string) (<-chan struct{}, func())
}

func (c *bindingOVSClient) Get(ctx context.Context, row model.Model) error {
	return c.get(ctx, row)
}

func (c *bindingOVSClient) WatchOVSInterface(name string) (<-chan struct{}, func()) {
	if c.watch != nil {
		return c.watch(name)
	}
	return nil, func() {}
}

type bindingPodGetter func() (*corev1.Pod, error)

func (g bindingPodGetter) getPod(_, _ string) (*corev1.Pod, error) {
	return g()
}

func bindingPod(t *testing.T) (*PodInterfaceInfo, *corev1.Pod) {
	t.Helper()
	info := &PodInterfaceInfo{PodUID: "uid", NADKey: types.DefaultNetworkName,
		PodAnnotation: util.PodAnnotation{MAC: net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0, 1}}}
	annotations, err := util.MarshalPodAnnotation(nil, &info.PodAnnotation, info.NADKey)
	require.NoError(t, err)
	return info, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "uid", Annotations: annotations}}
}

func TestWaitForPodInterfacePolling(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		readyAfter, cancelAfter, wantElapsed time.Duration
		initiallyMissing                     bool
		wantPodChecks                        int
		wantErr                              string
	}{
		{name: "already ready"},
		{name: "fast binding", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond, wantPodChecks: 1},
		{name: "initially absent", readyAfter: 3 * time.Millisecond, initiallyMissing: true,
			wantElapsed: 6 * time.Millisecond, wantPodChecks: 1},
		{name: "slow binding", readyAfter: 650 * time.Millisecond, wantElapsed: 800 * time.Millisecond, wantPodChecks: 4},
		{name: "deadline", readyAfter: -1, wantElapsed: 1050 * time.Millisecond, wantPodChecks: 6, wantErr: "timed out"},
		{name: "cancellation", readyAfter: -1, cancelAfter: 105 * time.Millisecond,
			wantElapsed: 105 * time.Millisecond, wantPodChecks: 1, wantErr: "canceled while"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), 1050*time.Millisecond)
				defer cancel()
				if tc.cancelAfter != 0 {
					go func() {
						time.Sleep(tc.cancelAfter)
						cancel()
					}()
				}
				reads := 0
				c := &bindingOVSClient{get: func(readCtx context.Context, row model.Model) error {
					require.Equal(t, ctx, readCtx)
					reads++
					if tc.initiallyMissing && time.Since(start) < tc.readyAfter {
						return client.ErrNotFound
					}
					iface := row.(*vswitchd.Interface)
					require.Equal(t, "pod-port", iface.Name)
					iface.ExternalIDs = map[string]string{"iface-id": "ns_pod"}
					if tc.readyAfter >= 0 && time.Since(start) >= tc.readyAfter {
						iface.ExternalIDs["ovn-installed"] = "true"
					}
					return nil
				}}
				info, pod := bindingPod(t)
				var podChecks []time.Duration
				getter := bindingPodGetter(func() (*corev1.Pod, error) {
					podChecks = append(podChecks, time.Since(start))
					return pod, nil
				})
				err := waitForPodInterface(ctx, c, info, "pod-port", "ns_pod", getter, "ns", "pod", "uid")
				if tc.wantErr != "" {
					require.ErrorContains(t, err, tc.wantErr)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, tc.wantElapsed, time.Since(start))
				require.Len(t, podChecks, tc.wantPodChecks)
				for i, elapsed := range podChecks {
					require.Equal(t, time.Duration(i)*200*time.Millisecond, elapsed)
				}
				require.LessOrEqual(t, reads, 12, "long waits must back off their cache polling")
			})
		})
	}
}

func TestWaitForPodInterfaceInvalidated(t *testing.T) {
	for _, tc := range []struct {
		name, wantErr string
	}{
		{name: "interface replaced", wantErr: "no longer active"},
		{name: "pod replaced", wantErr: "canceled old pod sandbox"},
		{name: "pod deleted", wantErr: "pod deleted"},
		{name: "annotation changed", wantErr: "pod OVN annotations changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				c := &bindingOVSClient{get: func(_ context.Context, row model.Model) error {
					iface := row.(*vswitchd.Interface)
					iface.ExternalIDs = map[string]string{"iface-id": "ns_pod"}
					if tc.name == "interface replaced" && time.Since(start) >= 3*time.Millisecond {
						iface.ExternalIDs = map[string]string{"iface-id": "ns_other", "ovn-installed": "true"}
					}
					return nil
				}}
				info, pod := bindingPod(t)
				getter := bindingPodGetter(func() (*corev1.Pod, error) {
					if time.Since(start) >= 3*time.Millisecond {
						switch tc.name {
						case "pod replaced":
							pod.UID = "replacement"
						case "pod deleted":
							return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pod")
						case "annotation changed":
							annotation := info.PodAnnotation
							annotation.MAC = net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0, 2}
							var err error
							pod.Annotations, err = util.MarshalPodAnnotation(nil, &annotation, info.NADKey)
							require.NoError(t, err)
						}
					}
					return pod, nil
				})
				err := waitForPodInterface(ctx, c, info, "pod-port", "ns_pod", getter, "ns", "pod", "uid")
				require.ErrorContains(t, err, tc.wantErr)
				require.LessOrEqual(t, time.Since(start), 200*time.Millisecond)
			})
		})
	}
}

func TestWaitForPodInterfaceNotifications(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		readyAfter, wantElapsed            time.Duration
		duringRegistration, duringRead     bool
		dropNotification, cancel, replaced bool
		spurious                           bool
		wantErr                            string
		wantPodChecks                      int
	}{
		{name: "ready during registration", duringRegistration: true},
		{name: "update between read and select", duringRead: true, wantPodChecks: 1},
		{name: "wakes before first timer", readyAfter: time.Millisecond, wantElapsed: time.Millisecond, wantPodChecks: 1},
		{name: "timer and notification together", readyAfter: 2 * time.Millisecond, wantElapsed: 2 * time.Millisecond, wantPodChecks: 1},
		{name: "wakes after backoff", readyAfter: 650 * time.Millisecond, wantElapsed: 650 * time.Millisecond, wantPodChecks: 4},
		{name: "dropped notification", readyAfter: 3 * time.Millisecond, wantElapsed: 6 * time.Millisecond, dropNotification: true, wantPodChecks: 1},
		{name: "cancellation and notification together", readyAfter: 3 * time.Millisecond, wantElapsed: 3 * time.Millisecond,
			cancel: true, wantErr: "canceled while", wantPodChecks: 1},
		{name: "replacement is not readiness", readyAfter: 3 * time.Millisecond, wantElapsed: 3 * time.Millisecond,
			replaced: true, wantErr: "no longer active", wantPodChecks: 1},
		{name: "spurious updates preserve pod check cadence", readyAfter: 650 * time.Millisecond, wantElapsed: 650 * time.Millisecond,
			spurious: true, wantPodChecks: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				start := time.Now()
				changes := make(chan struct{}, 1)
				notify := func() {
					select {
					case changes <- struct{}{}:
					default:
					}
				}
				registered, ready := false, false
				stopped := 0
				c := &bindingOVSClient{
					watch: func(name string) (<-chan struct{}, func()) {
						require.Equal(t, "pod-port", name)
						registered = true
						if tc.duringRegistration {
							ready = true
							notify()
						} else if !tc.duringRead {
							go func() {
								time.Sleep(tc.readyAfter)
								if tc.cancel {
									cancel()
								}
								if !tc.dropNotification {
									notify()
								}
							}()
						}
						if tc.spurious {
							go func() {
								for range 50 {
									time.Sleep(time.Millisecond)
									notify()
								}
							}()
						}
						return changes, func() { stopped++ }
					},
					get: func(_ context.Context, row model.Model) error {
						require.True(t, registered, "subscribe before the first cache read")
						iface := row.(*vswitchd.Interface)
						iface.ExternalIDs = map[string]string{"iface-id": "ns_pod"}
						if ready || (!tc.duringRead && time.Since(start) >= tc.readyAfter) {
							iface.ExternalIDs["ovn-installed"] = "true"
							if tc.replaced {
								iface.ExternalIDs["iface-id"] = "ns_other"
							}
						}
						if tc.duringRead && !ready {
							// Return the old snapshot after the cache has changed and
							// the callback has already signaled the waiting request.
							ready = true
							notify()
						}
						return nil
					},
				}
				info, pod := bindingPod(t)
				var podChecks []time.Duration
				getter := bindingPodGetter(func() (*corev1.Pod, error) {
					podChecks = append(podChecks, time.Since(start))
					return pod, nil
				})
				err := waitForPodInterface(ctx, c, info, "pod-port", "ns_pod", getter, "ns", "pod", "uid")
				if tc.wantErr != "" {
					require.ErrorContains(t, err, tc.wantErr)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, tc.wantElapsed, time.Since(start))
				require.Equal(t, 1, stopped, "all exit paths must unregister the waiter")
				require.Len(t, podChecks, tc.wantPodChecks)
				for i, elapsed := range podChecks {
					require.Equal(t, time.Duration(i)*200*time.Millisecond, elapsed)
				}
			})
		})
	}
}
