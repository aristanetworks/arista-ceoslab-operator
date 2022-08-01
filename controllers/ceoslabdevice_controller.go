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

package controllers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	ceoslabv1alpha1 "gitlab.aristanetworks.com/ofrasier/arista-ceoslab-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	FAILED_STATE     = "failed"
	INPROGRESS_STATE = "reconciling"
	SUCCESS_STATE    = "success"

	INIT_CONTAINER_IMAGE = "networkop/init-wait:latest"
	CEOS_COMMAND         = "/sbin/init"
	DEFAULT_CEOS_IMAGE   = "ceos:latest"
)

var (
	defaultEnvVars = map[string]string{
		"CEOS":                                "1",
		"EOS_PLATFORM":                        "ceoslab",
		"container":                           "docker",
		"ETBA":                                "1",
		"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT": "1",
		"INTFTYPE":                            "eth",
	}

	requeue    = ctrl.Result{Requeue: true}
	noRequeue  = ctrl.Result{}
	ethIntfRe  = regexp.MustCompile(`^Ethernet\d+(?:/\d+)?(?:/\d+)?$`)
	mgmtIntfRe = regexp.MustCompile(`^Management\d+(?:/\d+)?$`)
)

// CEosLabDeviceReconciler reconciles a CEosLabDevice object
type CEosLabDeviceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Helpers for updating status
func (r *CEosLabDeviceReconciler) updateDeviceFail(ctx context.Context, device *ceoslabv1alpha1.CEosLabDevice, errMsg string) {
	device.Status.State = FAILED_STATE
	device.Status.Reason = errMsg
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceReconciling(ctx context.Context, device *ceoslabv1alpha1.CEosLabDevice, msg string) {
	device.Status.State = INPROGRESS_STATE
	device.Status.Reason = msg
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceSuccess(ctx context.Context, device *ceoslabv1alpha1.CEosLabDevice) {
	device.Status.State = SUCCESS_STATE
	device.Status.Reason = ""
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceReconcilingWithRestart(ctx context.Context, pod *corev1.Pod, device *ceoslabv1alpha1.CEosLabDevice, msg string) {
	r.Delete(ctx, pod)
	r.updateDeviceReconciling(ctx, device, msg)
	return
}

//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices/finalizers,verbs=update

//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *CEosLabDeviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the CEosLabDevice instance
	device := &ceoslabv1alpha1.CEosLabDevice{}
	err := r.Get(ctx, req.NamespacedName, device)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Info("CEosLabDevice resource not found. Ignoring since object must be deleted")
			return noRequeue, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get CEosLabDevice")
		return requeue, err
	}

	// Create and validate the state of resources associated with this CEosLabDevice instance.
	// We create config maps to mount a few files into the container, the pod itself, and
	// possibly a set of services (if configured in the spec).
	// We need the configmaps on hand to create the pod, so we will do this in that order.

	// The error handling for creating and pushing config maps is very repetitive so it's
	// been factored out. The same applies to services and pods but we only configure them once.
	configMapFactories := []configMapFactory{}

	// Certificates
	for i, certConfig := range device.Spec.CertConfig.SelfSignedCerts {
		// Capture loop variables
		getName := func(i int) getNameFn {
			return func() string {
				return fmt.Sprintf("configmap-selfsigned-%s-%d", device.Name, i)
			}
		}(i)
		getConfigMap := func(certConfig ceoslabv1alpha1.SelfSignedCertConfig) getConfigMapFn {
			return func() (*corev1.ConfigMap, error) {
				certPem, keyPem, err := getSelfSignedCert(&certConfig)
				if err != nil {
					return nil, err
				}
				// Name and Namespace will be set when this factory is processed
				configMap := &corev1.ConfigMap{
					BinaryData: map[string][]byte{
						certConfig.CertName: certPem,
						certConfig.KeyName:  keyPem,
					},
				}
				return configMap, nil
			}
		}(certConfig)
		configMapFactories = append(configMapFactories, configMapFactory{
			getName:      getName,
			getConfigMap: getConfigMap,
		})
	}

	// Generate rc.eos to load certificates from /mnt/flash into ramfs
	if len(device.Spec.CertConfig.SelfSignedCerts) > 0 {
		getName := func() string {
			return fmt.Sprintf("configmap-rceos-%s", device.Name)
		}
		getConfigMap := func() (*corev1.ConfigMap, error) {
			rcEos := getRcEos(device)
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					// This file needs to be executable
					Annotations: map[string]string{"permissions": "775"},
				},
				BinaryData: map[string][]byte{
					"rc.eos": rcEos,
				},
			}
			return configMap, nil
		}
		configMapFactories = append(configMapFactories, configMapFactory{
			getName:      getName,
			getConfigMap: getConfigMap,
		})
	}

	// Explicit kernel device to EOS interface mapping
	if len(device.Spec.IntfMapping) > 0 {
		getName := func() string {
			return fmt.Sprintf("configmap-intfmapping-%s", device.Name)
		}
		getConfigMap := func() (*corev1.ConfigMap, error) {
			jsonIntfMapping, err := getJsonIntfMapping(device.Spec.IntfMapping)
			if err != nil {
				return nil, err
			}
			configMap := &corev1.ConfigMap{
				BinaryData: map[string][]byte{
					"EosIntfMapping.json": jsonIntfMapping,
				},
			}
			return configMap, nil
		}
		configMapFactories = append(configMapFactories, configMapFactory{
			getName:      getName,
			getConfigMap: getConfigMap,
		})
	}

	// Feature toggle overrides
	if len(device.Spec.ToggleOverrides) > 0 {
		getName := func() string {
			return fmt.Sprintf("configmap-toggle-override-%s", device.Name)
		}
		getConfigMap := func() (*corev1.ConfigMap, error) {
			toggleOverrides := getToggleOverrides(device.Spec.ToggleOverrides)
			configMap := &corev1.ConfigMap{
				BinaryData: map[string][]byte{
					"toggle_override": toggleOverrides,
				},
			}
			return configMap, nil
		}
		configMapFactories = append(configMapFactories, configMapFactory{
			getName:      getName,
			getConfigMap: getConfigMap,
		})
	}

	// Check if each config map already exists, if not create them.
	// Collect into a slice from which we later define volumes.
	configMaps := []*corev1.ConfigMap{}
	newConfigMaps := false
	for _, configMapFactory := range configMapFactories {
		configMap := &corev1.ConfigMap{}
		configMapName := configMapFactory.getName()
		err = r.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: device.Namespace}, configMap)
		if err != nil && errors.IsNotFound(err) {
			// Create a new config map
			msg := fmt.Sprintf("Creating configmap %s for CEosLabDevice %s", configMapName, device.Name)
			log.Info(msg)
			doConfigMapError := func() {
				errMsg := fmt.Sprintf("Failed to create configmap %s for CEosLabDevice %s",
					configMapName, device.Name)
				log.Error(err, errMsg)
				r.updateDeviceFail(ctx, device, errMsg)
			}
			configMap, err = configMapFactory.getConfigMap()
			if err != nil {
				doConfigMapError()
				return noRequeue, nil
			}
			configMap.ObjectMeta.Name = configMapName
			configMap.ObjectMeta.Namespace = device.Namespace
			err = r.Create(ctx, configMap)
			if err != nil {
				doConfigMapError()
				return noRequeue, nil
			}
			log.Info(fmt.Sprintf("Created configmap %s for CEosLabDevice %s", configMapName, device.Name))
			newConfigMaps = true
		} else if err != nil {
			errMsg := fmt.Sprintf("Failed to get configmap %s for CEosLabDevice %s",
				configMapName, device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(ctx, device, errMsg)
			return noRequeue, err
		}
		configMaps = append(configMaps, configMap)
	}
	if newConfigMaps {
		// We created new config maps. As a sanity test, requeue to make sure we pick them back up.
		msg := fmt.Sprintf("Created configmaps for CEosLabDevice %s", device.Name)
		log.Info(msg)
		r.updateDeviceReconciling(ctx, device, msg)
		return requeue, nil
	}

	// Check if the pod for this device already exists, if not create a new one
	devicePod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Name: device.Name, Namespace: device.Namespace}, devicePod)
	if err != nil && errors.IsNotFound(err) {
		// Define a new pod
		msg := fmt.Sprintf("Creating pod for CEosLabDevice %s", device.Name)
		log.Info(msg)
		doPodError := func() {
			errMsg := fmt.Sprintf("Failed to create pod for CEosLabDevice %s", device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(ctx, device, errMsg)
		}
		devicePod, err = r.getPod(device, configMaps)
		if err != nil {
			doPodError()
			return noRequeue, err
		}
		err = r.Create(ctx, devicePod)
		if err != nil {
			doPodError()
			return noRequeue, err
		}
		// Pod created successfully - return and requeue
		log.Info(fmt.Sprintf("Created pod for CEosLabDevice %s", device.Name))
		r.updateDeviceReconciling(ctx, device, msg)
		return requeue, nil
	} else if err != nil {
		errMsg := fmt.Sprintf("Failed to get pod for CEosLabDevice %s", device.Name)
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
		return noRequeue, err
	}

	// Check if pod services already exist, if not create new ones
	deviceService := &corev1.Service{}
	err = r.Get(ctx,
		types.NamespacedName{
			Name:      fmt.Sprintf("service-%s", device.Name),
			Namespace: device.Namespace,
		}, deviceService)
	if err != nil && errors.IsNotFound(err) {
		msg := fmt.Sprintf("Creating new services for CEosLabDevice %s", device.Name)
		log.Info(msg)
		deviceService = r.getService(device)
		err = r.Create(ctx, deviceService)
		if err != nil {
			errMsg := fmt.Sprintf("Failed to create services for CEosLabDevice %s", device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(ctx, device, errMsg)
			return noRequeue, err
		}
		// Service create successfully - return and requeue
		log.Info(fmt.Sprintf("Created service for CEosLabDevice %s", device.Name))
		r.updateDeviceReconciling(ctx, device, msg)
		return requeue, nil
	} else if err != nil {
		errMsg := fmt.Sprintf("Failed to get pod services for CEosLabDevice %s", device.Name)
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
		return noRequeue, err
	}

	// Ensure the pod labels are the same as the metadata
	labels := getLabels(device)
	podLabels := devicePod.ObjectMeta.Labels
	if !reflect.DeepEqual(labels, podLabels) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod labels, new: %v, old: %v",
			device.Name, labels, podLabels)
		log.Info(msg)
		// Update pod
		devicePod.ObjectMeta.Labels = labels
		r.Update(ctx, devicePod)
		// Update status
		r.updateDeviceReconciling(ctx, device, msg)
		return requeue, nil
	}

	// Ensure the pod environment variables are the same as the spec
	container := devicePod.Spec.Containers[0]
	envVarsMap := getEnvVarsMap(device)
	containerEnvVars := getEnvVarsMapFromK8sAPI(container.Env)
	if !reflect.DeepEqual(envVarsMap, containerEnvVars) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod environment variables, new: %v, old: %v",
			device.Name, envVarsMap, containerEnvVars)
		log.Info(msg)
		// Environment variables can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(ctx, devicePod, device, msg)
		return requeue, nil
	}

	// Ensure the pod image is the same as the spec
	specImage := getImage(device)
	containerImage := container.Image
	if specImage != containerImage {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod image, new: %s, old: %s",
			device.Name, specImage, containerImage)
		log.Info(msg)
		// Image can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(ctx, devicePod, device, msg)
		return requeue, nil
	}

	// Ensure the pod resource requirements are same as the spec
	specResourceRequirements, err := getResourceMap(device)
	containerResourceRequirements := getResourceMapFromK8sAPI(&container.Resources)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to validate CEosLabDevice %s pod  requirements, new: %v, old: %v",
			device.Name, device.Spec.Resources, containerResourceRequirements)
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
	}
	if !reflect.DeepEqual(specResourceRequirements, containerResourceRequirements) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod resource requirements, new: %v, old: %v",
			device.Name, specResourceRequirements, containerResourceRequirements)
		log.Info(msg)
		// Resource requirements can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(ctx, devicePod, device, msg)
		return requeue, nil
	}

	// Ensure the pod args are the same as the spec
	specArgs := getArgsMap(device, envVarsMap)
	containerArgs := strSliceToMap(container.Args)
	if !reflect.DeepEqual(specArgs, containerArgs) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod arguments, new: %v, old: %v",
			device.Name, specArgs, containerArgs)
		log.Info(msg)
		// Update pod
		// Pod args can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(ctx, devicePod, device, msg)
		return requeue, nil
	}

	// Ensure the pod init container args are correct.
	initContainer := devicePod.Spec.InitContainers[0]
	specInitContainerArgs := strSliceToMap(getInitContainerArgs(device))
	podInitContainerArgs := strSliceToMap(initContainer.Args)
	if !reflect.DeepEqual(specInitContainerArgs, podInitContainerArgs) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod init container arguments, new: %s, old: %s",
			device.Name, specInitContainerArgs, podInitContainerArgs)
		log.Info(msg)
		// Init container args can only be changed by reconciling
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(ctx, devicePod, device, msg)
		return requeue, nil
	}

	// Ensure services are the same as the spec
	specServiceMap := getServiceMap(device)
	containerServiceMap := getServiceMapFromK8sAPI(deviceService)
	if !reflect.DeepEqual(specServiceMap, containerServiceMap) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod services, new: %v, old; %v",
			device.Name, specServiceMap, containerServiceMap)
		log.Info(msg)
		// Update pod
		deviceService.Spec.Ports = getServicePortsAPI(specServiceMap)
		r.Update(ctx, deviceService)
		// Update status
		r.updateDeviceReconciling(ctx, device, msg)
		return requeue, nil
	}

	// Device reconciled
	log.Info(fmt.Sprintf("CEosLabDevice %s reconciled", device.Name))
	r.updateDeviceSuccess(ctx, device)
	return noRequeue, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CEosLabDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ceoslabv1alpha1.CEosLabDevice{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// Reconciliation helpers

// ConfigMaps

type getNameFn func() string
type getConfigMapFn func() (*corev1.ConfigMap, error)

type configMapFactory struct {
	getName      getNameFn
	getConfigMap getConfigMapFn
}

func getSelfSignedCert(config *ceoslabv1alpha1.SelfSignedCertConfig) (certPem []byte, keyPem []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, int(config.KeySize))
	if err != nil {
		return nil, nil, err
	}
	keyPem = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	template := &x509.Certificate{
		BasicConstraintsValid: true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		Issuer: pkix.Name{
			Organization: []string{"Arista Networks"},
			CommonName:   config.CommonName,
		},
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour * 24 * 365),
	}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	certPem = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert,
	})
	return certPem, keyPem, nil
}

