// Copyright (c) 2017 Chef Software Inc. and/or applicable contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	apiv1 "k8s.io/client-go/pkg/api/v1"
	appsv1beta1 "k8s.io/client-go/pkg/apis/apps/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	crv1 "github.com/kinvolk/habitat-operator/pkg/habitat/apis/cr/v1"
)

const (
	resyncPeriod = 1 * time.Minute

	userTomlFile = "user.toml"
	configMapDir = "/habitat-operator"

	peerFilename  = "peer-ip"
	peerFile      = "peer-watch-file"
	configMapName = peerFile

	// The key under which the ring key is stored in the Kubernetes Secret.
	ringSecretKey = "ring-key"
	// The extension of the key file.
	ringKeyFileExt = "sym.key"
	// Keys are saved to disk with the format `<name>-<revision>.<extension>`.
	// This regexp captures the name part.
	ringKeyRegexp = `^([\w_-]+)-\d{14}$`
)

var ringRegexp *regexp.Regexp = regexp.MustCompile(ringKeyRegexp)

type HabitatController struct {
	config Config
	logger log.Logger

	// queue contains the jobs that will be handled by syncServiceGroup.
	// A workqueue.RateLimitingInterface is a queue where failing jobs are re-enqueued with an exponential
	// delay, so that jobs in a crashing loop don't fill the queue.
	queue workqueue.RateLimitingInterface

	sgInformer         cache.SharedIndexInformer
	deploymentInformer cache.SharedIndexInformer
	secretInformer     cache.SharedIndexInformer
	configMapInformer  cache.SharedIndexInformer
	// Pod cache to use in events and other places where we get. And only use get from api for running pods func.
	podInformer cache.SharedIndexInformer
}

type Config struct {
	HabitatClient       *rest.RESTClient
	KubernetesClientset *kubernetes.Clientset
	Scheme              *runtime.Scheme
}

func New(config Config, logger log.Logger) (*HabitatController, error) {
	if config.HabitatClient == nil {
		return nil, errors.New("invalid controller config: no HabitatClient")
	}
	if config.KubernetesClientset == nil {
		return nil, errors.New("invalid controller config: no KubernetesClientset")
	}
	if config.Scheme == nil {
		return nil, errors.New("invalid controller config: no Schema")
	}
	if logger == nil {
		return nil, errors.New("invalid controller config: no logger")
	}

	hc := &HabitatController{
		config: config,
		logger: logger,
		queue:  workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "servicegroup"),
	}

	return hc, nil
}

// Run starts a Habitat resource controller.
func (hc *HabitatController) Run(ctx context.Context) error {
	level.Info(hc.logger).Log("msg", "Watching Service Group objects")

	hc.cacheSG()
	hc.cacheDeployment()
	hc.cacheSecret()
	hc.cacheConfigMap()
	hc.cachePods()

	// TODO: should all of them have the same ctx?
	go hc.sgInformer.Run(ctx.Done())
	go hc.deploymentInformer.Run(ctx.Done())
	go hc.secretInformer.Run(ctx.Done())
	go hc.configMapInformer.Run(ctx.Done())
	go hc.podInformer.Run(ctx.Done())

	// Start the synchronous queue consumer.
	go hc.worker()

	// This channel is closed when the context is canceled or times out.
	<-ctx.Done()

	// Err() contains the error, if any.
	return ctx.Err()
}

func (hc *HabitatController) cacheSG() {
	source := cache.NewListWatchFromClient(
		hc.config.HabitatClient,
		crv1.ServiceGroupResourcePlural,
		apiv1.NamespaceAll,
		fields.Everything())

	hc.sgInformer = cache.NewSharedIndexInformer(
		source,
		&crv1.ServiceGroup{},
		resyncPeriod,
		cache.Indexers{},
	)

	hc.sgInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    hc.handleSGAdd,
		UpdateFunc: hc.handleSGUpdate,
		DeleteFunc: hc.handleSGDelete,
	})
}

