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
	kLog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
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

	ctx context.Context
	log logr.Logger
)

// CEosLabDeviceReconciler reconciles a CEosLabDevice object
type CEosLabDeviceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Helpers for updating status
func (r *CEosLabDeviceReconciler) updateDeviceFail(device *ceoslabv1alpha1.CEosLabDevice, errMsg string) {
	device.Status.State = FAILED_STATE
	device.Status.Reason = errMsg
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceReconciling(device *ceoslabv1alpha1.CEosLabDevice, msg string) {
	device.Status.State = INPROGRESS_STATE
	device.Status.Reason = msg
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceSuccess(device *ceoslabv1alpha1.CEosLabDevice) {
	device.Status.State = SUCCESS_STATE
	device.Status.Reason = ""
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceReconcilingWithRestart(pod *corev1.Pod, device *ceoslabv1alpha1.CEosLabDevice, msg string) {
	r.Delete(ctx, pod)
	r.updateDeviceReconciling(device, msg)
	return
}

//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices/finalizers,verbs=update

//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.2/pkg/reconcile
func (r *CEosLabDeviceReconciler) Reconcile(ctx_ context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = ctx_
	log = kLog.FromContext(ctx)

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

	// Create or retrieve k8s objects associated with this CEosLabDevice, and validate their
	// state. We create configmaps to mount a few files into the container, the pod itself,
	// and possibly a set of services (if configured in the spec).

	// The error handling for creating and pushing k8s resources is very repetitive so it's
	// been factored out. Requeue if we create any new objects to be certain we get them back.
	isNewObject := false
	haveNewObjects := false

	// ConfigMaps and Secrets. Collect into a slice so later we can generate volumes from them.
	secretsAndConfigMaps := []client.Object{}

	// Certificates
	selfSignedCerts := device.Spec.CertConfig.SelfSignedCerts
	for i, certConfig := range selfSignedCerts {
		name := fmt.Sprintf("secret-selfsigned-%s-%d", device.Name, i)
		createObject := func(object client.Object) error {
			err := getSelfSignedCert(object.(*corev1.Secret), certConfig)
			if err != nil {
				return err
			}
			return nil
		}
		secret := &corev1.Secret{}
		isNewObject, err = r.getOrCreateObject(device, name, createObject, secret)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps = append(secretsAndConfigMaps, secret)
	}

	// Generate rc.eos to load certificates from /mnt/flash into in-memory filesystem
	if len(selfSignedCerts) > 0 {
		name := fmt.Sprintf("configmap-rceos-%s", device.Name)
		createObject := func(object client.Object) error {
			getRcEos(object.(*corev1.ConfigMap), selfSignedCerts)
			return nil
		}
		configMap := &corev1.ConfigMap{}
		isNewObject, err = r.getOrCreateObject(device, name, createObject, configMap)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps = append(secretsAndConfigMaps, configMap)
	}

	// Explicit kernel device to EOS interface mapping
	intfMapping := device.Spec.IntfMapping
	if len(intfMapping) > 0 {
		name := fmt.Sprintf("configmap-intfmapping-%s", device.Name)
		createObject := func(object client.Object) error {
			return getJsonIntfMapping(object.(*corev1.ConfigMap), intfMapping)
		}
		configMap := &corev1.ConfigMap{}
		isNewObject, err = r.getOrCreateObject(device, name, createObject, configMap)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps = append(secretsAndConfigMaps, configMap)
	}

	// Feature toggle overrides
	toggleOverrides := device.Spec.ToggleOverrides
	if len(toggleOverrides) > 0 {
		name := fmt.Sprintf("configmap-toggle-override-%s", device.Name)
		createObject := func(object client.Object) error {
			getToggleOverrides(object.(*corev1.ConfigMap), toggleOverrides)
			return nil
		}
		configMap := &corev1.ConfigMap{}
		isNewObject, err = r.getOrCreateObject(device, name, createObject, configMap)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps = append(secretsAndConfigMaps, configMap)
	}

	// Pods

	createPodObject := func(object client.Object) error {
		return getPod(object.(*corev1.Pod), device, secretsAndConfigMaps)
	}
	devicePod := &corev1.Pod{}
	isNewObject, err = r.getOrCreateObject(device, device.Name, createPodObject, devicePod)
	if err != nil {
		return noRequeue, err
	}
	haveNewObjects = haveNewObjects || isNewObject

	// Services

	serviceName := fmt.Sprintf("service-%s", device.Name)
	getServiceObject := func(object client.Object) error {
		getService(object.(*corev1.Service), device)
		return nil
	}
	deviceService := &corev1.Service{}
	isNewObject, err = r.getOrCreateObject(device, serviceName, getServiceObject, deviceService)
	if err != nil {
		return noRequeue, err
	}
	haveNewObjects = haveNewObjects || isNewObject

	if haveNewObjects {
		// We created new objects. As a sanity test, requeue to make sure we pick them back up.
		msg := fmt.Sprintf("Created k8s objects for CEosLabDevice %s", device.Name)
		log.Info(msg)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Reconcile observed state against desired state

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
		r.updateDeviceReconciling(device, msg)
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
		r.updateDeviceReconcilingWithRestart(devicePod, device, msg)
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
		r.updateDeviceReconcilingWithRestart(devicePod, device, msg)
		return requeue, nil
	}

	// Ensure the pod resource requirements are same as the spec
	specResourceRequirements, err := getResourceMap(device)
	containerResourceRequirements := getResourceMapFromK8sAPI(&container.Resources)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to validate CEosLabDevice %s pod  requirements, new: %v, old: %v",
			device.Name, device.Spec.Resources, containerResourceRequirements)
		log.Error(err, errMsg)
		r.updateDeviceFail(device, errMsg)
	}
	if !reflect.DeepEqual(specResourceRequirements, containerResourceRequirements) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod resource requirements, new: %v, old: %v",
			device.Name, specResourceRequirements, containerResourceRequirements)
		log.Info(msg)
		// Resource requirements can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(devicePod, device, msg)
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
		r.updateDeviceReconcilingWithRestart(devicePod, device, msg)
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
		r.updateDeviceReconcilingWithRestart(devicePod, device, msg)
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
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Device reconciled
	log.Info(fmt.Sprintf("CEosLabDevice %s reconciled", device.Name))
	r.updateDeviceSuccess(device)
	return noRequeue, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CEosLabDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ceoslabv1alpha1.CEosLabDevice{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

// Reconciliation helpers

type makeObjectFn func(client.Object) error

func (r *CEosLabDeviceReconciler) getOrCreateObject(device *ceoslabv1alpha1.CEosLabDevice, name string, makeObject makeObjectFn, object client.Object) (bool, error) {
	isNewObject := false
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: device.Namespace}, object)
	if err != nil && errors.IsNotFound(err) {
		// Create new k8s object
		msg := fmt.Sprintf("Creating %s for CEosLabDevice %s", name, device.Name)
		log.Info(msg)
		doCreateObjectError := func() {
			errMsg := fmt.Sprintf("Failed to create %s for CEosLabDevice %s",
				object, device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(device, errMsg)
		}
		err = makeObject(object)
		if err != nil {
			doCreateObjectError()
			return false, err
		}
		object.SetName(name)
		object.SetNamespace(device.Namespace)
		ctrl.SetControllerReference(device, object, r.Scheme)
		err = r.Create(ctx, object)
		if err != nil {
			doCreateObjectError()
			return false, err
		}
		log.Info(fmt.Sprintf("Created %s for CEosLabDevice %s", name, device.Name))
		isNewObject = true
	} else if err != nil {
		errMsg := fmt.Sprintf("Failed to get %s for CEosLabDevice %s", name, device.Name)
		log.Error(err, errMsg)
		r.updateDeviceFail(device, errMsg)
		return false, err
	}
	return isNewObject, nil
}

// ConfigMaps and Secrets

func getSelfSignedCert(secret *corev1.Secret, config ceoslabv1alpha1.SelfSignedCertConfig) error {
	key, err := rsa.GenerateKey(rand.Reader, int(config.KeySize))
	if err != nil {
		return err
	}
	keyPem := pem.EncodeToMemory(&pem.Block{
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
		return err
	}
	certPem := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert,
	})
	secret.Data = map[string][]byte{
		config.CertName: certPem,
		config.KeyName:  keyPem,
	}
	return nil
}

