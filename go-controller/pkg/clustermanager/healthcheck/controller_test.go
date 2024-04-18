package healthcheck

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/healthcheck"
	informerscorev1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/informers/core/v1"
	listerscorev1mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/k8s.io/client-go/listers/core/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/timeoutqueue"
)

type fakeController struct {
	reconciled string
}

func (fakeController *fakeController) Reconcile(key string) {
	fakeController.reconciled = key
}

func (fakeController *fakeController) ReconcileAll() {
	panic("not implemented") // TODO: Implement
}

type fakeConsumer struct {
	consumed string
	monitor  bool
}

func (fakeConsumer *fakeConsumer) Name() string {
	return "fakeConsumer"
}

func (fakeConsumer *fakeConsumer) HealthStateChanged(node string) {
	fakeConsumer.consumed = node
}

func (fakeConsumer *fakeConsumer) MonitorHealthState(node string) bool {
	return fakeConsumer.monitor
}

func Test_controller_probe(t *testing.T) {
	tests := []struct {
		name           string
		health         HealthState
		available      time.Time
		cancelled      bool
		isReachableErr error
		noInterest     bool
		expectHealth   HealthState
	}{
		{
			name:         "initial available probe",
			health:       UNKNOWN,
			expectHealth: AVAILABLE,
		},
		{
			name:           "initial unreachable probe",
			health:         UNKNOWN,
			isReachableErr: fmt.Errorf("some unreachable error"),
			expectHealth:   UNREACHABLE,
		},
		{
			name:         "available probe",
			health:       AVAILABLE,
			expectHealth: AVAILABLE,
		},
		{
			name:           "unreachable probe before timeout",
			health:         AVAILABLE,
			available:      time.Now(),
			isReachableErr: fmt.Errorf("some unreachable error"),
			expectHealth:   AVAILABLE,
		},
		{
			name:           "unreachable probe after timeout",
			health:         AVAILABLE,
			available:      time.Now().Add(-2 * time.Minute),
			isReachableErr: fmt.Errorf("some unreachable error"),
			expectHealth:   UNREACHABLE,
		},
		{
			name:         "available probe after unreachable",
			health:       UNREACHABLE,
			expectHealth: AVAILABLE,
		},
		{
			name:           "unreachable probe after unreachable",
			health:         UNREACHABLE,
			isReachableErr: fmt.Errorf("some unreachable error"),
			expectHealth:   UNREACHABLE,
		},
		{
			name:           "canceled probe",
			health:         AVAILABLE,
			isReachableErr: fmt.Errorf("some unreachable error"),
			expectHealth:   AVAILABLE,
			cancelled:      true,
		},
		{
			name:         "no interest after probe",
			health:       AVAILABLE,
			expectHealth: AVAILABLE,
			noInterest:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := "node-1"
			fakeController := fakeController{}
			fakeConsumer := fakeConsumer{
				monitor: !tt.noInterest,
			}
			c := &controller{
				controller:     &fakeController,
				probeTaskQueue: &timeoutqueue.TimeoutQueue[*probeTask]{},
				nodeState: map[string]nodeState{
					nodeName: {
						health:    tt.health,
						available: tt.available,
					},
				},
				consumers: []Consumer{&fakeConsumer},
				timeout:   time.Minute,
			}

			var disconnected bool
			healthStateClient := healthStateClient{
				isReachable: func(ctx context.Context) error { return tt.isReachableErr },
				disconnect:  func() { disconnected = true },
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			probeTask := &probeTask{
				node:   nodeName,
				client: healthStateClient,
				ctx:    ctx,
			}

			c.probe(probeTask)

			if tt.expectHealth != c.nodeState[nodeName].health {
				t.Fatalf("Expected health state %s got %s", tt.expectHealth, c.nodeState[nodeName].health)
			}

			if tt.noInterest != (fakeController.reconciled == nodeName) {
				t.Fatalf("Expected reconcile %t got %t", tt.noInterest, fakeController.reconciled == nodeName)
			}

			if tt.cancelled != disconnected {
				t.Fatalf("Expected disconnect %t got %t", tt.cancelled, disconnected)
			}

			if (tt.health != tt.expectHealth) != (fakeConsumer.consumed == nodeName) {
				t.Fatalf("Expected informed consumer %t got %t", tt.health != tt.expectHealth, fakeConsumer.consumed == nodeName)
			}
		})
	}
}