func (hc *HabitatController) cacheDeployment() {
	// TODO: Do we need to add field selector/label selector.
	source := cache.NewListWatchFromClient(
		hc.config.KubernetesClientset.AppsV1beta1().RESTClient(),
		"deployments",
		apiv1.NamespaceAll,
		fields.Everything())

	hc.deploymentInformer = cache.NewSharedIndexInformer(
		source,
		&appsv1beta1.Deployment{},
		resyncPeriod,
		cache.Indexers{},
	)

	hc.deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    hc.handleDeployAdd,
		UpdateFunc: hc.handleDeployUpdate,
		DeleteFunc: hc.handleDeployDelete,
	})
}

func (hc *HabitatController) cacheSecret() {
	source := cache.NewListWatchFromClient(
		hc.config.KubernetesClientset.Core().RESTClient(),
		"secrets",
		apiv1.NamespaceAll,
		fields.Everything())

	hc.secretInformer = cache.NewSharedIndexInformer(
		source,
		&apiv1.Secret{},
		resyncPeriod,
		cache.Indexers{},
	)

	hc.secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    hc.handleSecretAdd,
		UpdateFunc: hc.handleSecretUpdate,
		DeleteFunc: hc.handleSecretDelete,
	})
}

func (hc *HabitatController) cacheConfigMap() {
	ls := labels.SelectorFromSet(labels.Set(map[string]string{
		crv1.HabitatLabel:  "true",
		crv1.TopologyLabel: "leader",
	}))

	source := newListWatchFromClientWithLabels(
		hc.config.KubernetesClientset.CoreV1().RESTClient(),
		"configmaps",
		apiv1.NamespaceAll,
		ls)

	hc.configMapInformer = cache.NewSharedIndexInformer(
		source,
		&apiv1.ConfigMap{},
		resyncPeriod,
		cache.Indexers{},
	)

	hc.configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    hc.handleCMAdd,
		UpdateFunc: hc.handleCMUpdate,
		DeleteFunc: hc.handleCMDelete,
	})
}

func (hc *HabitatController) cachePods() {
	ls := labels.SelectorFromSet(labels.Set(map[string]string{"habitat": "true"}))

	source := newListWatchFromClientWithLabels(
		hc.config.KubernetesClientset.CoreV1().RESTClient(),
		"pods",
		apiv1.NamespaceAll,
		ls)

	hc.podInformer = cache.NewSharedIndexInformer(
		source,
		&apiv1.Pod{},
		resyncPeriod,
		cache.Indexers{},
	)

	hc.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    hc.onPodAdd,
		UpdateFunc: hc.onPodUpdate,
		DeleteFunc: hc.onPodDelete,
	})
}
func (hc *HabitatController) handleSGAdd(obj interface{}) {
	hc.enqueue(obj)
}

func (hc *HabitatController) handleSGUpdate(oldObj, newObj interface{}) {
	fmt.Println("update SG")
	fmt.Println(oldObj)
	fmt.Println(newObj)
	oldSG, ok := oldObj.(*crv1.ServiceGroup)
	if !ok {
		level.Error(hc.logger).Log("msg", "Failed to type assert ServiceGroup", "obj", oldObj)
		return
	}

	newSG, ok := newObj.(*crv1.ServiceGroup)
	if !ok {
		level.Error(hc.logger).Log("msg", "Failed to type assert ServiceGroup", "obj", newObj)
		return
	}

	if hc.serviceGroupNeedsUpdate(oldSG, newSG) {
		hc.enqueue(newSG)
	}
}

func (hc *HabitatController) handleSGDelete(obj interface{}) {
	hc.enqueue(obj)
}

func (hc *HabitatController) handleDeployAdd(obj interface{}) {
	// Check if current deployment is of interest,
	d, ok := obj.(*appsv1beta1.Deployment)
	if !ok {
		level.Error(hc.logger).Log("msg", "Failed to type assert deployment", "obj", obj)
		return
	}

	if d.ObjectMeta.Labels["habitat"] == "true" {
		fmt.Println("deployment")
		hc.enqueue(obj)
	}
	fmt.Println("no deployment added")
}

