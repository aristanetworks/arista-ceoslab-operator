/*
Copyright 2022.

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

package dynamic

import (
	"context"
	"fmt"

	"github.com/aristanetworks/arista-ceoslab-operator/api/v1alpha1"
	intf "github.com/aristanetworks/arista-ceoslab-operator/api/v1alpha1/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

type CEosLabDeviceV1Alpha1Client struct {
	dynamicResource dynamic.NamespaceableResourceInterface
}

// NewForConfig returns a new Clientset based on c.
func NewForConfig(c *rest.Config) (*CEosLabDeviceV1Alpha1Client, error) {
	config := *c
	config.ContentConfig.GroupVersion = &v1alpha1.GroupVersion
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	config.UserAgent = rest.DefaultKubernetesUserAgent()

	client, err := dynamic.NewForConfig(c)
	if err != nil {
		return nil, err
	}
	resourceInterface := client.Resource(v1alpha1.GroupVersionResource)

	return &CEosLabDeviceV1Alpha1Client{dynamicResource: resourceInterface}, nil
}

func (c *CEosLabDeviceV1Alpha1Client) CEosLabDevices(namespace string) intf.CEosLabDeviceOpsInterface {
	return &ceosLabDeviceClient{
		dynamicResource: c.dynamicResource,
		ns:              namespace,
	}
}

type ceosLabDeviceClient struct {
	dynamicResource dynamic.NamespaceableResourceInterface
	ns              string
}

func (c *CEosLabDeviceV1Alpha1Client) SetDeviceClient(newDynamicResource dynamic.NamespaceableResourceInterface) {
	c.dynamicResource = newDynamicResource
}

var (
	_ intf.CEosLabDeviceV1Alpha1Interface = (*CEosLabDeviceV1Alpha1Client)(nil)
	_ intf.CEosLabDeviceOpsInterface      = (*ceosLabDeviceClient)(nil)
)

func (c *ceosLabDeviceClient) List(ctx context.Context, opts metav1.ListOptions) (*v1alpha1.CEosLabDeviceList, error) {
	result := v1alpha1.CEosLabDeviceList{}
	u, err := c.dynamicResource.Namespace(c.ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &result); err != nil {
		return nil, fmt.Errorf("failed to type assert return to CEosLabDeviceList: %w", err)
	}
	return &result, err
}

func (c *ceosLabDeviceClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1alpha1.CEosLabDevice, error) {
	u, err := c.dynamicResource.Namespace(c.ns).Get(ctx, name, opts)
	if err != nil {
		return nil, err
	}
	result := v1alpha1.CEosLabDevice{}
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &result); err != nil {
		return nil, fmt.Errorf("failed to type assert return to CEosLabDevice: %w", err)
	}
	return &result, nil
}

func (c *ceosLabDeviceClient) Create(ctx context.Context, device *v1alpha1.CEosLabDevice, opts metav1.CreateOptions) (*v1alpha1.CEosLabDevice, error) {
	device.TypeMeta = metav1.TypeMeta{
		Kind:       v1alpha1.GroupVersionKind.Kind,
		APIVersion: v1alpha1.GroupVersion.String(),
	}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(device)
	if err != nil {
		return nil, fmt.Errorf("failed to convert CEosLabDevice to unstructured: %w", err)
	}
	u, err := c.dynamicResource.Namespace(c.ns).Create(ctx, &unstructured.Unstructured{Object: obj}, opts)
	if err != nil {
		return nil, err
	}
	result := v1alpha1.CEosLabDevice{}
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &result); err != nil {
		return nil, fmt.Errorf("failed to type assert return to CEosLabDevice: %w", err)
	}
	return &result, nil
}

func (c *ceosLabDeviceClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return c.dynamicResource.Namespace(c.ns).Watch(ctx, opts)
}

func (c *ceosLabDeviceClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	return c.dynamicResource.Namespace(c.ns).Delete(ctx, name, opts)
}

func (c *ceosLabDeviceClient) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*v1alpha1.CEosLabDevice, error) {
	u, err := c.dynamicResource.Namespace(c.ns).Patch(ctx, name, pt, data, opts, subresources...)
	if err != nil {
		return nil, err
	}
	result := v1alpha1.CEosLabDevice{}
	if err = runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &result); err != nil {
		return nil, fmt.Errorf("failed to type assert return to CEosLabDevice: %w", err)
	}
	return &result, nil
}
