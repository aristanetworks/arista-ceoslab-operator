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
	"sort"
	"strconv"
	"strings"
	"time"

	ceoslabv1alpha1 "github.com/aristanetworks/arista-ceoslab-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

	DEFAULT_INIT_CONTAINER_IMAGE = "networkop/init-wait:latest"
	CEOS_COMMAND                 = "/sbin/init"
	DEFAULT_CEOS_IMAGE           = "ceos:latest"
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
	r.Status().Update(ctx, device)
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceReconciling(device *ceoslabv1alpha1.CEosLabDevice, msg string) {
	device.Status.State = INPROGRESS_STATE
	device.Status.Reason = msg
	r.Status().Update(ctx, device)
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceSuccess(device *ceoslabv1alpha1.CEosLabDevice) {
	device.Status.State = SUCCESS_STATE
	device.Status.Reason = ""
	r.Status().Update(ctx, device)
	r.Update(ctx, device)
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

	// Create or retrieve k8s objects associated with this CEosLabDevice and validate their
	// state. We create configmaps to mount a few files into the container, the pod itself,
	// and possibly a set of services (if configured in the spec).

	// The error handling for creating and pushing k8s resources is very repetitive so it's
	// been factored out. Requeue if we create any new objects to check that we get them back.
	haveNewObjects := false

	// ConfigMaps and Secrets

	// Store certificate config to simplify reconciliation later. Keyed by secret name.
	selfSignedCertConfigs := map[string]ceoslabv1alpha1.SelfSignedCertConfig{}

	// Also, collect them into a map so we can generate volumes from them.
	secretsAndConfigMaps := map[string]client.Object{}

	configMapStatus := &device.Status.ConfigMapStatus

	// Certificates
	selfSignedCerts := device.Spec.CertConfig.SelfSignedCerts
	selfSignedCertsConfed := len(selfSignedCerts) > 0
	for i, certConfig := range selfSignedCerts {
		name := fmt.Sprintf("secret-selfsigned-%s-%d", device.Name, i)
		createObject := func(object client.Object) error {
			err := getSelfSignedCert(object.(*corev1.Secret), certConfig, device.Name)
			if err != nil {
				return err
			}
			ssCertStatus := &configMapStatus.SelfSignedCertStatus
			if *ssCertStatus == nil {
				*ssCertStatus = map[string]ceoslabv1alpha1.SelfSignedCertConfig{}
			}
			(*ssCertStatus)[name] = certConfig
			// Need to generate new rc.eos to reflect new certificates
			configMapStatus.RcEosStale = true
			return nil
		}
		secret := &corev1.Secret{}
		isNewObject, err := r.getOrCreateObject(device, name, createObject, secret)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps[name] = secret
		selfSignedCertConfigs[name] = certConfig
	}

	// Generate rc.eos to load certificates from /mnt/flash into in-memory filesystem
	rcEosName := fmt.Sprintf("configmap-rceos-%s", device.Name)
	if selfSignedCertsConfed {
		createObject := func(object client.Object) error {
			getRcEos(object.(*corev1.ConfigMap), selfSignedCerts)
			configMapStatus.RcEosStale = false
			return nil
		}
		configMap := &corev1.ConfigMap{}
		isNewObject, err := r.getOrCreateObject(device, rcEosName, createObject, configMap)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps[rcEosName] = configMap
	}

	// Explicit kernel device to EOS interface mapping
	intfMapping := device.Spec.IntfMapping
	intfMappingName := fmt.Sprintf("configmap-intfmapping-%s", device.Name)
	intfMappingConfed := len(intfMapping) > 0
	if intfMappingConfed {
		createObject := func(object client.Object) error {
			err := getJsonIntfMapping(object.(*corev1.ConfigMap), intfMapping)
			if err != nil {
				return err
			}
			configMapStatus.IntfMappingStatus = intfMapping
			return nil
		}
		configMap := &corev1.ConfigMap{}
		isNewObject, err := r.getOrCreateObject(device, intfMappingName, createObject, configMap)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps[intfMappingName] = configMap
	}

	// Feature toggle overrides
	toggleOverrides := device.Spec.ToggleOverrides
	toggleOverridesName := fmt.Sprintf("configmap-toggle-override-%s", device.Name)
	toggleOverridesConfed := len(toggleOverrides) > 0
	if toggleOverridesConfed {
		createObject := func(object client.Object) error {
			getToggleOverrides(object.(*corev1.ConfigMap), toggleOverrides)
			configMapStatus.ToggleOverridesStatus = toggleOverrides
			return nil
		}
		configMap := &corev1.ConfigMap{}
		isNewObject, err := r.getOrCreateObject(device, toggleOverridesName, createObject, configMap)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
		secretsAndConfigMaps[toggleOverridesName] = configMap
	}

	// KNE may create a configmap to be used as a startup config.
	var startupConfigResourceVersion *string = nil
	startupConfigName := fmt.Sprintf("%s-config", device.Name)
	{
		configMap := &corev1.ConfigMap{}
		err := r.Get(ctx, types.NamespacedName{Name: startupConfigName, Namespace: device.Namespace}, configMap)
		if err != nil && !errors.IsNotFound(err) {
			errMsg := fmt.Sprintf("Failed to get %s for CEosLabDevice %s", startupConfigName, device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(device, errMsg)
			return noRequeue, err
		} else if err == nil {
			// KNE created the configmap
			startupConfigResourceVersion = pointer.String(configMap.ObjectMeta.ResourceVersion)
			configMapStatus.StartupConfigResourceVersion = startupConfigResourceVersion
			secretsAndConfigMaps[startupConfigName] = configMap
		}
	}

	// Pods

	devicePod := &corev1.Pod{}
	{
		podName := device.Name
		createPodObject := func(object client.Object) error {
			err := getPod(object.(*corev1.Pod), device, secretsAndConfigMaps)
			if err != nil {
				return err
			}
			// Write back the configmap state at the time the pod is created. This way
			// we can resolve discrepencies between the pod and configmaps even if
			// the configmaps have already been updated.
			(&device.Status.ConfigMapStatus).DeepCopyInto(&device.Status.PodConfigMapStatus)
			return nil
		}
		isNewObject, err := r.getOrCreateObject(device, podName, createPodObject, devicePod)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
	}

	// Services

	deviceService := &corev1.Service{}
	serviceName := fmt.Sprintf("service-%s", device.Name)
	servicesConfed := len(device.Spec.Services) > 0
	if servicesConfed {
		getServiceObject := func(object client.Object) error {
			getService(object.(*corev1.Service), device)
			return nil
		}
		isNewObject, err := r.getOrCreateObject(device, serviceName, getServiceObject, deviceService)
		if err != nil {
			return noRequeue, err
		}
		haveNewObjects = haveNewObjects || isNewObject
	}

	if haveNewObjects {
		// We created new objects. As a sanity test, requeue to make sure we pick them back up.
		msg := fmt.Sprintf("Created k8s objects for CEosLabDevice %s", device.Name)
		log.Info(msg)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Reconcile observed state against desired state

	doDeleteConfigMap := func(name string, object client.Object) error {
		msg := fmt.Sprintf("Deleting %s for CEosLabDevice %s", name, device.Name)
		log.Info(msg)
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: device.Namespace}, object)
		if err == nil {
			r.Delete(ctx, object)
		} else if err != nil && !errors.IsNotFound(err) {
			errMsg := fmt.Sprintf("Could not delete %s for CEosLabDevice %s", name, device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(device, errMsg)
			return err
		}
		return nil
	}

	// Certificates
	for name, certConfig := range selfSignedCertConfigs {
		certStatus := configMapStatus.SelfSignedCertStatus[name]
		if certConfig != certStatus {
			msg := fmt.Sprintf("Cert %s out-of-sync for CEosLabDevice %s", name, device.Name)
			log.Info(msg)
			configMapStatus.RcEosStale = true
			doDeleteConfigMap(name, &corev1.Secret{})
			r.updateDeviceReconciling(device, msg)
			return requeue, nil
		}
	}
	for name := range configMapStatus.SelfSignedCertStatus {
		if _, present := selfSignedCertConfigs[name]; !present {
			msg := fmt.Sprintf("Cert %s deleted from CEosLabDevice %s", name, device.Name)
			log.Info(msg)
			configMapStatus.RcEosStale = true
			delete(configMapStatus.SelfSignedCertStatus, name)
			if len(configMapStatus.SelfSignedCertStatus) == 0 {
				configMapStatus.SelfSignedCertStatus = nil
			}
			err := doDeleteConfigMap(name, &corev1.Secret{})
			if err != nil {
				return noRequeue, err
			}
			r.updateDeviceReconciling(device, msg)
			return requeue, nil
		}
	}

	// rc.eos boot script
	if configMapStatus.RcEosStale {
		msg := ""
		if selfSignedCertsConfed {
			msg = fmt.Sprintf("rc.eos out-of-sync for CEosLabDevice %s", device.Name)
			log.Info(msg)
		} else {
			msg = fmt.Sprintf("Deleting rc.eos for CEosLabDevice %s", device.Name)
			log.Info(msg)
			configMapStatus.RcEosStale = false
		}
		doDeleteConfigMap(rcEosName, &corev1.ConfigMap{})
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Explicit kernel device to EOS interface mapping
	intfMappingStatus := configMapStatus.IntfMappingStatus
	if intfMappingStatus != nil {
		if intfMappingConfed {
			if !reflect.DeepEqual(intfMapping, intfMappingStatus) {
				msg := fmt.Sprintf("Interface mapping out-of-sync for CEosLabDevice %s",
					device.Name)
				log.Info(msg)
				doDeleteConfigMap(intfMappingName, &corev1.ConfigMap{})
				r.updateDeviceReconciling(device, msg)
				return requeue, nil
			}
		} else {
			msg := fmt.Sprintf("Interface mapping deleted from CEosLabDevice %s",
				device.Name)
			log.Info(msg)
			configMapStatus.IntfMappingStatus = nil
			doDeleteConfigMap(intfMappingName, &corev1.ConfigMap{})
			if err != nil {
				return noRequeue, err
			}
			r.updateDeviceReconciling(device, msg)
			return requeue, nil
		}
	}

	// EOS feature toggle overrides
	toggleOverridesStatus := configMapStatus.ToggleOverridesStatus
	if toggleOverridesStatus != nil {
		if toggleOverridesConfed {
			if !reflect.DeepEqual(toggleOverrides, toggleOverridesStatus) {
				msg := fmt.Sprintf("Toggle overrides out-of-sync for CEosLabDevice %s",
					device.Name)
				log.Info(msg)
				doDeleteConfigMap(toggleOverridesName, &corev1.ConfigMap{})
				r.updateDeviceReconciling(device, msg)
				return requeue, nil
			}
		} else {
			msg := fmt.Sprintf("Toggle overrides deleted from CEosLabDevice %s",
				device.Name)
			log.Info(msg)
			configMapStatus.ToggleOverridesStatus = nil
			doDeleteConfigMap(toggleOverridesName, &corev1.ConfigMap{})
			if err != nil {
				return noRequeue, err
			}
			r.updateDeviceReconciling(device, msg)
			return requeue, nil
		}
	}

	// Startup config
	startupConfigResourceVersionStatus := configMapStatus.StartupConfigResourceVersion
	if startupConfigResourceVersionStatus != nil {
		if startupConfigResourceVersion != nil {
			if *startupConfigResourceVersionStatus != *startupConfigResourceVersion {
				// No need to update the configmap, KNE already did. But we do
				// need to update the state which will trigger a pod restart
				// next reconciliation because the configmap and pod configmap
				// statuses will diverge.
				configMapStatus.StartupConfigResourceVersion = startupConfigResourceVersion
				msg := fmt.Sprintf("Startup-config out-of-sync for CEosLabDevice %s", device.Name)
				log.Info(msg)
				r.updateDeviceReconciling(device, msg)
				return requeue, nil
			}
		} else {
			// Again, no need to delete anything, but we still need to update the
			// status for the same reason.
			configMapStatus.StartupConfigResourceVersion = nil
			msg := fmt.Sprintf("Startup-config deleted from CEosLabDevice %s", device.Name)
			log.Info(msg)
			r.updateDeviceReconciling(device, msg)
			return requeue, nil
		}
	}

	// Ensure pod configmap config is the same as the current configmap config
	podConfigMapStatus := &device.Status.PodConfigMapStatus
	if !reflect.DeepEqual(podConfigMapStatus, configMapStatus) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s configmaps, new: %v, old; %v",
			device.Name, configMapStatus, podConfigMapStatus)
		log.Info(msg)
		// These configmaps are required at boot time so we need to restart the pod
		// Delete the pod, and requeue to recreate
		r.Delete(ctx, devicePod)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
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
		r.Delete(ctx, devicePod)
		r.updateDeviceReconciling(device, msg)
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
		r.Delete(ctx, devicePod)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Ensure the pod resource requirements are same as the spec
	specResourceRequirements, err := getResourceMap(device)
	containerResourceRequirements := getResourceMapFromK8sAPI(&container.Resources)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to validate CEosLabDevice %s pod requirements, new: %v, old: %v",
			device.Name, device.Spec.Resources, containerResourceRequirements)
		log.Error(err, errMsg)
		r.updateDeviceFail(device, errMsg)
		return noRequeue, nil
	}
	if !reflect.DeepEqual(specResourceRequirements, containerResourceRequirements) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod resource requirements, new: %v, old: %v",
			device.Name, specResourceRequirements, containerResourceRequirements)
		log.Info(msg)
		// Resource requirements can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.Delete(ctx, devicePod)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Ensure the pod args are the same as the spec
	specArgs := getArgs(device, envVarsMap)
	containerArgs := container.Args
	if !reflect.DeepEqual(specArgs, containerArgs) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod arguments, new: %v, old: %v",
			device.Name, specArgs, containerArgs)
		log.Info(msg)
		// Update pod
		// Pod args can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.Delete(ctx, devicePod)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Ensure the pod init container args are correct.
	initContainer := devicePod.Spec.InitContainers[0]
	specInitContainerArgs := getInitContainerArgs(device)
	podInitContainerArgs := initContainer.Args
	if !reflect.DeepEqual(specInitContainerArgs, podInitContainerArgs) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod init container arguments, new: %s, old: %s",
			device.Name, specInitContainerArgs, podInitContainerArgs)
		log.Info(msg)
		// Init container args can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.Delete(ctx, devicePod)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Ensure the pod init container image is correct
	specInitContainerImage := getInitContainerImage(device)
	podInitContainerImage := initContainer.Image
	if specInitContainerImage != podInitContainerImage {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod init container image, new: %s, old: %s",
			device.Name, specInitContainerImage, podInitContainerImage)
		log.Info(msg)
		// Init container image can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.Delete(ctx, devicePod)
		r.updateDeviceReconciling(device, msg)
		return requeue, nil
	}

	// Ensure services are the same as the spec
	specServiceMap := getServiceMap(device)
	containerServiceMap := getServiceMapFromK8sAPI(deviceService)
	if !reflect.DeepEqual(specServiceMap, containerServiceMap) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod services, new: %v, old; %v",
			device.Name, specServiceMap, containerServiceMap)
		log.Info(msg)
		// We could update services in place but this would make it harder to handle
		// the delete case. The fallout from just deleting and recreating them is
		// comparatively minimal.
		r.Delete(ctx, deviceService)
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

