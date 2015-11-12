/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package deployment

import (
	"fmt"
	"math"
	"time"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util"
	deploymentutil "k8s.io/kubernetes/pkg/util/deployment"
	"k8s.io/kubernetes/pkg/util/workqueue"
	"k8s.io/kubernetes/pkg/watch"
)

const (
	// FullDeploymentResyncPeriod means we'll attempt to recompute the required replicas
	// of all deployments that have fulfilled their expectations at least this often.
	// This recomputation happens based on contents in the local caches.
	FullDeploymentResyncPeriod = 30 * time.Second
)

// DeploymentController is responsible for synchronizing Deployment objects stored
// in the system with actual running rcs and pods.
type DeploymentController struct {
	client        client.Interface
	expClient     client.ExtensionsInterface
	eventRecorder record.EventRecorder

	// To allow injection of syncDeployment for testing.
	syncHandler func(dKey string) error

	// A store of deployments, populated by the dController
	dStore cache.StoreToDeploymentLister
	// Watches changes to all deployments
	dController *framework.Controller
	// A store of ReplicaSets, populated by the rsController
	rsStore cache.StoreToReplicaSetLister
	// Watches changes to all ReplicaSets
	rsController *framework.Controller
	// rsStoreSynced returns true if the ReplicaSet store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	rsStoreSynced func() bool
	// A store of pods, populated by the podController
	podStore cache.StoreToPodLister
	// Watches changes to all pods
	podController *framework.Controller
	// podStoreSynced returns true if the pod store has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podStoreSynced func() bool

	// A TTLCache of pod creates/deletes each deployment expects to see
	expectations controller.ControllerExpectationsInterface

	// Deployments that need to be synced
	queue *workqueue.Type
}

// NewDeploymentController creates a new DeploymentController.
func NewDeploymentController(client client.Interface, resyncPeriod controller.ResyncPeriodFunc) *DeploymentController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(client.Events(""))

	dc := &DeploymentController{
		client:        client,
		expClient:     client.Extensions(),
		eventRecorder: eventBroadcaster.NewRecorder(api.EventSource{Component: "deployment-controller"}),
		queue:         workqueue.New(),
		expectations:  controller.NewControllerExpectations(),
	}

	dc.dStore.Store, dc.dController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return dc.expClient.Deployments(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return dc.expClient.Deployments(api.NamespaceAll).Watch(options)
			},
		},
		&extensions.Deployment{},
		FullDeploymentResyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc: dc.enqueueDeployment,
			UpdateFunc: func(old, cur interface{}) {
				// Resync on deployment object relist.
				dc.enqueueDeployment(cur)
			},
			// This will enter the sync loop and no-op, because the deployment has been deleted from the store.
			DeleteFunc: dc.enqueueDeployment,
		},
	)

	dc.rsStore.Store, dc.rsController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return dc.expClient.ReplicaSets(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return dc.expClient.ReplicaSets(api.NamespaceAll).Watch(options)
			},
		},
		&extensions.ReplicaSet{},
		resyncPeriod(),
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dc.addReplicaSet,
			UpdateFunc: dc.updateReplicaSet,
			DeleteFunc: dc.deleteReplicaSet,
		},
	)

	dc.podStore.Store, dc.podController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return dc.client.Pods(api.NamespaceAll).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return dc.client.Pods(api.NamespaceAll).Watch(options)
			},
		},
		&api.Pod{},
		resyncPeriod(),
		framework.ResourceEventHandlerFuncs{
			// When pod updates (becomes ready), we need to enqueue deployment
			UpdateFunc: dc.updatePod,
			// When pod is deleted, we need to update deployment's expectations
			DeleteFunc: dc.deletePod,
		},
	)

	dc.syncHandler = dc.syncDeployment
	dc.rsStoreSynced = dc.rsController.HasSynced
	dc.podStoreSynced = dc.podController.HasSynced
	return dc
}