func Test_controller_reconcile(t *testing.T) {
	subnet := "10.128.0.0/24"
	updatedSubnet := "10.128.1.0/24"
	garbageSubnet := "garbage"
	noSubnet := ""

	tests := []struct {
		name          string
		health        HealthState
		nodeSubnets   *string
		noInterest    bool
		expectHealth  HealthState
		expectProbing bool
		expectErr     bool
	}{
		{
			name:         "node deleted",
			health:       AVAILABLE,
			nodeSubnets:  nil,
			expectHealth: UNKNOWN,
		},
		{
			name:         "no interest",
			health:       AVAILABLE,
			nodeSubnets:  &subnet,
			noInterest:   true,
			expectHealth: UNKNOWN,
		},
		{
			name:         "no subnets",
			health:       AVAILABLE,
			nodeSubnets:  &noSubnet,
			expectHealth: UNREACHABLE,
		},
		{
			name:        "bad subnets",
			health:      AVAILABLE,
			nodeSubnets: &garbageSubnet,
			expectErr:   true,
		},
		{
			name:          "updated subnets",
			health:        AVAILABLE,
			nodeSubnets:   &updatedSubnet,
			expectHealth:  AVAILABLE,
			expectProbing: true,
		},
		{
			name:          "no update",
			nodeSubnets:   &subnet,
			expectProbing: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := "node-1"
			var node *corev1.Node
			switch {
			case tt.nodeSubnets == nil:
			case *tt.nodeSubnets == "":
				node = &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
					},
				}
			default:
				node = &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: nodeName,
						Annotations: map[string]string{
							"k8s.ovn.org/node-subnets": "{\"default\":\"" + *tt.nodeSubnets + "\"}",
						},
					},
				}
			}

			nodeInformer := &informerscorev1mocks.NodeInformer{}
			nodeLister := listerscorev1mocks.NodeLister{}
			nodeInformer.On("Lister").Return(&nodeLister)
			nodeLister.On("Get", nodeName).Return(node, nil)

			fakeConsumer := fakeConsumer{
				monitor: !tt.noInterest,
			}

			c := &controller{
				ctx:            context.Background(),
				nodeInformer:   nodeInformer,
				consumers:      []Consumer{&fakeConsumer},
				nodeInfo:       map[string]nodeInfo{},
				nodeState:      map[string]nodeState{},
				probeTaskQueue: &timeoutqueue.TimeoutQueue[*probeTask]{},
			}

			var canceled bool
			var cancelTask func()
			if tt.health != UNKNOWN {
				c.nodeState[nodeName] = nodeState{
					health: tt.health,
				}
				cancelTask = func() { canceled = true }
				c.nodeInfo[nodeName] = nodeInfo{
					ips:        ovntest.MustParseIPs("10.128.0.2"),
					cancelTask: cancelTask,
				}
			}

			err := c.reconcile(nodeName)

			if (err != nil) != tt.expectErr {
				t.Fatalf("expected error %t got %v", tt.expectErr, err)
			}

			if tt.expectErr {
				return
			}

			if tt.expectHealth != c.nodeState[nodeName].health {
				t.Fatalf("Expected health state %s got %s", tt.expectHealth, c.nodeState[nodeName].health)
			}

			if tt.expectHealth == UNKNOWN && len(c.nodeState) > 0 {
				t.Fatalf("expected empty nodeState got %#v", c.nodeState)
			}

			if (tt.health != tt.expectHealth) != (fakeConsumer.consumed == nodeName) {
				t.Fatalf("Expected informed consumer %t got %t", tt.health != tt.expectHealth, fakeConsumer.consumed == nodeName)
			}

			if tt.expectProbing != (len(c.nodeInfo) == 1 && len(c.nodeInfo[nodeName].ips) == 1) {
				t.Fatalf("Expected ongoing probing nodeInfo %t but got %#v", tt.expectProbing, c.nodeInfo[nodeName])
			}

			if tt.expectProbing && !c.nodeInfo[nodeName].ips[0].Equal(util.GetNodeManagementIfAddr(ovntest.MustParseIPNet(*tt.nodeSubnets)).IP) {
				t.Fatalf("Expected probing ip %s but got %s", util.GetNodeManagementIfAddr(ovntest.MustParseIPNet(*tt.nodeSubnets)).IP, c.nodeInfo[nodeName].ips[0])
			}

			if !reflect.DeepEqual(c.nodeInfo[nodeName].ips, []net.IP{ovntest.MustParseIP("10.128.0.2").To4()}) && !canceled {
				t.Fatalf("Expected task canceled but was not")
			}
		})
	}
}
