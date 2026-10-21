// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"strings"
	"testing"

	frrapi "github.com/metallb/frr-k8s/api/v1beta1"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestRouteAdvertisementsNodeWatch(t *testing.T) {
	for _, localNode := range []string{"local", strings.Repeat("a", 63) + ".example.org"} {
		for _, nodeAgent := range []bool{false, true} {
			name := "OVN controller"
			if nodeAgent {
				name = "node agent"
			}
			t.Run(name+"/"+localNode, func(t *testing.T) {
				g := gomega.NewWithT(t)
				g.Expect(config.PrepareTestConfig()).To(gomega.Succeed())
				t.Cleanup(func() { g.Expect(config.PrepareTestConfig()).To(gomega.Succeed()) })
				config.OVNKubernetesFeature.EnableMultiNetwork = true
				config.OVNKubernetesFeature.EnableRouteAdvertisements = true
				clients := util.GetOVNClientset()
				for _, node := range []string{localNode, "remote"} {
					_, err := clients.FRRClient.ApiV1beta1().FRRConfigurations("frr").Create(context.Background(),
						&frrapi.FRRConfiguration{ObjectMeta: metav1.ObjectMeta{Name: node, Namespace: "frr", Labels: map[string]string{
							types.OvnRouteAdvertisementsKey: "ra", types.OvnRouteAdvertisementsNodeKey: util.RouteAdvertisementsNodeLabel(node),
						}}}, metav1.CreateOptions{})
					g.Expect(err).NotTo(gomega.HaveOccurred())
				}
				var wf *WatchFactory
				var err error
				if nodeAgent {
					wf, err = NewNodeWatchFactory(clients.GetNodeClientset(), localNode)
				} else {
					wf, err = NewOVNKubeControllerWatchFactory(clients.GetOVNKubeControllerClientset(), localNode)
				}
				g.Expect(err).NotTo(gomega.HaveOccurred())
				defer wf.Shutdown()
				g.Expect(wf.Start()).To(gomega.Succeed())
				configs, err := wf.FRRConfigurationsInformer().Lister().List(labels.Everything())
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(configs).To(gomega.HaveLen(1))
				g.Expect(configs[0].Name).To(gomega.Equal(localNode))
				indexed, err := wf.FRRConfigurationsInformer().Informer().GetIndexer().ByIndex(types.OvnRouteAdvertisementsKey, "ra/"+util.RouteAdvertisementsNodeLabel(localNode))
				g.Expect(err).NotTo(gomega.HaveOccurred())
				g.Expect(indexed).To(gomega.HaveLen(1))
			})
		}
	}
}
