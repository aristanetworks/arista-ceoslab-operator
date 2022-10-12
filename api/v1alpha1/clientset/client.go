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

package clientset

import (
	"context"

	"github.com/aristanetworks/arista-ceoslab-operator/api/v1alpha1"
	intf "github.com/aristanetworks/arista-ceoslab-operator/api/v1alpha1/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

type CEosLabDeviceV1Alpha1Client struct {
	restClient rest.Interface
}

func NewForConfig(c *rest.Config) (*CEosLabDeviceV1Alpha1Client, error) {
	config := *c
	config.ContentConfig.GroupVersion = &v1alpha1.GroupVersion
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	config.UserAgent = rest.DefaultKubernetesUserAgent()

	client, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}

	return &CEosLabDeviceV1Alpha1Client{restClient: client}, nil
}

func (c *CEosLabDeviceV1Alpha1Client) CEosLabDevices(namespace string) intf.CEosLabDeviceOpsInterface {
	return &ceosLabDeviceClient{
		restClient: c.restClient,
		ns:         namespace,
	}
}

type ceosLabDeviceClient struct {
	restClient rest.Interface
	ns         string
}

var (
	_ intf.CEosLabDeviceV1Alpha1Interface = (*CEosLabDeviceV1Alpha1Client)(nil)
	_ intf.CEosLabDeviceOpsInterface      = (*ceosLabDeviceClient)(nil)
)

func resource() string {
	return v1alpha1.GroupVersionResource.Resource
}

func (c *ceosLabDeviceClient) List(ctx context.Context, opts metav1.ListOptions) (*v1alpha1.CEosLabDeviceList, error) {
	result := v1alpha1.CEosLabDeviceList{}
	err := c.restClient.
		Get().
		Namespace(c.ns).
		Resource(resource()).
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(ctx).
		Into(&result)
	return &result, err
}

func (c *ceosLabDeviceClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1alpha1.CEosLabDevice, error) {
	result := v1alpha1.CEosLabDevice{}
	err := c.restClient.
		Get().
		Namespace(c.ns).
		Resource(resource()).
		Name(name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(ctx).
		Into(&result)
	return &result, err
}

func (c *ceosLabDeviceClient) Create(ctx context.Context, device *v1alpha1.CEosLabDevice, opts metav1.CreateOptions) (*v1alpha1.CEosLabDevice, error) {
	result := v1alpha1.CEosLabDevice{}
	err := c.restClient.
		Post().
		Namespace(c.ns).
		Resource(resource()).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(device).
		Do(ctx).
		Into(&result)
	return &result, err
}

func (c *ceosLabDeviceClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return c.restClient.
		Get().
		Namespace(c.ns).
		Resource(resource()).
		VersionedParams(&opts, scheme.ParameterCodec).
		Watch(ctx)
}

func (c *ceosLabDeviceClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error {
	err := c.restClient.
		Delete().
		Namespace(c.ns).
		Resource(resource()).
		Name(name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Do(ctx).
		Error()
	return err
}

func (c *ceosLabDeviceClient) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1alpha1.CEosLabDevice, err error) {
	result = &v1alpha1.CEosLabDevice{}
	err = c.restClient.
		Patch(pt).
		Namespace(c.ns).
		Resource(resource()).
		Name(name).
		SubResource(subresources...).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}
