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
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ceoslabv1alpha1 "gitlab.aristanetworks.com/ofrasier/arista-ceoslab-operator/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/utils/pointer"
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

	requeue   = ctrl.Result{Requeue: true}
	noRequeue = ctrl.Result{}
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the CEosLabDevice object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
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
		return noRequeue, err
	}

	// Check if the pod for this device already exists, if not create a new one
	foundPod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Name: device.Name, Namespace: device.Namespace}, foundPod)
	if err != nil && errors.IsNotFound(err) {
		// Define a new pod
		msg := fmt.Sprintf("Creating pod for CEosLabDevice %s", device.Name)
		log.Info(msg)
		doPodError := func(err error) {
			errMsg := fmt.Sprintf("Failed to create pod for CEosLabDevice %s", device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(ctx, device, errMsg)
		}
		newPod, err := r.getPod(device)
		if err != nil {
			doPodError(err)
			return noRequeue, err
		}
		err = r.Create(ctx, newPod)
		if err != nil {
			doPodError(err)
			return noRequeue, err
		}
		// Pod created successfully - return and requeue
		log.Info(fmt.Sprintf("Created pod for CEosLabDevice %s: %v", device.Name, newPod))
		r.updateDeviceReconciling(ctx, device, msg)
		return requeue, nil
	} else if err != nil {
		errMsg := fmt.Sprintf("Failed to get pod for CEosLabDevice %s", device.Name)
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
		return noRequeue, err
	}

	// Check if pod services already exist, if not create new ones
	foundServices := &corev1.Service{}
	err = r.Get(ctx,
		types.NamespacedName{
			Name:      fmt.Sprintf("service-%s", device.Name),
			Namespace: device.Namespace,
		}, foundServices)
	if err != nil && errors.IsNotFound(err) {
		msg := fmt.Sprintf("Creating new services for CEosLabDevice %s", device.Name)
		log.Info(msg)
		doServiceError := func(err error) {
			errMsg := fmt.Sprintf("Failed to create services for CEosLabDevice %s", device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(ctx, device, errMsg)
		}
		newService, err := r.getService(device)
		if err != nil {
			doServiceError(err)
			return noRequeue, err
		}
		err = r.Create(ctx, newService)
		if err != nil {
			doServiceError(err)
			return noRequeue, err
		}
		// Service create successfully - return and requeue
		log.Info(fmt.Sprintf("Created service for CEosLabDevice %s: %v", device.Name, newService))
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
	podLabels := foundPod.ObjectMeta.Labels
	if !reflect.DeepEqual(labels, podLabels) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod labels, new: %v, old: %v",
			device.Name, labels, podLabels)
		log.Info(msg)
		// Update pod
		foundPod.ObjectMeta.Labels = labels
		r.Update(ctx, foundPod)
		// Update status
		r.updateDeviceReconciling(ctx, device, msg)
		return requeue, nil
	}

	// Ensure the pod environment variables are the same as the spec
	container := foundPod.Spec.Containers[0]
	envVarsMap := getEnvVarsMap(device)
	containerEnvVars := getEnvVarsMapFromCorev1(container.Env)
	if !reflect.DeepEqual(envVarsMap, containerEnvVars) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod environment variables, new: %v, old: %v",
			device.Name, envVarsMap, containerEnvVars)
		log.Info(msg)
		// Environment variables can only be changed by restarting the pod
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(ctx, foundPod, device, msg)
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
		r.updateDeviceReconcilingWithRestart(ctx, foundPod, device, msg)
		return requeue, nil
	}

	// Ensure the pod resource requirements are same as the spec
	specResourceRequirements, err := getResourceMap(device)
	containerResourceRequirements := getResourceMapFromCorev1(&container.Resources)
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
		r.updateDeviceReconcilingWithRestart(ctx, foundPod, device, msg)
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
		r.updateDeviceReconcilingWithRestart(ctx, foundPod, device, msg)
		return requeue, nil
	}

	// Ensure the pod init container args are correct.
	initContainer := foundPod.Spec.InitContainers[0]
	specInitContainerArgs := strSliceToMap(getInitContainerArgs(device))
	podInitContainerArgs := strSliceToMap(initContainer.Args)
	if !reflect.DeepEqual(specInitContainerArgs, podInitContainerArgs) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod init container arguments, new: %s, old: %s",
			device.Name, specInitContainerArgs, podInitContainerArgs)
		log.Info(msg)
		// Init container args can only be changed by reconciling
		// Delete the pod, and requeue to recreate
		r.updateDeviceReconcilingWithRestart(ctx, foundPod, device, msg)
		return requeue, nil
	}

	// Ensure services are the same as the spec
	specServiceMap, err := getServiceMap(device)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to validate CEosLabDevice %s pod services, new: %v, old: %v",
			device.Name, device.Spec.Services, foundServices)
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
	}
	containerServiceMap := getServiceMapFromCorev1(foundServices)
	if !reflect.DeepEqual(specServiceMap, containerServiceMap) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s pod services, new: %v, old; %v",
			device.Name, specServiceMap, containerServiceMap)
		log.Info(msg)
		// Update pod
		foundServices.Spec.Ports = getServicePortsCore(specServiceMap)
		r.Update(ctx, foundServices)
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

