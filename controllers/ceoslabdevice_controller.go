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
	stderrors "errors"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ceoslabv1alpha1 "gitlab.aristanetworks.com/ofrasier/arista-ceoslab-operator/api/v1alpha1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CEosLabDeviceReconciler reconciles a CEosLabDevice object
type CEosLabDeviceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Helpers for updating status
func (r *CEosLabDeviceReconciler) updateDeviceFail(ctx context.Context, device *ceoslabv1alpha1.CEosLabDevice, errMsg string) {
	device.Status.State = "failed"
	device.Status.Reason = errMsg
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceReconciling(ctx context.Context, device *ceoslabv1alpha1.CEosLabDevice, msg string) {
	device.Status.State = "reconciling"
	device.Status.Reason = msg
	r.Update(ctx, device)
}

func (r *CEosLabDeviceReconciler) updateDeviceSuccess(ctx context.Context, device *ceoslabv1alpha1.CEosLabDevice) {
	device.Status.State = "success"
	device.Status.Reason = ""
	r.Update(ctx, device)
}

//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ceoslab.arista.com,resources=ceoslabdevices/finalizers,verbs=update

// We need permission to access deployments, see https://github.com/operator-framework/operator-sdk/issues/4059
//+kubebuilder:rbac:groups=core,resources=deployments,verbs=get;list;watch;create;update;patch;delete

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
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get CEosLabDevice")
		return ctrl.Result{}, err
	}

	// Check if the deployment for this device already exists, if not create a new one
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: device.Name, Namespace: device.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		// Define a new deployment
		dep := r.getDeployment(device)
		msg := fmt.Sprintf("Creating deployment for CEosLabDevice %s", device.Name)
		log.Info(msg)
		err = r.Create(ctx, dep)
		if err != nil {
			errMsg := fmt.Sprintf("Failed to create deployment for CEosLabDevice %s", device.Name)
			log.Error(err, errMsg)
			r.updateDeviceFail(ctx, device, errMsg)
			return ctrl.Result{}, err
		}
		// Deployment created successfully - return and requeue
		log.Info(fmt.Sprintf("Created deployment for CEosLabDevice %s: %v", device.Name, dep))
		r.updateDeviceReconciling(ctx, device, msg)
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil {
		errMsg := fmt.Sprintf("Failed to get deployment for CEosLabDevice %s", device.Name)
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
		return ctrl.Result{}, err
	}

	// Ensure we have only a single container configured for this deployment
	deploymentContainers := found.Spec.Template.Spec.Containers
	numContainers := len(deploymentContainers)
	if numContainers != 1 {
		errMsg := fmt.Sprintf("Deployment for CEosLabDevice %s has %d containers instead of 1",
			device.Name, numContainers)
		err := stderrors.New("Too many containers")
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
		return ctrl.Result{}, err
	}

	// Ensure the deployment labels are the same as the metadata
	labels := getLabels(device)
	// deploymentMatchLabels and deploymentTemplateLabels should be identical.
	deploymentMatchLabels := found.Spec.Selector.MatchLabels
	deploymentTemplateLabels := found.Spec.Template.Labels
	if !reflect.DeepEqual(deploymentMatchLabels, deploymentTemplateLabels) {
		errMsg := fmt.Sprintf("Deployment for CEosLabDevice %s template and match labels not equal, template: %v, match: %v",
			device.Name, deploymentTemplateLabels, deploymentMatchLabels)
		err := stderrors.New("Label mismatch")
		log.Error(err, errMsg)
		r.updateDeviceFail(ctx, device, errMsg)
		return ctrl.Result{}, err
	}
	if !reflect.DeepEqual(labels, deploymentMatchLabels) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s deployment labels, new: %v, old: %v",
			device.Name, labels, deploymentMatchLabels)
		log.Info(msg)
		// Update deployment
		found.Spec.Selector.MatchLabels = labels
		found.Spec.Template.Labels = labels
		r.Update(ctx, found)
		// Update status
		r.updateDeviceReconciling(ctx, device, msg)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Ensure the deployment environment variables are the same as the spec
	container := &deploymentContainers[0]
	envVarsMap := getEnvVarsMap(device)
	containerEnvVars := map[string]string{}
	for _, envvar := range container.Env {
		containerEnvVars[envvar.Name] = envvar.Value
	}
	if !reflect.DeepEqual(envVarsMap, containerEnvVars) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s deployment environment variables, new: %v, old: %v",
			device.Name, envVarsMap, containerEnvVars)
		log.Info(msg)
		// Update deployment
		env := getEnvVarsCore(envVarsMap)
		command := getEnvVarsCommand(envVarsMap)
		container.Env = env
		container.Command = command
		r.Update(ctx, found)
		// Update status
		r.updateDeviceReconciling(ctx, device, msg)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Ensure the deployment image is the same as the spec
	specImage := getImage(device)
	containerImage := container.Image
	if specImage != containerImage {
		msg := fmt.Sprintf("Updating CEosLabDevice %s deployment image, new: %s, old: %s",
			device.Name, specImage, containerImage)
		log.Info(msg)
		// Update deployment
		container.Image = specImage
		r.Update(ctx, found)
		// Update status
		r.updateDeviceReconciling(ctx, device, msg)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Ensure the deployment ports are the same as the spec
	specPorts := getPorts(device)
	containerPorts := container.Ports
	if !reflect.DeepEqual(specPorts, containerPorts) {
		msg := fmt.Sprintf("Updating CEosLabDevice %s deployment ports, new %v, old; %v",
			device.Name, specPorts, containerPorts)
		log.Info(msg)
		// Update deployment
		container.Ports = specPorts
		r.Update(ctx, found)
		// Update status
		r.updateDeviceReconciling(ctx, device, msg)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Device reconciled
	log.Info(fmt.Sprintf("CEosLabDevice %s reconciled", device.Name))
	r.updateDeviceSuccess(ctx, device)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CEosLabDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ceoslabv1alpha1.CEosLabDevice{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}

func (r *CEosLabDeviceReconciler) getDeployment(device *ceoslabv1alpha1.CEosLabDevice) *appsv1.Deployment {
	labels := getLabels(device)
	envVarsMap := getEnvVarsMap(device)
	env := getEnvVarsCore(envVarsMap)
	command := getEnvVarsCommand(envVarsMap)
	image := getImage(device)
	ports := getPorts(device)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      device.Name,
			Namespace: device.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Image:   image,
						Name:    "ceos",
						Command: command,
						Env:     env,
						Ports:   ports,
					}},
				},
			},
		},
	}
	ctrl.SetControllerReference(device, dep, r.Scheme)
	return dep
}

