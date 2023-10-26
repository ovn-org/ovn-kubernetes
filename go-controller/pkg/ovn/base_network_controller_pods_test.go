package ovn

import (
	"net"
	"testing"
	"time"

	"github.com/onsi/gomega"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestBaseNetworkController_trackPodsReleasedBeforeStartup(t *testing.T) {
	tests := []struct {
		name           string
		podAnnotations map[*corev1.Pod]map[string]*util.PodAnnotation
		expected       map[string]sets.Set[string]
	}{
		{
			name: "a scheduled/running annotated pod should not be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
				}: {
					"default": {
						IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.1/24")},
					},
				},
			},
			expected: map[string]sets.Set[string]{},
		},
		{
			name: "a completed annotated pod should not be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}: {
					"default": {
						IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.1/24")},
					},
				},
			},
			expected: map[string]sets.Set[string]{},
		},
		{
			// consider dual-stack IPs individually but only track at the pod level based
			// on a couple of assumptions:
			// - while the same pair of IPs released for a pod will most likely be
			//   assigned to a different pod, assume that one of those IPs might be
			//   assigned to a pod and the other IP to a different pod. This is easy to
			//   handle so better take a safe approach
			// - assume that there is no error path leading to one of the IPs of the
			//   pair to be released while the other is not. This is based on the fact
			//   that both IPs are released in block.
			name: "a completed pod sharing at least one IP with a running Pod should be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.2/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
			},
			expected: map[string]sets.Set[string]{
				"default": sets.New("completed"),
			},
		},
		{
			name: "only the last completed pod of multiple completed pods sharing at least one IP should not be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed-third",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
						Conditions: []corev1.PodCondition{
							{
								Type: corev1.PodInitialized,
								LastTransitionTime: metav1.Time{
									Time: time.Time{}.Add(time.Second * 2),
								},
							},
						},
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed-first",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
						Conditions: []corev1.PodCondition{
							{
								Type: corev1.PodInitialized,
								LastTransitionTime: metav1.Time{
									Time: time.Time{},
								},
							},
						},
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::2/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed-second",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
						Conditions: []corev1.PodCondition{
							{
								Type: corev1.PodInitialized,
								LastTransitionTime: metav1.Time{
									Time: time.Time{}.Add(time.Second),
								},
							},
						},
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.2/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
			},
			expected: map[string]sets.Set[string]{
				"default": sets.New("completed-first", "completed-second"),
			},
		},
		{
			name: "a completed pod sharing at least one IP from nad1 with a running Pod on nad2 should be considered released on nad1",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}: {
					"nad1": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
					"nad2": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.2/24"),
							ovntest.MustParseIPNet("fd11::2/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
				}: {
					"nad1": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.3/24"),
							ovntest.MustParseIPNet("fd11::3/64"),
						},
					},
					"nad2": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.4/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
			},
			expected: map[string]sets.Set[string]{
				"nad1": sets.New("completed"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			bnc := &BaseNetworkController{}

			bnc.trackPodsReleasedBeforeStartup(tt.podAnnotations)

			g.Expect(bnc.releasedPodsBeforeStartup).To(gomega.Equal(tt.expected))
		})
	}
}