func (r *CEosLabDeviceReconciler) getPod(device *ceoslabv1alpha1.CEosLabDevice) (*corev1.Pod, error) {
	labels := getLabels(device)
	envVarsMap := getEnvVarsMap(device)
	env := getEnvVarsCore(device, envVarsMap)
	command := getCommand(device)
	args := strMapToSlice(getArgsMap(device, envVarsMap))
	image := getImage(device)
	resourceMap, err := getResourceMap(device)
	if err != nil {
		return nil, err
	}
	resources := *getResourcesCore(resourceMap)
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
				// Run container in privileged mode
				SecurityContext: &corev1.SecurityContext{Privileged: pointer.Bool(true)},
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
		},
	}
	ctrl.SetControllerReference(device, pod, r.Scheme)
	return pod, nil
}

func (r *CEosLabDeviceReconciler) getService(device *ceoslabv1alpha1.CEosLabDevice) (*corev1.Service, error) {
	serviceMap, err := getServiceMap(device)
	if err != nil {
		return nil, err
	}
	servicePorts := getServicePortsCore(serviceMap)
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
	return service, nil
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

func getEnvVarsCore(device *ceoslabv1alpha1.CEosLabDevice, envVarsMap map[string]string) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}
	for varname, val := range envVarsMap {
		envVars = append(envVars, corev1.EnvVar{
			Name:  varname,
			Value: val,
		})
	}
	return envVars
}

func getEnvVarsMapFromCorev1(envVarsCore []corev1.EnvVar) map[string]string {
	envVars := map[string]string{}
	for _, envvar := range envVarsCore {
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

func getResourceMapFromCorev1(resources *corev1.ResourceRequirements) map[string]string {
	resourceMap := map[string]string{}
	for resourceType, asResource := range resources.Requests {
		canonicalString := asResource.String()
		resourceMap[string(resourceType)] = canonicalString
	}
	return resourceMap
}

func getResourcesCore(resourceMap map[string]string) *corev1.ResourceRequirements {
	requirements := &corev1.ResourceRequirements{
		Requests: map[corev1.ResourceName]resource.Quantity{},
	}
	for resourceType, resourceStr := range resourceMap {
		// resourceStr has already been parsed into a canonical form, so this should never fail.
		requirements.Requests[corev1.ResourceName(resourceType)] = resource.MustParse(resourceStr)
	}
	return requirements
}

type deviceServicePort struct {
	port     int32
	protocol corev1.Protocol
}

func getServiceMap(device *ceoslabv1alpha1.CEosLabDevice) (map[string]deviceServicePort, error) {
	serviceMap := map[string]deviceServicePort{}
	for service, servicePortStr := range device.Spec.Services {
		// Parse service ports froms strings
		errMsg := fmt.Sprintf("Unknown port spec %s, expected \"<port num>:<TCP|UDP>,...\"", servicePortStr)
		splits := strings.Split(servicePortStr, ":")
		if len(splits) != 2 {
			return nil, fmt.Errorf(errMsg)
		}
		// ex. splits := [ "123", "TCP,UDP" ]
		port, err := strconv.Atoi(splits[0])
		if err != nil {
			return nil, fmt.Errorf(errMsg)
		}
		protocols := strings.Split(splits[1], ",")
		if len(protocols) == 0 {
			return nil, fmt.Errorf(errMsg)
		}
		for _, protocol := range protocols {
			corev1Protocol := corev1.Protocol(protocol)
			if !(corev1Protocol == corev1.ProtocolTCP || corev1Protocol == corev1.ProtocolUDP) {
				return nil, fmt.Errorf(errMsg)
			}
			// The core port type only defines one protocol per port, so mangle the service
			// name with the protocol so we can have multiple w/o name collisions.
			// This is probably just paranoia.
			serviceMap[strings.ToLower(service+protocol)] = deviceServicePort{
				port:     int32(port),
				protocol: corev1Protocol,
			}
		}
	}
	return serviceMap, nil
}

func getServiceMapFromCorev1(service *corev1.Service) map[string]deviceServicePort {
	serviceMap := map[string]deviceServicePort{}
	for _, v := range service.Spec.Ports {
		serviceMap[v.Name] = deviceServicePort{
			port:     v.Port,
			protocol: v.Protocol,
		}
	}
	return serviceMap
}

func getServicePortsCore(serviceMap map[string]deviceServicePort) []corev1.ServicePort {
	ports := []corev1.ServicePort{}
	for serviceName, servicePort := range serviceMap {
		// serviceName is the service name concatenated with the protocol type
		ports = append(ports, corev1.ServicePort{
			Name:     serviceName,
			Protocol: servicePort.protocol,
			Port:     servicePort.port,
		})
	}
	return ports
}