func getRcEos(configMap *corev1.ConfigMap, configs []ceoslabv1alpha1.SelfSignedCertConfig) {
	// Generate file to run at boot. It waits for agents to be warm and then
	// copies certificates to an in-memory filesystem. The `Cli -c` command expects
	// literal carriage returns. Sample output:
	// #!/bin/bash
	// {
	// wfw
	// Cli -Ac 'enable
	// copy file:/mnt/flash/cert.pem certificate:cert.pem
	// copy file:/mnt/flash/key.pem sslkey:key.pem
	// '
	// } 2> /mnt/flash/rc.eos.log >&2 &
	buffer := &bytes.Buffer{}
	writeLn := func(txt string) {
		buffer.Write([]byte(txt + "\n"))
	}
	writeLn("#!/bin/bash")
	writeLn("{")
	writeLn("wfw")
	writeLn("Cli -Ac 'enable")
	for _, certConfig := range configs {
		writeLn(fmt.Sprintf("copy file:/mnt/flash/%s certificate:%s",
			certConfig.CertName, certConfig.CertName))
		writeLn(fmt.Sprintf("copy file:/mnt/flash/%s sslkey:%s",
			certConfig.KeyName, certConfig.KeyName))
	}
	writeLn("'")
	writeLn("} 2>/mnt/flash/rc.eos.log >&2 &")
	script := buffer.Bytes()
	// This file needs to be executable
	configMap.ObjectMeta.Annotations = map[string]string{"permissions": "775"}
	configMap.BinaryData = map[string][]byte{"rc.eos": script}
	return
}

