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
	"hash/adler32"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util"
)

// GetOldReplicaSets returns the old ReplicaSets targeted by the given Deployment; get PodList and ReplicaSetList from client interface.
func GetOldReplicaSets(deployment extensions.Deployment, c client.Interface) ([]*extensions.ReplicaSet, error) {
	return GetOldReplicaSetsFromLists(deployment, c,
		func(namespace string, options api.ListOptions) (*api.PodList, error) {
			return c.Pods(namespace).List(options)
		},
		func(namespace string, options api.ListOptions) ([]extensions.ReplicaSet, error) {
			rsList, err := c.Extensions().ReplicaSets(namespace).List(options)
			return rsList.Items, err
		})
}

// GetOldReplicaSetsFromLists returns the old ReplicaSets targeted by the given Deployment; get PodList and ReplicaSetList with input functions.
func GetOldReplicaSetsFromLists(deployment extensions.Deployment, c client.Interface, getPodList func(string, api.ListOptions) (*api.PodList, error), getRSList func(string, api.ListOptions) ([]extensions.ReplicaSet, error)) ([]*extensions.ReplicaSet, error) {
	namespace := deployment.ObjectMeta.Namespace
	selector, err := extensions.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("failed to convert LabelSelector to Selector: %v", err)
	}

	// 1. Find all pods whose labels match deployment.Spec.Selector
	options := api.ListOptions{LabelSelector: selector}
	podList, err := getPodList(namespace, options)
	if err != nil {
		return nil, fmt.Errorf("error listing pods: %v", err)
	}
	// 2. Find the corresponding ReplicaSets for pods in podList.
	// TODO: Right now we list all ReplicaSets and then filter. We should add an API for this.
	oldRSs := map[string]extensions.ReplicaSet{}
	rsList, err := getRSList(namespace, api.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing ReplicaSets: %v", err)
	}
	newRSTemplate := GetNewReplicaSetTemplate(deployment)
	for _, pod := range podList.Items {
		podLabelsSelector := labels.Set(pod.ObjectMeta.Labels)
		for _, rs := range rsList {
			rsLabelsSelector, err := extensions.LabelSelectorAsSelector(rs.Spec.Selector)
			if err != nil {
				return nil, fmt.Errorf("failed to convert LabelSelector to Selector: %v", err)
			}
			if rsLabelsSelector.Matches(podLabelsSelector) {
				// Filter out ReplicaSet that has the same pod template spec as the deployment - that is the new ReplicaSet.
				if api.Semantic.DeepEqual(rs.Spec.Template, &newRSTemplate) {
					continue
				}
				oldRSs[rs.ObjectMeta.Name] = rs
			}
		}
	}
	requiredRSs := []*extensions.ReplicaSet{}
	for key := range oldRSs {
		value := oldRSs[key]
		requiredRSs = append(requiredRSs, &value)
	}
	return requiredRSs, nil
}

// GetNewReplicaSet returns a ReplicaSet that matches the intent of the given deployment; get ReplicaSetList
// from client interface. Returns nil if the new ReplicaSet doesnt exist yet.
func GetNewReplicaSet(deployment extensions.Deployment, c client.Interface) (*extensions.ReplicaSet, error) {
	return GetNewReplicaSetFromList(deployment, c,
		func(namespace string, options api.ListOptions) ([]extensions.ReplicaSet, error) {
			rsList, err := c.Extensions().ReplicaSets(namespace).List(options)
			return rsList.Items, err
		})
}

// GetNewReplicaSetFromList returns a ReplicaSet that matches the intent of the given deployment; get
// ReplicaSetList with the input function. Returns nil if the new ReplicaSet doesnt exist yet.
func GetNewReplicaSetFromList(deployment extensions.Deployment, c client.Interface, getRSList func(string, api.ListOptions) ([]extensions.ReplicaSet, error)) (*extensions.ReplicaSet, error) {
	namespace := deployment.ObjectMeta.Namespace
	rsList, err := getRSList(namespace, api.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error listing ReplicaSets: %v", err)
	}
	newRSTemplate := GetNewReplicaSetTemplate(deployment)

	for i := range rsList {
		if api.Semantic.DeepEqual(rsList[i].Spec.Template, &newRSTemplate) {
			// This is the new ReplicaSet.
			return &rsList[i], nil
		}
	}
	// new ReplicaSet does not exist.
	return nil, nil
}

// Returns the desired PodTemplateSpec for the new ReplicaSet corresponding to the given ReplicaSet.
func GetNewReplicaSetTemplate(deployment extensions.Deployment) api.PodTemplateSpec {
	// newRS will have the same template as in deployment spec, plus a unique label in some cases.
	newRSTemplate := api.PodTemplateSpec{
		ObjectMeta: deployment.Spec.Template.ObjectMeta,
		Spec:       deployment.Spec.Template.Spec,
	}
	newRSTemplate.ObjectMeta.Labels = CloneAndAddLabel(
		deployment.Spec.Template.ObjectMeta.Labels,
		deployment.Spec.UniqueLabelKey,
		GetPodTemplateSpecHash(newRSTemplate))
	return newRSTemplate
}

