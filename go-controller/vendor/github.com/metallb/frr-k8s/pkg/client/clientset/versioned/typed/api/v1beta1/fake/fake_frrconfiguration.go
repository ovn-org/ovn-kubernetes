// SPDX-License-Identifier:Apache-2.0

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1beta1 "github.com/metallb/frr-k8s/api/v1beta1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeFRRConfigurations implements FRRConfigurationInterface
type FakeFRRConfigurations struct {
	Fake *FakeApiV1beta1
	ns   string
}

var frrconfigurationsResource = v1beta1.SchemeGroupVersion.WithResource("frrconfigurations")

var frrconfigurationsKind = v1beta1.SchemeGroupVersion.WithKind("FRRConfiguration")

// Get takes name of the fRRConfiguration, and returns the corresponding fRRConfiguration object, and an error if there is any.
func (c *FakeFRRConfigurations) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1beta1.FRRConfiguration, err error) {
	emptyResult := &v1beta1.FRRConfiguration{}
	obj, err := c.Fake.
		Invokes(testing.NewGetActionWithOptions(frrconfigurationsResource, c.ns, name, options), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1beta1.FRRConfiguration), err
}

// List takes label and field selectors, and returns the list of FRRConfigurations that match those selectors.
func (c *FakeFRRConfigurations) List(ctx context.Context, opts v1.ListOptions) (result *v1beta1.FRRConfigurationList, err error) {
	emptyResult := &v1beta1.FRRConfigurationList{}
	obj, err := c.Fake.
		Invokes(testing.NewListActionWithOptions(frrconfigurationsResource, frrconfigurationsKind, c.ns, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1beta1.FRRConfigurationList{ListMeta: obj.(*v1beta1.FRRConfigurationList).ListMeta}
	for _, item := range obj.(*v1beta1.FRRConfigurationList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested fRRConfigurations.
func (c *FakeFRRConfigurations) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchActionWithOptions(frrconfigurationsResource, c.ns, opts))

}

// Create takes the representation of a fRRConfiguration and creates it.  Returns the server's representation of the fRRConfiguration, and an error, if there is any.
func (c *FakeFRRConfigurations) Create(ctx context.Context, fRRConfiguration *v1beta1.FRRConfiguration, opts v1.CreateOptions) (result *v1beta1.FRRConfiguration, err error) {
	emptyResult := &v1beta1.FRRConfiguration{}
	obj, err := c.Fake.
		Invokes(testing.NewCreateActionWithOptions(frrconfigurationsResource, c.ns, fRRConfiguration, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1beta1.FRRConfiguration), err
}

// Update takes the representation of a fRRConfiguration and updates it. Returns the server's representation of the fRRConfiguration, and an error, if there is any.
func (c *FakeFRRConfigurations) Update(ctx context.Context, fRRConfiguration *v1beta1.FRRConfiguration, opts v1.UpdateOptions) (result *v1beta1.FRRConfiguration, err error) {
	emptyResult := &v1beta1.FRRConfiguration{}
	obj, err := c.Fake.
		Invokes(testing.NewUpdateActionWithOptions(frrconfigurationsResource, c.ns, fRRConfiguration, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1beta1.FRRConfiguration), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeFRRConfigurations) UpdateStatus(ctx context.Context, fRRConfiguration *v1beta1.FRRConfiguration, opts v1.UpdateOptions) (result *v1beta1.FRRConfiguration, err error) {
	emptyResult := &v1beta1.FRRConfiguration{}
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceActionWithOptions(frrconfigurationsResource, "status", c.ns, fRRConfiguration, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1beta1.FRRConfiguration), err
}

// Delete takes name of the fRRConfiguration and deletes it. Returns an error if one occurs.
func (c *FakeFRRConfigurations) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteActionWithOptions(frrconfigurationsResource, c.ns, name, opts), &v1beta1.FRRConfiguration{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeFRRConfigurations) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionActionWithOptions(frrconfigurationsResource, c.ns, opts, listOpts)

	_, err := c.Fake.Invokes(action, &v1beta1.FRRConfigurationList{})
	return err
}

// Patch applies the patch and returns the patched fRRConfiguration.
func (c *FakeFRRConfigurations) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1beta1.FRRConfiguration, err error) {
	emptyResult := &v1beta1.FRRConfiguration{}
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceActionWithOptions(frrconfigurationsResource, c.ns, name, pt, data, opts, subresources...), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1beta1.FRRConfiguration), err
}