func (hc *HabitatController) handleDeployUpdate(oldObj, newObj interface{}) {
	// check if it belongs to a SG, by label.
	d, ok := newObj.(*appsv1beta1.Deployment)
	if !ok {
		level.Error(hc.logger).Log("msg", "Failed to type assert deployment", "obj", newObj)
		return
	}

	if d.ObjectMeta.Labels["habitat"] == "true" {
		fmt.Println("deployment")
		hc.enqueue(newObj)
	}
}

func (hc *HabitatController) handleDeployDelete(obj interface{}) {
	// check if it belongs to a SG, by label.
	d, ok := obj.(*appsv1beta1.Deployment)
	if !ok {
		level.Error(hc.logger).Log("msg", "Failed to type assert deployment", "obj", obj)
		return
	}

	if d.ObjectMeta.Labels["habitat"] == "true" {
		fmt.Println("deployment")
		hc.enqueue(obj)
	}
}

func (hc *HabitatController) handleSecretAdd(obj interface{}) {
	// check if it belongs to a SG, by label.
	d := obj.(*apiv1.Secret)
	if d.ObjectMeta.Labels["habitat"] == "true" {
		fmt.Println("deployment")
		hc.enqueue(obj)
	}
}

func (hc *HabitatController) handleSecretUpdate(oldObj, newObj interface{}) {
	// check if it belongs to a SG, by label.
	d := newObj.(*apiv1.Secret)
	if d.ObjectMeta.Labels["habitat"] == "true" {
		fmt.Println("deployment")
		hc.enqueue(newObj)
	}
}

func (hc *HabitatController) handleSecretDelete(obj interface{}) {
	// check if it belongs to a SG, by label.
	d := obj.(*apiv1.Secret)
	if d.ObjectMeta.Labels["habitat"] == "true" {
		fmt.Println("deployment")
		hc.enqueue(obj)
	}
}

func (hc *HabitatController) handleCMAdd(obj interface{}) {
	// TODO:
	// check if it belongs to a SG, by label.

	// if it does then enqueue thi key.
	// if it doesnt ignore.
	fmt.Println("add cm")
	fmt.Println(obj)
	hc.enqueue(obj)
}

func (hc *HabitatController) handleCMUpdate(oldObj, newObj interface{}) {
	// TODO:
	// check if it belongs to a SG, by label.

	// if it does then enqueue thi key.
	// if it doesnt ignore.
	fmt.Println("update cm")
	fmt.Println(oldObj)
	fmt.Println(newObj)
	hc.enqueue(newObj)
}

func (hc *HabitatController) handleCMDelete(obj interface{}) {
	// TODO:
	// check if it belongs to a SG, by label.

	// if it does then enqueue thi key.
	// if it doesnt ignore.
	fmt.Println("delete cm")
	fmt.Println(obj)
	hc.enqueue(obj)
}

func (hc *HabitatController) handleServiceGroupCreation(sg *crv1.ServiceGroup) error {
	level.Debug(hc.logger).Log("function", "handleServiceGroupCreation", "msg", sg.ObjectMeta.SelfLink)

	// Validate object.
	if err := validateCustomObject(*sg); err != nil {
		return err
	}

	level.Debug(hc.logger).Log("msg", "validated object")

	deployment, err := hc.newDeployment(sg)
	if err != nil {
		return err
	}

	// Create Deployment, if it doesn't already exist.
	//var d *appsv1beta1.Deployment

	_, err = hc.config.KubernetesClientset.AppsV1beta1Client.Deployments(sg.Namespace).Create(deployment)
	if err != nil {
		// Was the error due to the Deployment already existing?
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		// TODO: take from deployments cache
		_, _, err := hc.deploymentInformer.GetStore().GetByKey(deployment.Name)
		//d, err = hc.config.KubernetesClientset.AppsV1beta1Client.Deployments(sg.Namespace).Get(deployment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		level.Debug(hc.logger).Log("msg", "deployment already existed", "name", deployment.Name)
	} else {
		level.Info(hc.logger).Log("msg", "created deployment", "name", deployment.Name)
	}

	// Handle creation/updating of peer IP ConfigMap.
	if err := hc.handleConfigMap(sg); err != nil {
		return err
	}

	return nil
}