// Run begins watching and syncing.
func (dc *DeploymentController) Run(workers int, stopCh <-chan struct{}) {
	defer util.HandleCrash()
	go dc.dController.Run(stopCh)
	go dc.rsController.Run(stopCh)
	go dc.podController.Run(stopCh)
	for i := 0; i < workers; i++ {
		go util.Until(dc.worker, time.Second, stopCh)
	}
	<-stopCh
	glog.Infof("Shutting down deployment controller")
	dc.queue.ShutDown()
}

// addReplicaSet enqueues the deployment that manages an ReplicaSet when the ReplicaSet is created.
func (dc *DeploymentController) addReplicaSet(obj interface{}) {
	rs := obj.(*extensions.ReplicaSet)
	glog.V(4).Infof("ReplicaSet %s added.", rs.Name)
	if d := dc.getDeploymentForReplicaSet(rs); rs != nil {
		dc.enqueueDeployment(d)
	}
}

// getDeploymentForReplicaSet returns the deployment managing the given ReplicaSet.
// TODO: Surface that we are ignoring multiple deployments for a given ReplicaSet.
func (dc *DeploymentController) getDeploymentForReplicaSet(rs *extensions.ReplicaSet) *extensions.Deployment {
	deployments, err := dc.dStore.GetDeploymentsForReplicaSet(rs)
	if err != nil || len(deployments) == 0 {
		glog.V(4).Infof("Error: %v. No deployment found for ReplicaSet %v, deployment controller will avoid syncing.", err, rs.Name)
		return nil
	}
	// Because all ReplicaSet's belonging to a deployment should have a unique label key,
	// there should never be more than one deployment returned by the above method.
	// If that happens we should probably dynamically repair the situation by ultimately
	// trying to clean up one of the controllers, for now we just return one of the two,
	// likely randomly.
	return &deployments[0]
}

// updateReplicaSet figures out what deployment(s) manage a ReplicaSet when the ReplicaSet
// is updated and wake them up. If the anything of the ReplicaSets have changed, we need to
// awaken both the old and new deployments. old and cur must be *extensions.ReplicaSet
// types.
func (dc *DeploymentController) updateReplicaSet(old, cur interface{}) {
	if api.Semantic.DeepEqual(old, cur) {
		// A periodic relist will send update events for all known controllers.
		return
	}
	// TODO: Write a unittest for this case
	curRS := cur.(*extensions.ReplicaSet)
	glog.V(4).Infof("ReplicaSet %s updated.", curRS.Name)
	if d := dc.getDeploymentForReplicaSet(curRS); d != nil {
		dc.enqueueDeployment(d)
	}
	// A number of things could affect the old deployment: labels changing,
	// pod template changing, etc.
	oldRS := old.(*extensions.ReplicaSet)
	if !api.Semantic.DeepEqual(oldRS, curRS) {
		if oldD := dc.getDeploymentForReplicaSet(oldRS); oldD != nil {
			dc.enqueueDeployment(oldD)
		}
	}
}

// deleteReplicaSet enqueues the deployment that manages a ReplicaSet when
// the ReplicaSet is deleted. obj could be an *extensions.ReplicaSet, or
// a DeletionFinalStateUnknown marker item.
func (dc *DeploymentController) deleteReplicaSet(obj interface{}) {
	rs, ok := obj.(*extensions.ReplicaSet)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the ReplicaSet
	// changed labels the new deployment will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %+v, could take up to %v before a deployment recreates/updates replicasets", obj, FullDeploymentResyncPeriod)
			return
		}
		rs, ok = tombstone.Obj.(*extensions.ReplicaSet)
		if !ok {
			glog.Errorf("Tombstone contained object that is not a ReplicaSet %+v, could take up to %v before a deployment recreates/updates replicasets", obj, FullDeploymentResyncPeriod)
			return
		}
	}
	glog.V(4).Infof("ReplicaSet %s deleted.", rs.Name)
	if d := dc.getDeploymentForReplicaSet(rs); d != nil {
		dc.enqueueDeployment(d)
	}
}