func getRcEos(device *ceoslabv1alpha1.CEosLabDevice) []byte {
	// Generate file to run at boot. It waits for agents to be warm and then
	// copies certificates to an in-memory filesystem. The `Cli -c` command expects
	// literal carriage returns. Sample output:
	// #!/bin/sh
	// {
	// wfw
	// Cli -c 'enable
	// copy file:/mnt/flash/cert.pem certificate:cert.pem
	// copy file:/mnt/flash/key.pem sslkey:key.pem
	// '
	// } 2> /mnt/flash/rc.eos.log >&2
	buffer := &bytes.Buffer{}
	writeLn := func(txt string) {
		buffer.Write([]byte(txt + "\n"))
	}
	writeLn("#!/bin/bash")
	writeLn("{")
	writeLn("/usr/bin/wfw")
	writeLn("/usr/bin/Cli -c 'enable")
	for _, certConfig := range device.Spec.CertConfig.SelfSignedCerts {
		writeLn(fmt.Sprintf("copy file:/mnt/flash/%s certificate:%s",
			certConfig.CertName, certConfig.CertName))
		writeLn(fmt.Sprintf("copy file:/mnt/flash/%s sslkey:%s",
			certConfig.KeyName, certConfig.KeyName))
	}
	writeLn("'")
	writeLn("} 2>/mnt/flash/rc.eos.log >&2")
	return buffer.Bytes()
}