func getEnvVarsMap(device *ceoslabv1alpha1.CEosLabDevice) map[string]string {
	envVars := map[string]string{
		// Default values
		"CEOS":                                "1",
		"EOS_PLATFORM":                        "ceoslab",
		"container":                           "docker",
		"ETBA":                                "1",
		"SKIP_ZEROTOUCH_BARRIER_IN_SYSDBINIT": "1",
		"INTFTYPE":                            "eth",
	}
	for varname, val := range device.Spec.EnvVar {
		// Add new environment variables, possibly overriding defaults
		envVars[varname] = val
	}
	if device.Spec.Hostname != "" {
		// Hostnames are configured by environment variable and the seperate property is
		// just sugar
		envVars["HOSTNAME"] = device.Spec.Hostname
	} else {
		envVars["HOSTNAME"] = device.Name
	}
	return envVars
}

func getEnvVarsCore(envVarsMap map[string]string) []corev1.EnvVar {
	envVars := []corev1.EnvVar{}
	for varname, val := range envVarsMap {
		envVars = append(envVars, corev1.EnvVar{
			Name:  varname,
			Value: val,
		})
	}
	return envVars
}

func getEnvVarsCommand(envVarsMap map[string]string) []string {
	command := []string{"/sbin/init"}
	for varname, val := range envVarsMap {
		command = append(command, "systemd.setenv="+varname+"="+val)
	}
	return command
}

func getLabels(device *ceoslabv1alpha1.CEosLabDevice) map[string]string {
	labels := map[string]string{
		// Defaults
		"app":  "ceoslabdevice",
		"name": device.Name,
	}
	for label, value := range device.Labels {
		// Extra labels attached to CR, such as model, version, and OS
		labels[label] = value
	}
	return labels
}

func getImage(device *ceoslabv1alpha1.CEosLabDevice) string {
	image := device.Spec.Image
	if image == "" {
		image = "ceos:latest"
	}
	return image
}

func getPorts(device *ceoslabv1alpha1.CEosLabDevice) []corev1.ContainerPort {
	portMap := map[string]int32{
		// Defaults
		"ssh":  22,
		"ssl":  443,
		"gnmi": 6030,
	}
	for service, port := range device.Spec.Ports {
		// Extra ports, possibly overriding defaults
		portMap[service] = port
	}
	ports := []corev1.ContainerPort{}
	for service, port := range portMap {
		ports = append(ports, corev1.ContainerPort{
			Name:          service,
			ContainerPort: port,
			Protocol:      "TCP",
		})
	}
	return ports
}
