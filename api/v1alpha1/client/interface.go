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

package client

import (
	"context"

	"github.com/aristanetworks/arista-ceoslab-operator/v2/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

type CEosLabDeviceV1Alpha1Interface interface {
	CEosLabDevices(namespace string) CEosLabDeviceOpsInterface
}

type CEosLabDeviceOpsInterface interface {
	List(ctx context.Context, opts metav1.ListOptions) (*v1alpha1.CEosLabDeviceList, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1alpha1.CEosLabDevice, error)
	Create(ctx context.Context, device *v1alpha1.CEosLabDevice, opts metav1.CreateOptions) (*v1alpha1.CEosLabDevice, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *v1alpha1.CEosLabDevice, err error)
}
