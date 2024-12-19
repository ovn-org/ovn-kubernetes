/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"
	json "encoding/json"
	"fmt"

	v1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1"
	networkqosv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1/apis/applyconfiguration/networkqos/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeNetworkQoSes implements NetworkQoSInterface
type FakeNetworkQoSes struct {
	Fake *FakeK8sV1
	ns   string
}

var networkqosesResource = v1.SchemeGroupVersion.WithResource("networkqoses")

var networkqosesKind = v1.SchemeGroupVersion.WithKind("NetworkQoS")

// Get takes name of the networkQoS, and returns the corresponding networkQoS object, and an error if there is any.
func (c *FakeNetworkQoSes) Get(ctx context.Context, name string, options metav1.GetOptions) (result *v1.NetworkQoS, err error) {
	emptyResult := &v1.NetworkQoS{}
	obj, err := c.Fake.
		Invokes(testing.NewGetActionWithOptions(networkqosesResource, c.ns, name, options), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.NetworkQoS), err
}

// List takes label and field selectors, and returns the list of NetworkQoSes that match those selectors.
func (c *FakeNetworkQoSes) List(ctx context.Context, opts metav1.ListOptions) (result *v1.NetworkQoSList, err error) {
	emptyResult := &v1.NetworkQoSList{}
	obj, err := c.Fake.
		Invokes(testing.NewListActionWithOptions(networkqosesResource, networkqosesKind, c.ns, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1.NetworkQoSList{ListMeta: obj.(*v1.NetworkQoSList).ListMeta}
	for _, item := range obj.(*v1.NetworkQoSList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested networkQoSes.
func (c *FakeNetworkQoSes) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchActionWithOptions(networkqosesResource, c.ns, opts))

}

// Create takes the representation of a networkQoS and creates it.  Returns the server's representation of the networkQoS, and an error, if there is any.
func (c *FakeNetworkQoSes) Create(ctx context.Context, networkQoS *v1.NetworkQoS, opts metav1.CreateOptions) (result *v1.NetworkQoS, err error) {
	emptyResult := &v1.NetworkQoS{}
	obj, err := c.Fake.
		Invokes(testing.NewCreateActionWithOptions(networkqosesResource, c.ns, networkQoS, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.NetworkQoS), err
}

// Update takes the representation of a networkQoS and updates it. Returns the server's representation of the networkQoS, and an error, if there is any.
func (c *FakeNetworkQoSes) Update(ctx context.Context, networkQoS *v1.NetworkQoS, opts metav1.UpdateOptions) (result *v1.NetworkQoS, err error) {
	emptyResult := &v1.NetworkQoS{}
	obj, err := c.Fake.
		Invokes(testing.NewUpdateActionWithOptions(networkqosesResource, c.ns, networkQoS, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.NetworkQoS), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeNetworkQoSes) UpdateStatus(ctx context.Context, networkQoS *v1.NetworkQoS, opts metav1.UpdateOptions) (result *v1.NetworkQoS, err error) {
	emptyResult := &v1.NetworkQoS{}
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceActionWithOptions(networkqosesResource, "status", c.ns, networkQoS, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.NetworkQoS), err
}

// Delete takes name of the networkQoS and deletes it. Returns an error if one occurs.
func (c *FakeNetworkQoSes) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteActionWithOptions(networkqosesResource, c.ns, name, opts), &v1.NetworkQoS{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeNetworkQoSes) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error {
	action := testing.NewDeleteCollectionActionWithOptions(networkqosesResource, c.ns, opts, listOpts)

	_, err := c.Fake.Invokes(action, &v1.NetworkQoSList{})
	return err
}

// Patch applies the patch and returns the patched networkQoS.
func (c *FakeNetworkQoSes) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1.NetworkQoS, err error) {
	emptyResult := &v1.NetworkQoS{}
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceActionWithOptions(networkqosesResource, c.ns, name, pt, data, opts, subresources...), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.NetworkQoS), err
}

// Apply takes the given apply declarative configuration, applies it and returns the applied networkQoS.
func (c *FakeNetworkQoSes) Apply(ctx context.Context, networkQoS *networkqosv1.NetworkQoSApplyConfiguration, opts metav1.ApplyOptions) (result *v1.NetworkQoS, err error) {
	if networkQoS == nil {
		return nil, fmt.Errorf("networkQoS provided to Apply must not be nil")
	}
	data, err := json.Marshal(networkQoS)
	if err != nil {
		return nil, err
	}
	name := networkQoS.Name
	if name == nil {
		return nil, fmt.Errorf("networkQoS.Name must be provided to Apply")
	}
	emptyResult := &v1.NetworkQoS{}
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceActionWithOptions(networkqosesResource, c.ns, *name, types.ApplyPatchType, data, opts.ToPatchOptions()), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.NetworkQoS), err
}

// ApplyStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating ApplyStatus().
func (c *FakeNetworkQoSes) ApplyStatus(ctx context.Context, networkQoS *networkqosv1.NetworkQoSApplyConfiguration, opts metav1.ApplyOptions) (result *v1.NetworkQoS, err error) {
	if networkQoS == nil {
		return nil, fmt.Errorf("networkQoS provided to Apply must not be nil")
	}
	data, err := json.Marshal(networkQoS)
	if err != nil {
		return nil, err
	}
	name := networkQoS.Name
	if name == nil {
		return nil, fmt.Errorf("networkQoS.Name must be provided to Apply")
	}
	emptyResult := &v1.NetworkQoS{}
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceActionWithOptions(networkqosesResource, c.ns, *name, types.ApplyPatchType, data, opts.ToPatchOptions(), "status"), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1.NetworkQoS), err
}