type jsonIntfMapping struct {
	// Internal structure to marshal to JSON kernel device to interface mapping
	EthernetIntf   map[string]string `json:"EthernetIntf,omitempty"`
	ManagementIntf map[string]string `json:"ManagementIntf,omitempty"`
}

func getJsonIntfMapping(intfMapping map[string]string) ([]byte, error) {
	jsonIntfMapping := jsonIntfMapping{
		EthernetIntf:   map[string]string{},
		ManagementIntf: map[string]string{},
	}
	for kernelDevice, eosInterface := range intfMapping {
		if ethIntfRe.MatchString(eosInterface) {
			jsonIntfMapping.EthernetIntf[kernelDevice] = eosInterface
		} else if mgmtIntfRe.MatchString(eosInterface) {
			jsonIntfMapping.ManagementIntf[kernelDevice] = eosInterface
		} else {
			err := fmt.Errorf("Unrecognized EOS interface %s", eosInterface)
			return nil, err
		}
	}
	jsonIntfMappingBytes, err := json.Marshal(intfMapping)
	if err != nil {
		return nil, err
	}
	return jsonIntfMappingBytes, nil
}

func getToggleOverrides(toggleOverrides map[string]bool) []byte {
	buffer := &bytes.Buffer{}
	for toggle, enabled := range toggleOverrides {
		if enabled {
			buffer.Write([]byte(toggle + "=1\n"))
		} else {
			buffer.Write([]byte(toggle + "=0\n"))
		}
	}
	return buffer.Bytes()
}