// getDeploymentForPod returns the deployment managing the ReplicaSet that manages the given Pod.
// TODO: Surface that we are ignoring multiple deployments for a given Pod.
func (dc *DeploymentController) getDeploymentForPod(pod *api.Pod) *extensions.Deployment {
	rss, err := dc.rsStore.GetPodReplicaSets(pod)
	if err != nil {
		glog.V(4).Infof("Error: %v. No ReplicaSets found for pod %v, deployment controller will avoid syncing.", err, pod.Name)
		return nil
	}
	for _, rs := range rss {
		deployments, err := dc.dStore.GetDeploymentsForReplicaSet(&rs)
		if err == nil && len(deployments) > 0 {
			return &deployments[0]
		}
	}
	glog.V(4).Infof("No deployments found for pod %v, deployment controller will avoid syncing.", pod.Name)
	return nil
}

// updatePod figures out what deployment(s) manage the ReplicaSet that manages the Pod when the Pod
// is updated and wake them up. If anything of the Pods have changed, we need to awaken both
// the old and new deployments. old and cur must be *api.Pod types.
func (dc *DeploymentController) updatePod(old, cur interface{}) {
	if api.Semantic.DeepEqual(old, cur) {
		return
	}
	curPod := cur.(*api.Pod)
	glog.V(4).Infof("Pod %s updated.", curPod.Name)
	if d := dc.getDeploymentForPod(curPod); d != nil {
		dc.enqueueDeployment(d)
	}
	oldPod := old.(*api.Pod)
	if !api.Semantic.DeepEqual(oldPod, curPod) {
		if oldD := dc.getDeploymentForPod(oldPod); oldD != nil {
			dc.enqueueDeployment(oldD)
		}
	}
}

// When a pod is deleted, update expectations of the controller that manages the pod.
// obj could be an *api.Pod, or a DeletionFinalStateUnknown marker item.
func (dc *DeploymentController) deletePod(obj interface{}) {
	pod, ok := obj.(*api.Pod)
	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the pod
	// changed labels the new ReplicaSet will not be woken up till the periodic
	// resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %+v, could take up to %v before a ReplicaSet recreates a replica", obj, controller.ExpectationsTimeout)
			return
		}
		pod, ok = tombstone.Obj.(*api.Pod)
		if !ok {
			glog.Errorf("Tombstone contained object that is not a pod %+v, could take up to %v before ReplicaSet recreates a replica", obj, controller.ExpectationsTimeout)
			return
		}
	}
	glog.V(4).Infof("Pod %s deleted.", pod.Name)
	if d := dc.getDeploymentForPod(pod); d != nil {
		dKey, err := controller.KeyFunc(d)
		if err != nil {
			glog.Errorf("Couldn't get key for deployment controller %#v: %v", d, err)
			return
		}
		dc.expectations.DeletionObserved(dKey)
	}
}

// obj could be an *api.Deployment, or a DeletionFinalStateUnknown marker item.
func (dc *DeploymentController) enqueueDeployment(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}

	// TODO: Handle overlapping deployments better. Either disallow them at admission time or
	// deterministically avoid syncing deployments that fight over ReplicaSet's. Currently, we
	// only ensure that the same deployment is synced for a given ReplicaSet. When we
	// periodically relist all deployments there will still be some ReplicaSet instability. One
	//  way to handle this is by querying the store for all deployments that this deployment
	// overlaps, as well as all deployments that overlap this deployments, and sorting them.
	dc.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (dc *DeploymentController) worker() {
	for {
		func() {
			key, quit := dc.queue.Get()
			if quit {
				return
			}
			defer dc.queue.Done(key)
			err := dc.syncHandler(key.(string))
			if err != nil {
				glog.Errorf("Error syncing deployment: %v", err)
			}
		}()
	}
}