// Clones the given map and returns a new map with the given key and value added.
// Returns the given map, if labelKey is empty.
func CloneAndAddLabel(labels map[string]string, labelKey string, labelValue uint32) map[string]string {
	if labelKey == "" {
		// Dont need to add a label.
		return labels
	}
	// Clone.
	newLabels := map[string]string{}
	for key, value := range labels {
		newLabels[key] = value
	}
	newLabels[labelKey] = fmt.Sprintf("%d", labelValue)
	return newLabels
}

// Clones the given selector and returns a new selector with the given key and value added.
// Returns the given selector, if labelKey is empty.
func CloneSelectorAndAddLabel(selector *extensions.LabelSelector, labelKey string, labelValue uint32) *extensions.LabelSelector {
	if labelKey == "" {
		// Dont need to add a label.
		return selector
	}

	// Clone.
	newSelector := new(extensions.LabelSelector)

	// TODO(madhusudancs): Check if you can use deepCopy_extensions_LabelSelector here.
	newSelector.MatchLabels = make(map[string]string)
	if selector.MatchLabels != nil {
		for key, val := range selector.MatchLabels {
			newSelector.MatchLabels[key] = val
		}
	}
	newSelector.MatchLabels[labelKey] = fmt.Sprintf("%d", labelValue)

	if selector.MatchExpressions != nil {
		newMExps := make([]extensions.LabelSelectorRequirement, len(selector.MatchExpressions))
		for i, me := range selector.MatchExpressions {
			newMExps[i].Key = me.Key
			newMExps[i].Operator = me.Operator
			if me.Values != nil {
				newMExps[i].Values = make([]string, len(me.Values))
				for j, val := range me.Values {
					newMExps[i].Values[j] = val
				}
			} else {
				newMExps[i].Values = nil
			}
		}
		newSelector.MatchExpressions = newMExps
	} else {
		newSelector.MatchExpressions = nil
	}

	return newSelector
}

func GetPodTemplateSpecHash(template api.PodTemplateSpec) uint32 {
	podTemplateSpecHasher := adler32.New()
	util.DeepHashObject(podTemplateSpecHasher, template)
	return podTemplateSpecHasher.Sum32()
}

// Returns the sum of Replicas of the given ReplicaSets.
func GetReplicaCountForReplicaSets(replicaSets []*extensions.ReplicaSet) int {
	totalReplicaCount := 0
	for _, rs := range replicaSets {
		totalReplicaCount += rs.Spec.Replicas
	}
	return totalReplicaCount
}

// Returns the number of available pods corresponding to the given ReplicaSets.
func GetAvailablePodsForReplicaSets(c client.Interface, rss []*extensions.ReplicaSet, minReadySeconds int) (int, error) {
	allPods, err := getPodsForReplicaSets(c, rss)
	if err != nil {
		return 0, err
	}
	return getReadyPodsCount(allPods, minReadySeconds), nil
}

func getReadyPodsCount(pods []api.Pod, minReadySeconds int) int {
	readyPodCount := 0
	for _, pod := range pods {
		if api.IsPodReady(&pod) {
			// Check if we've passed minReadySeconds since LastTransitionTime
			// If so, this pod is ready
			for _, c := range pod.Status.Conditions {
				// we only care about pod ready conditions
				if c.Type == api.PodReady {
					// 2 cases that this ready condition is valid (passed minReadySeconds, i.e. the pod is ready):
					// 1. minReadySeconds <= 0
					// 2. LastTransitionTime (is set) + minReadySeconds (>0) < current time
					minReadySecondsDuration := time.Duration(minReadySeconds) * time.Second
					if minReadySeconds <= 0 || !c.LastTransitionTime.IsZero() && c.LastTransitionTime.Add(minReadySecondsDuration).Before(time.Now()) {
						readyPodCount++
						break
					}
				}
			}
		}
	}
	return readyPodCount
}

func getPodsForReplicaSets(c client.Interface, replicaSets []*extensions.ReplicaSet) ([]api.Pod, error) {
	allPods := []api.Pod{}
	for _, rs := range replicaSets {
		selector, err := extensions.LabelSelectorAsSelector(rs.Spec.Selector)
		if err != nil {
			return nil, fmt.Errorf("failed to convert LabelSelector to Selector: %v", err)
		}
		options := api.ListOptions{LabelSelector: selector}
		podList, err := c.Pods(rs.ObjectMeta.Namespace).List(options)
		if err != nil {
			return allPods, fmt.Errorf("error listing pods: %v", err)
		}
		allPods = append(allPods, podList.Items...)
	}
	return allPods, nil
}