// Pods

func (r *CEosLabDeviceReconciler) getPod(device *ceoslabv1alpha1.CEosLabDevice, configMaps []*corev1.ConfigMap) (*corev1.Pod, error) {
	labels := getLabels(device)
	envVarsMap := getEnvVarsMap(device)
	env := getEnvVarsAPI(device, envVarsMap)
	command := getCommand(device)
	args := strMapToSlice(getArgsMap(device, envVarsMap))
	image := getImage(device)
	resourceMap, err := getResourceMap(device)
	if err != nil {
		return nil, err
	}
	volumes := getVolumes(configMaps)
	volumeMounts := getVolumeMounts(configMaps)
	resources := *getResourcesAPI(resourceMap)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      device.Name,
			Namespace: device.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{
				Name:  fmt.Sprintf("init-%s", device.Name),
				Image: INIT_CONTAINER_IMAGE,
				Args: []string{
					fmt.Sprintf("%d", device.Spec.NumInterfaces+1),
					fmt.Sprintf("%d", device.Spec.Sleep),
				},
				ImagePullPolicy: "IfNotPresent",
			}},
			Containers: []corev1.Container{{
				Image:           image,
				Name:            "ceos",
				Command:         command,
				Args:            args,
				Env:             env,
				Resources:       resources,
				ImagePullPolicy: "IfNotPresent",
				VolumeMounts:    volumeMounts,
				// Run container in privileged mode
				SecurityContext: &corev1.SecurityContext{Privileged: pointer.Bool(true)},
				// Check if agents are warm
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						Exec: &corev1.ExecAction{
							Command: []string{"wfw", "-t", "5"},
						},
					},
					TimeoutSeconds: 5,
					PeriodSeconds:  5,
					// Try for up to a minute
					FailureThreshold: 12,
				},
			}},
			TerminationGracePeriodSeconds: pointer.Int64(0),
			NodeSelector:                  map[string]string{},
			Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
						Weight: 100,
						PodAffinityTerm: corev1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{{
									Key:      "topo",
									Operator: "In",
									Values:   []string{device.Name},
								}},
							},
							TopologyKey: "kubernetes.io/hostname",
						},
					}},
				},
			},
			Volumes: volumes,
		},
	}
	ctrl.SetControllerReference(device, pod, r.Scheme)
	return pod, nil
}