// syncDeployment will sync the deployment with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (dc *DeploymentController) syncDeployment(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing deployment %q (%v)", key, time.Now().Sub(startTime))
	}()

	obj, exists, err := dc.dStore.Store.GetByKey(key)
	if err != nil {
		glog.Infof("Unable to retrieve deployment %v from store: %v", key, err)
		dc.queue.Add(key)
		return err
	}
	if !exists {
		glog.Infof("Deployment has been deleted %v", key)
		dc.expectations.DeleteExpectations(key)
		return nil
	}
	d := *obj.(*extensions.Deployment)
	switch d.Spec.Strategy.Type {
	case extensions.RecreateDeploymentStrategyType:
		return dc.syncRecreateDeployment(d)
	case extensions.RollingUpdateDeploymentStrategyType:
		return dc.syncRollingUpdateDeployment(d)
	}
	return fmt.Errorf("unexpected deployment strategy type: %s", d.Spec.Strategy.Type)
}

func (dc *DeploymentController) syncRecreateDeployment(deployment extensions.Deployment) error {
	// TODO: implement me.
	return nil
}

func (dc *DeploymentController) syncRollingUpdateDeployment(deployment extensions.Deployment) error {
	newRS, err := dc.getNewReplicaSet(deployment)
	if err != nil {
		return err
	}

	oldRSs, err := dc.getOldReplicaSets(deployment)
	if err != nil {
		return err
	}

	allRSs := append(oldRSs, newRS)

	// Scale up, if we can.
	scaledUp, err := dc.reconcileNewReplicaSet(allRSs, newRS, deployment)
	if err != nil {
		return err
	}
	if scaledUp {
		// Update DeploymentStatus
		return dc.updateDeploymentStatus(allRSs, newRS, deployment)
	}

	// Scale down, if we can.
	scaledDown, err := dc.reconcileOldReplicaSets(allRSs, oldRSs, newRS, deployment, true)
	if err != nil {
		return err
	}
	if scaledDown {
		// Update DeploymentStatus
		return dc.updateDeploymentStatus(allRSs, newRS, deployment)
	}

	// Sync deployment status
	totalReplicas := deploymentutil.GetReplicaCountForReplicaSets(allRSs)
	updatedReplicas := deploymentutil.GetReplicaCountForReplicaSets([]*extensions.ReplicaSet{newRS})
	if deployment.Status.Replicas != totalReplicas || deployment.Status.UpdatedReplicas != updatedReplicas {
		return dc.updateDeploymentStatus(allRSs, newRS, deployment)
	}

	// TODO: raise an event, neither scaled up nor down.
	return nil
}

func (dc *DeploymentController) getOldReplicaSets(deployment extensions.Deployment) ([]*extensions.ReplicaSet, error) {
	return deploymentutil.GetOldReplicaSetsFromLists(deployment, dc.client,
		func(namespace string, options api.ListOptions) (*api.PodList, error) {
			selector, err := extensions.LabelSelectorAsSelector(deployment.Spec.Selector)
			if err != nil {
				return nil, fmt.Errorf("failed to convert LabelSelector to Selector: %v", err)
			}
			podList, err := dc.podStore.Pods(namespace).List(selector)
			return &podList, err
		},
		func(namespace string, options api.ListOptions) ([]extensions.ReplicaSet, error) {
			return dc.rsStore.List()
		})
}

