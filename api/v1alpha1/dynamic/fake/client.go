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

package fake

import (
	"github.com/aristanetworks/arista-ceoslab-operator/v2/api/v1alpha1"
	"github.com/aristanetworks/arista-ceoslab-operator/v2/api/v1alpha1/dynamic"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

func NewSimpleClientset(objects ...runtime.Object) (*dynamic.CEosLabDeviceV1Alpha1Client, error) {
	ceosClient, err := dynamic.NewForConfig(&rest.Config{})
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	v1alpha1.AddToScheme(scheme)
	dynamicClient := fakeclient.NewSimpleDynamicClient(scheme, objects...)
	ceosClient.SetDeviceClient(dynamicClient.Resource(v1alpha1.GroupVersionResource))
	return ceosClient, nil
}