func getEnvVarsMap(device *ceoslabv1alpha1.CEosLabDevice) map[string]string {
	envVars := map[string]string{}
	for k, v := range defaultEnvVars {
		envVars[k] = v
	}
	for varname, val := range device.Spec.EnvVar {
		// Add new environment variables, possibly overriding defaults
		envVars[varname] = val
	}
	return envVars
}

func getEnvVarsAPI(device *ceoslabv1alpha1.CEosLabDevice, envVarsMap map[string]string) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}
	for varname, val := range envVarsMap {
		envVars = append(envVars, corev1.EnvVar{
			Name:  varname,
			Value: val,
		})
	}
	return envVars
}

func getEnvVarsMapFromK8sAPI(envVarsAPI []corev1.EnvVar) map[string]string {
	envVars := map[string]string{}
	for _, envvar := range envVarsAPI {
		envVars[envvar.Name] = envvar.Value
	}
	return envVars
}

func getCommand(device *ceoslabv1alpha1.CEosLabDevice) []string {
	command := []string{CEOS_COMMAND}
	return command
}

func getLabels(device *ceoslabv1alpha1.CEosLabDevice) map[string]string {
	labels := map[string]string{
		// Defaults
		"app":  device.Name,
		"topo": device.Namespace,
	}
	for label, value := range device.Labels {
		// Extra labels attached to CR, such as model, version, and OS
		labels[label] = value
	}
	return labels
}