func (hc *HabitatController) getRunningPods(namespace string) ([]apiv1.Pod, error) {
	fs := fields.SelectorFromSet(fields.Set{
		"status.phase": "Running",
	})
	ls := fields.SelectorFromSet(fields.Set(map[string]string{
		crv1.HabitatLabel: "true",
	}))

	running := metav1.ListOptions{
		FieldSelector: fs.String(),
		LabelSelector: ls.String(),
	}

	pods, err := hc.config.KubernetesClientset.CoreV1Client.Pods(namespace).List(running)
	if err != nil {
		return nil, err
	}

	return pods.Items, nil
}

func (hc *HabitatController) writeLeaderIP(cm *apiv1.ConfigMap, ip string) error {
	cm.Data[peerFile] = ip

	if _, err := hc.config.KubernetesClientset.CoreV1().ConfigMaps(cm.Namespace).Update(cm); err != nil {
		return err
	}

	return nil
}

func (hc *HabitatController) handleConfigMap(sg *crv1.ServiceGroup) error {
	runningPods, err := hc.getRunningPods(sg.Namespace)
	if err != nil {
		return err
	}

	if len(runningPods) == 0 {
		// No running Pods, create an empty ConfigMap.
		newCM := newConfigMap(sg.Name, "")

		cm, err := hc.config.KubernetesClientset.CoreV1().ConfigMaps(sg.Namespace).Create(newCM)
		if err != nil {
			// Was the error due to the ConfigMap already existing?
			if !apierrors.IsAlreadyExists(err) {
				return err
			}

			// Delete the IP in the existing ConfigMap, as it must necessarily be invalid,
			// since there are no running Pods.
			// TODO: take from configmap cache
			cm, err = hc.config.KubernetesClientset.CoreV1Client.ConfigMaps(sg.Namespace).Get(newCM.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}

			if err := hc.writeLeaderIP(cm, ""); err != nil {
				return err
			}
		}

		level.Info(hc.logger).Log("msg", "created peer IP ConfigMap", "name", newCM.Name)

		return nil
	}

	// There are running Pods, add the IP of one of them to the ConfigMap.
	leaderIP := runningPods[0].Status.PodIP

	newCM := newConfigMap(sg.Name, leaderIP)

	cm, err := hc.config.KubernetesClientset.CoreV1Client.ConfigMaps(sg.Namespace).Create(newCM)
	if err != nil {
		// Was the error due to the ConfigMap already existing?
		if !apierrors.IsAlreadyExists(err) {
			return err
		}

		// The ConfigMap already exists. Is the leader still running?
		// Was the error due to the ConfigMap already existing?
		// TODO: take from cm cache
		cm, err = hc.config.KubernetesClientset.CoreV1Client.ConfigMaps(sg.Namespace).Get(newCM.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		curLeader := cm.Data[peerFile]

		for _, p := range runningPods {
			if p.Status.PodIP == curLeader {
				// The leader is still up, nothing to do.
				level.Debug(hc.logger).Log("msg", "Leader still running", "ip", curLeader)

				return nil
			}
		}

		// The leader is not in the list of running Pods, so the ConfigMap must be updated.
		if err := hc.writeLeaderIP(cm, leaderIP); err != nil {
			return err
		}

		level.Info(hc.logger).Log("msg", "updated peer IP in ConfigMap", "name", cm.Name, "ip", leaderIP)
	} else {
		level.Info(hc.logger).Log("msg", "created peer IP ConfigMap", "name", cm.Name, "ip", leaderIP)
	}

	return nil
}

func (hc *HabitatController) handleServiceGroupDeletion(key string) error {
	// Delete deployment.
	deploymentNS, deploymentName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	deploymentsClient := hc.config.KubernetesClientset.AppsV1beta1Client.Deployments(deploymentNS)

	// With this policy, dependent resources will be deleted, but we don't wait
	// for that to happen.
	deletePolicy := metav1.DeletePropagationBackground
	deleteOptions := &metav1.DeleteOptions{
		PropagationPolicy: &deletePolicy,
	}

	if err := deploymentsClient.Delete(deploymentName, deleteOptions); err != nil {
		level.Error(hc.logger).Log("msg", err)
		return err
	}

	level.Info(hc.logger).Log("msg", "deleted deployment", "name", deploymentName)

	return nil
}

func (hc *HabitatController) onPodAdd(obj interface{}) {

}

func (hc *HabitatController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod, ok1 := oldObj.(*apiv1.Pod)
	if !ok1 {
		level.Error(hc.logger).Log("msg", "Failed to type assert pod", "obj", oldObj)
		return
	}

	newPod, ok2 := newObj.(*apiv1.Pod)
	if !ok2 {
		level.Error(hc.logger).Log("msg", "Failed to type assert pod", "obj", newObj)
		return
	}

	if !hc.podNeedsUpdate(oldPod, newPod) {
		return
	}

	sg, err := hc.getServiceGroupFromPod(newPod)
	if err != nil {
		if sgErr, ok := err.(serviceGroupNotFoundError); !ok {
			level.Error(hc.logger).Log("msg", sgErr)
			return
		}

		// This only means the Pod and the ServiceGroup watchers are not in sync.
		level.Debug(hc.logger).Log("msg", "ServiceGroup not found", "function", "onPodUpdate")

		return
	}

	hc.enqueue(sg)
}

func (hc *HabitatController) onPodDelete(obj interface{}) {
	pod, ok := obj.(*apiv1.Pod)
	if !ok {
		level.Error(hc.logger).Log("msg", "Failed to type assert pod", "obj", obj)
		return
	}

	sg, err := hc.getServiceGroupFromPod(pod)
	if err != nil {
		if sgErr, ok := err.(serviceGroupNotFoundError); !ok {
			level.Error(hc.logger).Log("msg", sgErr)
			return
		}

		// This only means the Pod and the ServiceGroup watchers are not in sync.
		level.Debug(hc.logger).Log("msg", "ServiceGroup not found", "function", "onPodDelete")

		return
	}

	hc.enqueue(sg)
}

func (hc *HabitatController) newDeployment(sg *crv1.ServiceGroup) (*appsv1beta1.Deployment, error) {
	// This value needs to be passed as a *int32, so we convert it, assign it to a
	// variable and afterwards pass a pointer to it.
	count := int32(sg.Spec.Count)

	// Set the service arguments we send to Habitat.
	var habArgs []string
	if sg.Spec.Habitat.Group != "" {
		// When a service is started without explicitly naming the group,
		// it's assigned to the default group.
		habArgs = append(habArgs,
			"--group", sg.Spec.Habitat.Group)
	}

	// As we want to label our pods with the
	// topology type we set standalone as the default one.
	// We do not need to pass this to habitat, as if no topology
	// is set, habitat by default sets standalone topology.
	topology := crv1.TopologyStandalone

	if sg.Spec.Habitat.Topology == crv1.TopologyLeader {
		topology = crv1.TopologyLeader
	}

	path := fmt.Sprintf("%s/%s", configMapDir, peerFilename)

	habArgs = append(habArgs,
		"--topology", topology.String(),
		"--peer-watch-file", path,
	)

	// Runtime binding.
	// One Service connects to another forming a producer/consumer relationship.
	for _, bind := range sg.Spec.Habitat.Bind {
		// Pass --bind flag.
		bindArg := fmt.Sprintf("%s:%s.%s", bind.Name, bind.Service, bind.Group)
		habArgs = append(habArgs,
			"--bind", bindArg)
	}

	base := &appsv1beta1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: sg.Name,
		},
		Spec: appsv1beta1.DeploymentSpec{
			Replicas: &count,
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						crv1.HabitatLabel:      "true",
						crv1.ServiceGroupLabel: sg.Name,
						crv1.TopologyLabel:     topology.String(),
					},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						{
							Name:  "habitat-service",
							Image: sg.Spec.Image,
							Args:  habArgs,
							VolumeMounts: []apiv1.VolumeMount{
								{
									Name:      "config",
									MountPath: configMapDir,
									ReadOnly:  true,
								},
							},
						},
					},
					// Define the volume for the ConfigMap.
					Volumes: []apiv1.Volume{
						{
							Name: "config",
							VolumeSource: apiv1.VolumeSource{
								ConfigMap: &apiv1.ConfigMapVolumeSource{
									LocalObjectReference: apiv1.LocalObjectReference{
										Name: configMapName,
									},
									Items: []apiv1.KeyToPath{
										{
											Key:  peerFile,
											Path: peerFilename,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	// If we have a secret name present we should mount that secret.
	if sg.Spec.Habitat.Config != "" {
		// Let's make sure our secret is there before mounting it.
		// TODO: take from secrets cache
		secret, err := hc.config.KubernetesClientset.CoreV1().Secrets(sg.Namespace).Get(sg.Spec.Habitat.Config, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		secretVolume := &apiv1.Volume{
			Name: "initialconfig",
			VolumeSource: apiv1.VolumeSource{
				Secret: &apiv1.SecretVolumeSource{
					SecretName: secret.Name,
					Items: []apiv1.KeyToPath{
						{
							Key:  userTomlFile,
							Path: userTomlFile,
						},
					},
				},
			},
		}

		secretVolumeMount := &apiv1.VolumeMount{
			Name: "initialconfig",
			// Our user.toml file must be in a directory with the same name as the service.
			MountPath: fmt.Sprintf("/hab/svc/%s", sg.Name),
			ReadOnly:  false,
		}

		base.Spec.Template.Spec.Containers[0].VolumeMounts = append(base.Spec.Template.Spec.Containers[0].VolumeMounts, *secretVolumeMount)
		base.Spec.Template.Spec.Volumes = append(base.Spec.Template.Spec.Volumes, *secretVolume)
	}

	// Handle ring key, if one is specified.
	if ringSecretName := sg.Spec.Habitat.RingSecretName; ringSecretName != "" {
		// TODO: take from secrects cache
		s, err := hc.config.KubernetesClientset.CoreV1().Secrets(apiv1.NamespaceDefault).Get(ringSecretName, metav1.GetOptions{})
		if err != nil {
			level.Error(hc.logger).Log("msg", "Could not find Secret containing ring key")
			return nil, err
		}

		// The filename under which the ring key is saved.
		ringKeyFile := fmt.Sprintf("%s.%s", ringSecretName, ringKeyFileExt)

		// Extract the bare ring name, by removing the revision.
		// Validation has already been performed by this point.
		ringName := ringRegexp.FindStringSubmatch(ringSecretName)[1]

		v := &apiv1.Volume{
			Name: ringSecretName,
			VolumeSource: apiv1.VolumeSource{
				Secret: &apiv1.SecretVolumeSource{
					SecretName: s.Name,
					Items: []apiv1.KeyToPath{
						{
							Key:  ringSecretKey,
							Path: ringKeyFile,
						},
					},
				},
			},
		}

		vm := &apiv1.VolumeMount{
			Name:      ringSecretName,
			MountPath: "/hab/cache/keys",
			// This directory cannot be made read-only, as the supervisor writes to
			// it during its operation.
			ReadOnly: false,
		}

		// Mount ring key file.
		base.Spec.Template.Spec.Volumes = append(base.Spec.Template.Spec.Volumes, *v)
		base.Spec.Template.Spec.Containers[0].VolumeMounts = append(base.Spec.Template.Spec.Containers[0].VolumeMounts, *vm)

		// Add --ring argument to supervisor invocation.
		base.Spec.Template.Spec.Containers[0].Args = append(base.Spec.Template.Spec.Containers[0].Args, "--ring", ringName)
	}

	return base, nil
}

func (hc *HabitatController) enqueue(obj interface{}) {
	k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		level.Error(hc.logger).Log("msg", "Object key could not be retrieved", "object", obj)
		return
	}

	hc.queue.Add(k)
}

func (hc *HabitatController) worker() {
	for hc.processNextItem() {
	}
}

func (hc *HabitatController) processNextItem() bool {
	key, quit := hc.queue.Get()
	if quit {
		return false
	}
	defer hc.queue.Done(key)

	err := hc.conform(key.(string))
	if err != nil {
		level.Error(hc.logger).Log("msg", "ServiceGroup could not be synced, requeueing", "msg", err)

		hc.queue.AddRateLimited(key)

		return true
	}

	hc.queue.Forget(key)

	return true
}

// conform is where the reconciliation takes place.
// It is invoked when any of the following resources get created, updated or deleted.
// ServiceGroup, Pod, Deployment, Secret or ConfigMap.
func (hc *HabitatController) conform(key string) error {
	obj, exists, err := hc.sgInformer.GetStore().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		// The SG was deleted.
		return hc.handleServiceGroupDeletion(key)
	}

	// The ServiceGroup was either created or updated.
	sg, ok := obj.(*crv1.ServiceGroup)
	if !ok {
		return fmt.Errorf("unknown event type")
	}

	level.Debug(hc.logger).Log("function", "handleServiceGroupCreation", "msg", sg.ObjectMeta.SelfLink)

	// Validate object.
	if err := validateCustomObject(*sg); err != nil {
		return err
	}

	level.Debug(hc.logger).Log("msg", "validated object")

	deployment, err := hc.newDeployment(sg)
	if err != nil {
		return err
	}

	// Create Deployment, if it doesn't already exist.
	var d *appsv1beta1.Deployment

	d, err = hc.config.KubernetesClientset.AppsV1beta1Client.Deployments(sg.Namespace).Create(deployment)
	fmt.Println(d)
	if err != nil {
		// Was the error due to the Deployment already existing?
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		// TODO: take from deployments cache
		//_, _, err := hc.deploymentInformer.GetStore().GetByKey(deployment.Name)
		d, err = hc.config.KubernetesClientset.AppsV1beta1Client.Deployments(sg.Namespace).Get(deployment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		level.Debug(hc.logger).Log("msg", "deployment already existed", "name", deployment.Name)
	} else {
		level.Info(hc.logger).Log("msg", "created deployment", "name", deployment.Name)
	}

	// Handle creation/updating of peer IP ConfigMap.
	if err := hc.handleConfigMap(sg); err != nil {
		return err
	}

	return nil
}

func (hc *HabitatController) serviceGroupNeedsUpdate(oldSG, newSG *crv1.ServiceGroup) bool {
	if reflect.DeepEqual(oldSG.Spec, newSG.Spec) {
		level.Debug(hc.logger).Log("msg", "Update ignored as it didn't change ServiceGroup spec", "sg", newSG)
		return false
	}

	return true
}

func (hc *HabitatController) podNeedsUpdate(oldPod, newPod *apiv1.Pod) bool {
	// Ignore identical objects.
	// https://github.com/kubernetes/kubernetes/blob/7e630154dfc7b2155f8946a06f92e96e268dcbcd/pkg/controller/replicaset/replica_set.go#L276-L277
	if oldPod.ResourceVersion == newPod.ResourceVersion {
		level.Debug(hc.logger).Log("msg", "Update ignored as it didn't change Pod resource version", "pod", newPod)
		return false
	}

	// Ignore changes that don't change the Pod's status.
	if oldPod.Status.Phase == newPod.Status.Phase {
		level.Debug(hc.logger).Log("msg", "Update ignored as it didn't change Pod status", "pod", newPod)
		return false
	}

	return true
}

func (hc *HabitatController) getServiceGroupFromPod(pod *apiv1.Pod) (*crv1.ServiceGroup, error) {
	key := serviceGroupKeyFromPod(pod)

	obj, exists, err := hc.sgInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, serviceGroupNotFoundError{key: key}
	}

	sg, ok := obj.(*crv1.ServiceGroup)
	if !ok {
		return nil, fmt.Errorf("unknown object type in Service Group cache: %v", obj)
	}

	return sg, nil
}

func newConfigMap(sgName string, ip string) *apiv1.ConfigMap {
	return &apiv1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configMapName,
		},
		Data: map[string]string{
			peerFile: ip,
		},
	}
}

func serviceGroupKeyFromPod(pod *apiv1.Pod) string {
	sgName := pod.Labels[crv1.ServiceGroupLabel]

	key := fmt.Sprintf("%s/%s", pod.Namespace, sgName)

	return key
}
