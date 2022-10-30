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

// Package v1alpha1 contains API Schema definitions for the ceoslab v1alpha1 API group
//+kubebuilder:object:generate=true
//+groupName=ceoslab.arista.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
)

var (
	group    = "ceoslab.arista.com"
	version  = "v1alpha1"
	resource = "ceoslabdevices"
	kind     = "CEosLabDevice"

	// GroupVersionResource is used to register and type these objects
	GroupVersionResource = schema.GroupVersionResource{Group: group, Version: version, Resource: resource}

	// Used by the dynamic client to create a new device (Kind)
	GroupVersionKind = schema.GroupVersionKind{Group: group, Version: version, Kind: kind}

	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: group, Version: version}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&CEosLabDevice{},
		&CEosLabDeviceList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	metav1.AddMetaToScheme(scheme)
	return nil
}

func init() {
	if err := AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}
}