func getImage(device *ceoslabv1alpha1.CEosLabDevice) string {
	image := DEFAULT_CEOS_IMAGE
	if specImage := device.Spec.Image; specImage != "" {
		image = specImage
	}
	return image
}

func getArgsMap(device *ceoslabv1alpha1.CEosLabDevice, envVarsMap map[string]string) map[string]struct{} {
	args := map[string]struct{}{}
	for varname, val := range envVarsMap {
		args["systemd.setenv="+varname+"="+val] = struct{}{}
	}
	for _, arg := range device.Spec.Args {
		args[arg] = struct{}{}
	}
	return args
}

func strSliceToMap(slice []string) map[string]struct{} {
	strMap := map[string]struct{}{}
	for _, str := range slice {
		strMap[str] = struct{}{}
	}
	return strMap
}

func strMapToSlice(strMap map[string]struct{}) []string {
	slice := []string{}
	for str := range strMap {
		slice = append(slice, str)
	}
	return slice
}

func getInitContainerArgs(device *ceoslabv1alpha1.CEosLabDevice) []string {
	args := []string{
		fmt.Sprintf("%d", device.Spec.NumInterfaces+1),
		fmt.Sprintf("%d", device.Spec.Sleep),
	}
	return args
}

func getResourceMap(device *ceoslabv1alpha1.CEosLabDevice) (map[string]string, error) {
	resourceMap := map[string]string{}
	resourceSpec := device.Spec.Resources
	for resourceType, resourceStr := range resourceSpec {
		// Convert the resource into a quantity and back into a string. This is stupid,
		// but it lets us easily parse the requirement into a canonical form. Otherwise
		// we can't compare it to what's stored in etcd.
		asResource, err := resource.ParseQuantity(resourceStr)
		if err != nil {
			return nil, fmt.Errorf("Could not parse resource requirements: %s", resourceStr)
		}
		canonicalString := asResource.String()
		resourceMap[resourceType] = canonicalString
	}
	return resourceMap, nil
}

func getResourceMapFromK8sAPI(resources *corev1.ResourceRequirements) map[string]string {
	resourceMap := map[string]string{}
	for resourceType, asResource := range resources.Requests {
		canonicalString := asResource.String()
		resourceMap[string(resourceType)] = canonicalString
	}
	return resourceMap
}