// Returns a ReplicaSet that matches the intent of the given deployment.
// It creates a new ReplicaSet if required.
func (dc *DeploymentController) getNewReplicaSet(deployment extensions.Deployment) (*extensions.ReplicaSet, error) {
	existingNewRS, err := deploymentutil.GetNewReplicaSetFromList(deployment, dc.client,
		func(namespace string, options api.ListOptions) ([]extensions.ReplicaSet, error) {
			return dc.rsStore.List()
		})
	if err != nil || existingNewRS != nil {
		return existingNewRS, err
	}
	// new ReplicaSet does not exist, create one.
	namespace := deployment.ObjectMeta.Namespace
	podTemplateSpecHash := deploymentutil.GetPodTemplateSpecHash(deployment.Spec.Template)
	newRSTemplate := deploymentutil.GetNewReplicaSetTemplate(deployment)
	// Add podTemplateHash label to selector.
	newRSSelector := deploymentutil.CloneSelectorAndAddLabel(deployment.Spec.Selector, deployment.Spec.UniqueLabelKey, podTemplateSpecHash)

	newRS := extensions.ReplicaSet{
		ObjectMeta: api.ObjectMeta{
			GenerateName: deployment.Name + "-",
			Namespace:    namespace,
		},
		Spec: extensions.ReplicaSetSpec{
			Replicas: 0,
			Selector: newRSSelector,
			Template: &newRSTemplate,
		},
	}
	createdRS, err := dc.expClient.ReplicaSets(namespace).Create(&newRS)
	if err != nil {
		return nil, fmt.Errorf("error creating replica set: %v", err)
	}
	return createdRS, nil
}

func (dc *DeploymentController) reconcileNewReplicaSet(allRSs []*extensions.ReplicaSet, newRS *extensions.ReplicaSet, deployment extensions.Deployment) (bool, error) {
	if newRS.Spec.Replicas == deployment.Spec.Replicas {
		// Scaling not required.
		return false, nil
	}
	if newRS.Spec.Replicas > deployment.Spec.Replicas {
		// Scale down.
		_, err := dc.scaleReplicaSetAndRecordEvent(newRS, deployment.Spec.Replicas, deployment)
		return true, err
	}
	// Check if we can scale up.
	maxSurge, isPercent, err := util.GetIntOrPercentValue(&deployment.Spec.Strategy.RollingUpdate.MaxSurge)
	if err != nil {
		return false, fmt.Errorf("invalid value for MaxSurge: %v", err)
	}
	if isPercent {
		maxSurge = util.GetValueFromPercent(maxSurge, deployment.Spec.Replicas)
	}
	// Find the total number of pods
	currentPodCount := deploymentutil.GetReplicaCountForReplicaSets(allRSs)
	maxTotalPods := deployment.Spec.Replicas + maxSurge
	if currentPodCount >= maxTotalPods {
		// Cannot scale up.
		return false, nil
	}
	// Scale up.
	scaleUpCount := maxTotalPods - currentPodCount
	// Do not exceed the number of desired replicas.
	scaleUpCount = int(math.Min(float64(scaleUpCount), float64(deployment.Spec.Replicas-newRS.Spec.Replicas)))
	newReplicasCount := newRS.Spec.Replicas + scaleUpCount
	_, err = dc.scaleReplicaSetAndRecordEvent(newRS, newReplicasCount, deployment)
	return true, err
}

