/*
Copyright 2022 The Kubernetes Authors.

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

package removepodsviolatingnodeconditions

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"sigs.k8s.io/descheduler/pkg/descheduler/evictions"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	frameworktypes "sigs.k8s.io/descheduler/pkg/framework/types"
)

const PluginName = "RemovePodsViolatingNodeConditions"

// RemovePodsViolatingNodeConditions evicts pods on the node which violate NoSchedule Conditions on nodes
type RemovePodsViolatingNodeConditions struct {
	handle    frameworktypes.Handle
	args      *RemovePodsViolatingNodeConditionsArgs
	podFilter podutil.FilterFunc
}

var _ frameworktypes.DeschedulePlugin = &RemovePodsViolatingNodeConditions{}

// New builds plugin from its arguments while passing a handle
func New(args runtime.Object, handle frameworktypes.Handle) (frameworktypes.Plugin, error) {
	nodeConditionsArgs, ok := args.(*RemovePodsViolatingNodeConditionsArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type RemovePodsViolatingNodeConditionsArgs, got %T", args)
	}

	var includedNamespaces, excludedNamespaces sets.Set[string]
	if nodeConditionsArgs.Namespaces != nil {
		includedNamespaces = sets.New(nodeConditionsArgs.Namespaces.Include...)
		excludedNamespaces = sets.New(nodeConditionsArgs.Namespaces.Exclude...)
	}

	// We can combine Filter and PreEvictionFilter since for this strategy it does not matter where we run PreEvictionFilter
	podFilter, err := podutil.NewOptions().
		WithFilter(podutil.WrapFilterFuncs(handle.Evictor().Filter, handle.Evictor().PreEvictionFilter)).
		WithNamespaces(includedNamespaces).
		WithoutNamespaces(excludedNamespaces).
		WithLabelSelector(nodeConditionsArgs.LabelSelector).
		BuildFilterFunc()
	if err != nil {
		return nil, fmt.Errorf("error initializing pod filter function: %v", err)
	}

	return &RemovePodsViolatingNodeConditions{
		handle:    handle,
		podFilter: podFilter,
		args:      nodeConditionsArgs,
	}, nil
}

// Name retrieves the plugin name
func (d *RemovePodsViolatingNodeConditions) Name() string {
	return PluginName
}

// Deschedule extension point implementation for the plugin
func (d *RemovePodsViolatingNodeConditions) Deschedule(ctx context.Context, nodes []*v1.Node) *frameworktypes.Status {
	for _, node := range nodes {
		klog.V(1).InfoS("Processing node", "node", klog.KObj(node))
		pods, err := podutil.ListAllPodsOnANode(node.Name, d.handle.GetPodsAssignedToNodeFunc(), d.podFilter)
		if err != nil {
			// no pods evicted as error encountered retrieving evictable Pods
			return &frameworktypes.Status{
				Err: fmt.Errorf("error listing pods on a node: %v", err),
			}
		}

		// only evict if the node has conditions (except KubeletReady) that are True
		drainCondition := false
		conditions := node.Status.Conditions
		totalConditions := len(conditions)
		if totalConditions > 0 {
			klog.V(5).InfoS("The node has the following node conditions", "node", klog.KObj(node))
			for a := 0; a < totalConditions; a++ {
				klog.V(5).InfoS("Condition has property", "Message", conditions[a].Message)
				klog.V(5).InfoS("Condition has property", "Reason", conditions[a].Reason)
				klog.V(5).InfoS("Condition has property", "Status", conditions[a].Status)
				klog.V(5).InfoS("Condition has property", "Type", conditions[a].Type)
				if conditions[a].Reason != "KubeletReady" && conditions[a].Status == "True" {
					drainCondition = true
					klog.V(3).InfoS("The node has a drain node condition!", "node", klog.KObj(node))
				}
			}

			// we can drain this node
			if drainCondition {
				totalPods := len(pods)
				// evict all the pods on this node
				for i := 0; i < totalPods; i++ {
					klog.V(3).InfoS("Evicting pod on node due to node conditions", "pod", klog.KObj(pods[i]), "node", klog.KObj(node))
					d.handle.Evictor().Evict(ctx, pods[i], evictions.EvictOptions{})
					if d.handle.Evictor().NodeLimitExceeded(node) {
						break
					}
				}
			}
		} else {
			klog.V(3).InfoS("The node has NO node conditions", "node", klog.KObj(node))
		}
	}

	return nil
}