func getResourcesAPI(resourceMap map[string]string) *corev1.ResourceRequirements {
	requirements := &corev1.ResourceRequirements{
		Requests: map[corev1.ResourceName]resource.Quantity{},
	}
	for resourceType, resourceStr := range resourceMap {
		// resourceStr has already been parsed into a canonical form, so this should never fail.
		requirements.Requests[corev1.ResourceName(resourceType)] = resource.MustParse(resourceStr)
	}
	return requirements
}

func volumeName(configMapName string) string {
	return "volume-" + configMapName
}

func getVolumes(configMaps []*corev1.ConfigMap) []corev1.Volume {
	volumes := []corev1.Volume{}
	for _, configMap := range configMaps {
		volume := corev1.Volume{
			Name: volumeName(configMap.Name),
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: configMap.Name,
					},
				},
			},
		}
		if permsStr, ok := configMap.Annotations["perms"]; ok {
			// Permissions are represented in octal
			perms, err := strconv.ParseInt(permsStr, 8, 32)
			if err != nil {
				// This is set by the controller so this would be unexpected.
				panic(fmt.Sprintf("Could not parse permissions %s to int", permsStr))
			}
			volume.VolumeSource.ConfigMap.DefaultMode = pointer.Int32(int32(perms))
		}
		volumes = append(volumes, volume)
	}
	return volumes
}

func getVolumeMounts(configMaps []*corev1.ConfigMap) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{}
	for _, configMap := range configMaps {
		for filename := range configMap.BinaryData {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volumeName(configMap.Name),
				MountPath: "/mnt/flash/" + filename,
				SubPath:   filename,
			})
		}
	}
	return mounts
}

// Services

func (r *CEosLabDeviceReconciler) getService(device *ceoslabv1alpha1.CEosLabDevice) *corev1.Service {
	serviceMap := getServiceMap(device)
	servicePorts := getServicePortsAPI(serviceMap)
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("service-%s", device.Name),
			Namespace: device.Namespace,
			Labels: map[string]string{
				"pod": device.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: servicePorts,
			Selector: map[string]string{
				"app": device.Name,
			},
			Type: "LoadBalancer",
		},
	}
	return service
}

type deviceServicePort struct {
	port     uint32
	protocol corev1.Protocol
}

func getServiceMap(device *ceoslabv1alpha1.CEosLabDevice) map[string]deviceServicePort {
	serviceMap := map[string]deviceServicePort{}
	for service, serviceConfig := range device.Spec.Services {
		for _, tcpPort := range serviceConfig.TCPPorts {
			serviceName := strings.ToLower(fmt.Sprintf("%s%s%d", service, "TCP", tcpPort))
			serviceMap[serviceName] = deviceServicePort{
				port:     tcpPort,
				protocol: "TCP",
			}
		}
		for _, udpPort := range serviceConfig.UDPPorts {
			serviceName := strings.ToLower(fmt.Sprintf("%s%s%d", service, "UDP", udpPort))
			serviceMap[serviceName] = deviceServicePort{
				port:     udpPort,
				protocol: "UDP",
			}
		}
	}
	return serviceMap
}

func getServiceMapFromK8sAPI(service *corev1.Service) map[string]deviceServicePort {
	serviceMap := map[string]deviceServicePort{}
	for _, v := range service.Spec.Ports {
		serviceMap[v.Name] = deviceServicePort{
			port:     uint32(v.Port),
			protocol: v.Protocol,
		}
	}
	return serviceMap
}

func getServicePortsAPI(serviceMap map[string]deviceServicePort) []corev1.ServicePort {
	ports := []corev1.ServicePort{}
	for serviceName, servicePort := range serviceMap {
		// serviceName is the service name concatenated with the protocol type
		ports = append(ports, corev1.ServicePort{
			Name:     serviceName,
			Protocol: servicePort.protocol,
			Port:     int32(servicePort.port),
		})
	}
	return ports
}
