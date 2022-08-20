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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CEosLabDeviceSpec defines the desired state of CEosLabDevice
type CEosLabDeviceSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Additional environment variables. Those necessary to boot properly are already present.
	EnvVar map[string]string `json:"envvars,omitempty"`
	// Image name. Default: ceos:latest
	Image string `json:"image,omitempty"`
	// Additional arguments to pass to /sbin/init. Those necessary to boot properly are already present.
	Args []string `json:"args,omitempty"`
	// Resource requests to configure on the pod. Default: none
	Resources map[string]string `json:"resourcerequirements,omitempty"`
	// Port mappings for container services. Default: none
	Services map[string]ServiceConfig `json:"services,omitempty"`
	// Number of data interfaces to create. An additional interface (eth0) is created for pod connectivity. Default: 0 interfaces
	NumInterfaces uint32 `json:"numinterfaces,omitempty"`
	// Time (in seconds) to wait before starting the device. Default: 0 seconds
	Sleep uint32 `json:"sleep,omitempty"`
	// X.509 certificate configuration.
	CertConfig CertConfig `json:"certconfig,omitempty"`
	// Explicit interface mapping between kernel devices and interface names. If this is defined, any unmapped devices are ignored.
	IntfMapping map[string]string `json:"intfmapping,omitempty"`
	// EOS feature toggle overrides
	ToggleOverrides map[string]bool `json:"toggleoverrides,omitempty"`
}

type ServiceConfig struct {
	// TCP ports to forward to the pod.
	TCPPorts []uint32 `json:"tcpports,omitempty"`
	// UDP ports to forward to the pod.
	UDPPorts []uint32 `json:"udpports,omitempty"`
}

type CertConfig struct {
	// Configuration for self-signed certificates.
	SelfSignedCerts []SelfSignedCertConfig `json:"selfsignedcerts,omitempty"`
}

type SelfSignedCertConfig struct {
	// Certificate name on the node.
	CertName string `json:"certname,omitempty"`
	// Key name on the node.
	KeyName string `json:"keyname,omitempty"`
	// RSA keysize to use for key generation.
	KeySize uint32 `json:"keysize,omitempty"`
	// Common name to set in the cert.
	CommonName string `json:"commonname,omitempty"`
}

// CEosLabDeviceStatus defines the observed state of CEosLabDevice
type CEosLabDeviceStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Device status
	State string `json:"status,omitempty"`
	// Reason for potential failure
	Reason string `json:"reason,omitempty"`

	// It's difficult to deduce the config maps' running state because the data is unstructured.
	// For simplicity's sake we store the last configuration here. Other parameters are deduced
	// via the K8s API.

	// ConfigMap state as configured in configmaps
	ConfigMapStatus ConfigMapStatus `json:"configmapconfig,omitempty"`
	// ConfigMap state as present in the pod. If these diverge, we need to restart the pod to
	// update. Even if an in-place update is possible these are needed at boot time.
	PodConfigMapStatus ConfigMapStatus `json:"podconfigmapconfig,omitempty"`
}

type ConfigMapStatus struct {
	SelfSignedCertStatus         map[string]SelfSignedCertConfig `json:"selfsignedcertstatus,omitempty"`
	IntfMappingStatus            map[string]string               `json:"intfmappingstatus,omitempty"`
	ToggleOverridesStatus        map[string]bool                 `json:"toggleoverridesstatus,omitempty"`
	RcEosStale                   bool                            `json:"rceosstale,omitempty"`
	StartupConfigResourceVersion *string                         `json:"startupconfigresourceversion,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// CEosLabDevice is the Schema for the ceoslabdevices API
type CEosLabDevice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CEosLabDeviceSpec   `json:"spec,omitempty"`
	Status CEosLabDeviceStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// CEosLabDeviceList contains a list of CEosLabDevice
type CEosLabDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CEosLabDevice `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CEosLabDevice{}, &CEosLabDeviceList{})
}