// Set expectationsCheck to false to bypass expectations check when testing
func (dc *DeploymentController) reconcileOldReplicaSets(allRSs []*extensions.ReplicaSet, oldRSs []*extensions.ReplicaSet, newRS *extensions.ReplicaSet, deployment extensions.Deployment, expectationsCheck bool) (bool, error) {
	oldPodsCount := deploymentutil.GetReplicaCountForReplicaSets(oldRSs)
	if oldPodsCount == 0 {
		// Cant scale down further
		return false, nil
	}
	maxUnavailable, isPercent, err := util.GetIntOrPercentValue(&deployment.Spec.Strategy.RollingUpdate.MaxUnavailable)
	if err != nil {
		return false, fmt.Errorf("invalid value for MaxUnavailable: %v", err)
	}
	if isPercent {
		maxUnavailable = util.GetValueFromPercent(maxUnavailable, deployment.Spec.Replicas)
	}
	// Check if we can scale down.
	minAvailable := deployment.Spec.Replicas - maxUnavailable
	minReadySeconds := deployment.Spec.Strategy.RollingUpdate.MinReadySeconds
	// Check the expectations of deployment before counting available pods
	dKey, err := controller.KeyFunc(&deployment)
	if err != nil {
		return false, fmt.Errorf("Couldn't get key for deployment %#v: %v", deployment, err)
	}
	if expectationsCheck && !dc.expectations.SatisfiedExpectations(dKey) {
		fmt.Printf("Expectations not met yet before reconciling old ReplicaSets\n")
		return false, nil
	}
	// Find the number of ready pods.
	readyPodCount, err := deploymentutil.GetAvailablePodsForReplicaSets(dc.client, allRSs, minReadySeconds)
	if err != nil {
		return false, fmt.Errorf("could not find available pods: %v", err)
	}

	if readyPodCount <= minAvailable {
		// Cannot scale down.
		return false, nil
	}
	totalScaleDownCount := readyPodCount - minAvailable
	for _, targetRS := range oldRSs {
		if totalScaleDownCount == 0 {
			// No further scaling required.
			break
		}
		if targetRS.Spec.Replicas == 0 {
			// cannot scale down this ReplicaSet.
			continue
		}
		// Scale down.
		scaleDownCount := int(math.Min(float64(targetRS.Spec.Replicas), float64(totalScaleDownCount)))
		newReplicasCount := targetRS.Spec.Replicas - scaleDownCount
		_, err = dc.scaleReplicaSetAndRecordEvent(targetRS, newReplicasCount, deployment)
		if err != nil {
			return false, err
		}
		totalScaleDownCount -= scaleDownCount
		dKey, err := controller.KeyFunc(&deployment)
		if err != nil {
			return false, fmt.Errorf("Couldn't get key for deployment %#v: %v", deployment, err)
		}
		if expectationsCheck {
			dc.expectations.ExpectDeletions(dKey, scaleDownCount)
		}
	}
	return true, err
}

func (dc *DeploymentController) updateDeploymentStatus(allRSs []*extensions.ReplicaSet, newRS *extensions.ReplicaSet, deployment extensions.Deployment) error {
	totalReplicas := deploymentutil.GetReplicaCountForReplicaSets(allRSs)
	updatedReplicas := deploymentutil.GetReplicaCountForReplicaSets([]*extensions.ReplicaSet{newRS})
	newDeployment := deployment
	// TODO: Reconcile this with API definition. API definition talks about ready pods, while this just computes created pods.
	newDeployment.Status = extensions.DeploymentStatus{
		Replicas:        totalReplicas,
		UpdatedReplicas: updatedReplicas,
	}
	_, err := dc.expClient.Deployments(deployment.ObjectMeta.Namespace).UpdateStatus(&newDeployment)
	return err
}

func (dc *DeploymentController) scaleReplicaSetAndRecordEvent(rs *extensions.ReplicaSet, newScale int, deployment extensions.Deployment) (*extensions.ReplicaSet, error) {
	scalingOperation := "down"
	if rs.Spec.Replicas < newScale {
		scalingOperation = "up"
	}
	newRS, err := dc.scaleReplicaSet(rs, newScale)
	if err == nil {
		dc.eventRecorder.Eventf(&deployment, api.EventTypeNormal, "ScalingReplicaSet", "Scaled %s ReplicaSet %s to %d", scalingOperation, rs.Name, newScale)
	}
	return newRS, err
}

func (dc *DeploymentController) scaleReplicaSet(rs *extensions.ReplicaSet, newScale int) (*extensions.ReplicaSet, error) {
	// TODO: Using client for now, update to use store when it is ready.
	rs.Spec.Replicas = newScale
	return dc.expClient.ReplicaSets(rs.ObjectMeta.Namespace).Update(rs)
}

func (dc *DeploymentController) updateDeployment(deployment *extensions.Deployment) (*extensions.Deployment, error) {
	// TODO: Using client for now, update to use store when it is ready.
	return dc.expClient.Deployments(deployment.ObjectMeta.Namespace).Update(deployment)
}