type jsonIntfMapping struct {
	// Internal structure to marshal to JSON kernel device to interface mapping
	EthernetIntf   map[string]string `json:"EthernetIntf,omitempty"`
	ManagementIntf map[string]string `json:"ManagementIntf,omitempty"`
}

func getJsonIntfMapping(configMap *corev1.ConfigMap, intfMapping map[string]string) error {
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
			return err
		}
	}
	jsonIntfMappingBytes, err := json.Marshal(intfMapping)
	if err != nil {
		return err
	}
	configMap.BinaryData = map[string][]byte{"EosIntfMapping.json": jsonIntfMappingBytes}
	return nil
}

func getToggleOverrides(configMap *corev1.ConfigMap, toggleOverrides map[string]bool) {
	buffer := &bytes.Buffer{}
	for toggle, enabled := range toggleOverrides {
		if enabled {
			buffer.Write([]byte(toggle + "=1\n"))
		} else {
			buffer.Write([]byte(toggle + "=0\n"))
		}
	}
	overrides := buffer.Bytes()
	configMap.BinaryData = map[string][]byte{"toggle_override": overrides}
	return
}

// Pods

func getPod(pod *corev1.Pod, device *ceoslabv1alpha1.CEosLabDevice, secretsAndConfigMaps []client.Object) error {
	labels := getLabels(device)
	envVarsMap := getEnvVarsMap(device)
	env := getEnvVarsAPI(device, envVarsMap)
	command := getCommand(device)
	args := strMapToSlice(getArgsMap(device, envVarsMap))
	image := getImage(device)
	resourceMap, err := getResourceMap(device)
	if err != nil {
		return err
	}
	volumes := getVolumes(secretsAndConfigMaps)
	volumeMounts := getVolumeMounts(secretsAndConfigMaps)
	resources := getResourcesAPI(resourceMap)
	pod.ObjectMeta.Labels = labels
	pod.Spec = corev1.PodSpec{
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
	}
	return nil
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

func getResourcesAPI(resourceMap map[string]string) corev1.ResourceRequirements {
	requirements := corev1.ResourceRequirements{
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

func getVolumes(secretsAndConfigMaps []client.Object) []corev1.Volume {
	volumes := []corev1.Volume{}
	for _, secretOrConfigMap := range secretsAndConfigMaps {
		var permissions *int32 = nil
		if permsStr, ok := secretOrConfigMap.GetAnnotations()["permissions"]; ok {
			// Permissions are represented in octal
			perms, err := strconv.ParseInt(permsStr, 8, 32)
			if err != nil {
				// This is set by the controller so this would be unexpected.
				panic(fmt.Sprintf("Could not parse permissions %s to int", permsStr))
			}
			permissions = pointer.Int32(int32(perms))
		}
		volumeSource := corev1.VolumeSource{}
		switch secretOrConfigMap.(type) {
		case *corev1.ConfigMap:
			volumeSource = corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: secretOrConfigMap.GetName(),
					},
				},
			}
			if permissions != nil {
				volumeSource.ConfigMap.DefaultMode = permissions
			}
		case *corev1.Secret:
			volumeSource = corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: secretOrConfigMap.GetName(),
				},
			}
			if permissions != nil {
				volumeSource.Secret.DefaultMode = permissions
			}
		}
		volume := corev1.Volume{
			Name:         volumeName(secretOrConfigMap.GetName()),
			VolumeSource: volumeSource,
		}
		volumes = append(volumes, volume)
	}
	return volumes
}

func getVolumeMounts(secretsAndConfigMaps []client.Object) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{}
	for _, secretOrConfigMap := range secretsAndConfigMaps {
		var data map[string][]byte
		switch v := secretOrConfigMap.(type) {
		case *corev1.ConfigMap:
			data = v.BinaryData
		case *corev1.Secret:
			data = v.Data
		}
		for filename := range data {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volumeName(secretOrConfigMap.GetName()),
				MountPath: "/mnt/flash/" + filename,
				SubPath:   filename,
			})
		}
	}
	return mounts
}

// Services

func getService(service *corev1.Service, device *ceoslabv1alpha1.CEosLabDevice) {
	serviceMap := getServiceMap(device)
	servicePorts := getServicePortsAPI(serviceMap)
	service.ObjectMeta.Labels = map[string]string{"pod": device.Name}
	service.Spec = corev1.ServiceSpec{
		Ports: servicePorts,
		Selector: map[string]string{
			"app": device.Name,
		},
		Type: "LoadBalancer",
	}
	return
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