func getSelfSignedCert(secret *corev1.Secret, config ceoslabv1alpha1.SelfSignedCertConfig, deviceName string) error {
	key, err := rsa.GenerateKey(rand.Reader, int(config.KeySize))
	if err != nil {
		return err
	}
	keyPem := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	commonName := config.CommonName
	if commonName == "" {
		commonName = deviceName
	}
	template := &x509.Certificate{
		BasicConstraintsValid: true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		Subject: pkix.Name{
			Organization: []string{"Arista Networks"},
			CommonName:   commonName,
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
	jsonIntfMappingBytes, err := json.Marshal(jsonIntfMapping)
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

func getPod(pod *corev1.Pod, device *ceoslabv1alpha1.CEosLabDevice, secretsAndConfigMaps map[string]client.Object) error {
	labels := getLabels(device)
	envVarsMap := getEnvVarsMap(device)
	env := getEnvVarsAPI(device, envVarsMap)
	command := getCommand(device)
	args := getArgs(device, envVarsMap)
	image := getImage(device)
	initImage := getInitContainerImage(device)
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
			Image: initImage,
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
		Volumes:                       volumes,
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
	varnames := []string{}
	for varname := range envVarsMap {
		varnames = append(varnames, varname)
	}
	// Sort the environment variables. It's important to have deterministic output for
	// testing purposes.
	sort.Strings(varnames)
	envVars := []corev1.EnvVar{}
	for _, varname := range varnames {
		envVars = append(envVars, corev1.EnvVar{
			Name:  varname,
			Value: envVarsMap[varname],
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

func getArgs(device *ceoslabv1alpha1.CEosLabDevice, envVarsMap map[string]string) []string {
	varnames := []string{}
	for varname := range envVarsMap {
		varnames = append(varnames, varname)
	}
	// Sort the environment variables. It's important to have deterministic output for
	// testing purposes.
	sort.Strings(varnames)
	args := []string{}
	for _, varname := range varnames {
		val := envVarsMap[varname]
		args = append(args, "systemd.setenv="+varname+"="+val)
	}
	for _, arg := range device.Spec.Args {
		args = append(args, arg)
	}
	return args
}

func getInitContainerArgs(device *ceoslabv1alpha1.CEosLabDevice) []string {
	args := []string{
		fmt.Sprintf("%d", device.Spec.NumInterfaces+1),
		fmt.Sprintf("%d", device.Spec.Sleep),
	}
	return args
}

func getInitContainerImage(device *ceoslabv1alpha1.CEosLabDevice) string {
	image := DEFAULT_INIT_CONTAINER_IMAGE
	if specImage := device.Spec.InitContainerImage; specImage != "" {
		image = specImage
	}
	return image
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

func getVolumes(secretsAndConfigMaps map[string]client.Object) []corev1.Volume {
	volumeNames := []string{}
	for name := range secretsAndConfigMaps {
		volumeNames = append(volumeNames, name)
	}
	sort.Strings(volumeNames)
	volumes := []corev1.Volume{}
	for _, name := range volumeNames {
		secretOrConfigMap := secretsAndConfigMaps[name]
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
						Name: name,
					},
				},
			}
			if permissions != nil {
				volumeSource.ConfigMap.DefaultMode = permissions
			}
		case *corev1.Secret:
			volumeSource = corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: name,
				},
			}
			if permissions != nil {
				volumeSource.Secret.DefaultMode = permissions
			}
		}
		volume := corev1.Volume{
			Name:         volumeName(name),
			VolumeSource: volumeSource,
		}
		volumes = append(volumes, volume)
	}
	return volumes
}

func getVolumeMounts(secretsAndConfigMaps map[string]client.Object) []corev1.VolumeMount {
	volumeNames := []string{}
	for name := range secretsAndConfigMaps {
		volumeNames = append(volumeNames, name)
	}
	sort.Strings(volumeNames)
	mounts := []corev1.VolumeMount{}
	for _, name := range volumeNames {
		secretOrConfigMap := secretsAndConfigMaps[name]
		filenames := []string{}
		switch v := secretOrConfigMap.(type) {
		case *corev1.ConfigMap:
			for filename := range v.BinaryData {
				filenames = append(filenames, filename)
			}
			for filename := range v.Data {
				filenames = append(filenames, filename)
			}
		case *corev1.Secret:
			for filename := range v.Data {
				filenames = append(filenames, filename)
			}
			for filename := range v.StringData {
				filenames = append(filenames, filename)
			}
		}
		sort.Strings(filenames)
		for _, filename := range filenames {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      volumeName(name),
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

func getServiceMap(device *ceoslabv1alpha1.CEosLabDevice) map[string]uint32 {
	serviceMap := map[string]uint32{}
	for service, serviceConfig := range device.Spec.Services {
		for _, tcpPort := range serviceConfig.TCPPorts {
			serviceName := strings.ToLower(fmt.Sprintf("%s%d", service, tcpPort))
			serviceMap[serviceName] = tcpPort
		}
	}
	return serviceMap
}

func getServiceMapFromK8sAPI(service *corev1.Service) map[string]uint32 {
	serviceMap := map[string]uint32{}
	for _, v := range service.Spec.Ports {
		serviceMap[v.Name] = uint32(v.Port)
	}
	return serviceMap
}

func getServicePortsAPI(serviceMap map[string]uint32) []corev1.ServicePort {
	services := []string{}
	for service := range serviceMap {
		services = append(services, service)
	}
	// Sort the services. It's important to have deterministic output for testing purposes.
	sort.Strings(services)
	ports := []corev1.ServicePort{}
	for _, serviceName := range services {
		servicePort := serviceMap[serviceName]
		// serviceName is the service name concatenated with the protocol type
		ports = append(ports, corev1.ServicePort{
			Name:     serviceName,
			Protocol: corev1.ProtocolTCP,
			Port:     int32(servicePort),
		})
	}
	return ports
}
