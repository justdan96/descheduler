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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"

	"sigs.k8s.io/descheduler/pkg/descheduler/evictions"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	frameworkfake "sigs.k8s.io/descheduler/pkg/framework/fake"
	"sigs.k8s.io/descheduler/pkg/framework/plugins/defaultevictor"
	frameworktypes "sigs.k8s.io/descheduler/pkg/framework/types"
	"sigs.k8s.io/descheduler/pkg/utils"
	"sigs.k8s.io/descheduler/test"
)

func createNodeCondition() v1.NodeCondition {
	return v1.NodeCondition{
		Type:               v1.NodeConditionType("ReadonlyFilesystem"),
		Status:             v1.ConditionTrue,
		LastHeartbeatTime:  meta.Time{Time: time.Now()},
		LastTransitionTime: meta.Time{Time: time.Now()},
		Reason:             "FilesystemIsNotReadOnly",
		Message:            "Filesystem is not read-only",
	}
}

func addConditionsToNode(node *v1.Node) *v1.Node {
	conditions := []v1.NodeCondition{}
	conditions = append(conditions, createNodeCondition())
	node.Status.Conditions = conditions
	return node
}

func TestDeletePodsViolatingNodeConditions(t *testing.T) {
	node1 := test.BuildTestNode("n1", 2000, 3000, 10, nil)
	node1 = addConditionsToNode(node1)
	node2 := test.BuildTestNode("n2", 2000, 3000, 10, nil)
	node2 = addConditionsToNode(node2)

	node3 := test.BuildTestNode("n3", 2000, 3000, 10, func(node *v1.Node) {
		node.ObjectMeta.Labels = map[string]string{
			"datacenter": "east",
		}
	})
	node4 := test.BuildTestNode("n4", 2000, 3000, 10, func(node *v1.Node) {
		node.Spec = v1.NodeSpec{
			Unschedulable: true,
		}
	})

	node5 := test.BuildTestNode("n5", 2000, 3000, 10, nil)
	node5.Status.Conditions = []v1.NodeCondition{
		createNodeCondition(),
	}

	node6 := test.BuildTestNode("n6", 1, 1, 1, nil)
	node6.Status.Conditions = []v1.NodeCondition{
		createNodeCondition(),
	}

	p1 := test.BuildTestPod("p1", 100, 0, node1.Name, nil)
	p2 := test.BuildTestPod("p2", 100, 0, node1.Name, nil)
	p3 := test.BuildTestPod("p3", 100, 0, node1.Name, nil)
	p4 := test.BuildTestPod("p4", 100, 0, node1.Name, nil)
	p5 := test.BuildTestPod("p5", 100, 0, node1.Name, nil)
	p6 := test.BuildTestPod("p6", 100, 0, node1.Name, nil)

	p1.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p2.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p3.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p4.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p5.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p6.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p7 := test.BuildTestPod("p7", 100, 0, node2.Name, nil)
	p8 := test.BuildTestPod("p8", 100, 0, node2.Name, nil)
	p9 := test.BuildTestPod("p9", 100, 0, node2.Name, nil)
	p10 := test.BuildTestPod("p10", 100, 0, node2.Name, nil)
	p11 := test.BuildTestPod("p11", 100, 0, node2.Name, nil)
	p11.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p12 := test.BuildTestPod("p11", 100, 0, node2.Name, nil)
	p12.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()

	// The following 4 pods won't get evicted.
	// A Critical Pod.
	p7.Namespace = "kube-system"
	priority := utils.SystemCriticalPriority
	p7.Spec.Priority = &priority
	p7.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()

	// A daemonset.
	p8.ObjectMeta.OwnerReferences = test.GetDaemonSetOwnerRefList()
	// A pod with local storage.
	p9.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()
	p9.Spec.Volumes = []v1.Volume{
		{
			Name: "sample",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{Path: "somePath"},
				EmptyDir: &v1.EmptyDirVolumeSource{
					SizeLimit: resource.NewQuantity(int64(10), resource.BinarySI),
				},
			},
		},
	}
	// A Mirror Pod.
	p10.Annotations = test.GetMirrorPodAnnotation()

	p12.Spec.NodeSelector = map[string]string{
		"datacenter": "west",
	}

	p13 := test.BuildTestPod("p13", 100, 0, node5.Name, nil)
	p13.ObjectMeta.OwnerReferences = test.GetNormalPodOwnerRefList()

	var uint1 uint = 1

	tests := []struct {
		description                    string
		nodes                          []*v1.Node
		pods                           []*v1.Pod
		evictLocalStoragePods          bool
		evictSystemCriticalPods        bool
		maxPodsToEvictPerNode          *uint
		maxNoOfPodsToEvictPerNamespace *uint
		expectedEvictedPodCount        uint
		nodeFit                        bool
		includePreferNoSchedule        bool
		excludedConditions             []string
	}{
		{
			description:             "Pods not tolerating node condition should be evicted",
			pods:                    []*v1.Pod{p1, p2, p3},
			nodes:                   []*v1.Node{node1},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 1, // p2 gets evicted
		},
		{
			description:             "Pods with tolerations but not tolerating node condition should be evicted",
			pods:                    []*v1.Pod{p1, p3, p4},
			nodes:                   []*v1.Node{node1},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 1, // p4 gets evicted
		},
		{
			description:             "Only <maxPodsToEvictPerNode> number of Pods not tolerating node condition should be evicted",
			pods:                    []*v1.Pod{p1, p5, p6},
			nodes:                   []*v1.Node{node1},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			maxPodsToEvictPerNode:   &uint1,
			expectedEvictedPodCount: 1, // p5 or p6 gets evicted
		},
		{
			description:                    "Only <maxNoOfPodsToEvictPerNamespace> number of Pods not tolerating node condition should be evicted",
			pods:                           []*v1.Pod{p1, p5, p6},
			nodes:                          []*v1.Node{node1},
			evictLocalStoragePods:          false,
			evictSystemCriticalPods:        false,
			maxNoOfPodsToEvictPerNamespace: &uint1,
			expectedEvictedPodCount:        1, // p5 or p6 gets evicted
		},
		{
			description:             "Critical pods not tolerating node condition should not be evicted",
			pods:                    []*v1.Pod{p7, p8, p9, p10},
			nodes:                   []*v1.Node{node2},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 0, // nothing is evicted
		},
		{
			description:             "Critical pods except storage pods not tolerating node condition should not be evicted",
			pods:                    []*v1.Pod{p7, p8, p9, p10},
			nodes:                   []*v1.Node{node2},
			evictLocalStoragePods:   true,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 1, // p9 gets evicted
		},
		{
			description:             "Critical and non critical pods, only non critical pods not tolerating node condition should be evicted",
			pods:                    []*v1.Pod{p7, p8, p10, p11},
			nodes:                   []*v1.Node{node2},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 1, // p11 gets evicted
		},
		{
			description:             "Critical and non critical pods, pods not tolerating node condition should be evicted even if they are critical",
			pods:                    []*v1.Pod{p2, p7, p9, p10},
			nodes:                   []*v1.Node{node1, node2},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: true,
			expectedEvictedPodCount: 2, // p2 and p7 are evicted
		},
		{
			description:             "Pod p2 doesn't tolerate condition on it's node, but also doesn't tolerate conditions on other nodes",
			pods:                    []*v1.Pod{p1, p2, p3},
			nodes:                   []*v1.Node{node1, node2},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 0, // p2 gets evicted
			nodeFit:                 true,
		},
		{
			description:             "Pod p12 doesn't tolerate condition on it's node, but other nodes don't match it's selector",
			pods:                    []*v1.Pod{p1, p3, p12},
			nodes:                   []*v1.Node{node1, node3},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 0, // p2 gets evicted
			nodeFit:                 true,
		},
		{
			description:             "Pod p2 doesn't tolerate condition on it's node, but other nodes are unschedulable",
			pods:                    []*v1.Pod{p1, p2, p3},
			nodes:                   []*v1.Node{node1, node4},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 0, // p2 gets evicted
			nodeFit:                 true,
		},
		{
			description:             "Pods not tolerating PreferNoSchedule node condition should not be evicted when not enabled",
			pods:                    []*v1.Pod{p13},
			nodes:                   []*v1.Node{node5},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			expectedEvictedPodCount: 0,
		},
		{
			description:             "Pods not tolerating PreferNoSchedule node condition should be evicted when enabled",
			pods:                    []*v1.Pod{p13},
			nodes:                   []*v1.Node{node5},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			includePreferNoSchedule: true,
			expectedEvictedPodCount: 1, // p13 gets evicted
		},
		{
			description:             "Pods not tolerating excluded node conditions (by key) should not be evicted",
			pods:                    []*v1.Pod{p2},
			nodes:                   []*v1.Node{node1},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			excludedConditions:      []string{"excludedCondition1", "testCondition1"},
			expectedEvictedPodCount: 0, // nothing gets evicted, as one of the specified excludedConditions matches the key of node1's condition
		},
		{
			description:             "Pods not tolerating excluded node conditions (by key and value) should not be evicted",
			pods:                    []*v1.Pod{p2},
			nodes:                   []*v1.Node{node1},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			excludedConditions:      []string{"testCondition1=test1"},
			expectedEvictedPodCount: 0, // nothing gets evicted, as both the key and value of the excluded condition match node1's condition
		},
		{
			description:             "The excluded condition matches the key of node1's condition, but does not match the value",
			pods:                    []*v1.Pod{p2},
			nodes:                   []*v1.Node{node1},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: false,
			excludedConditions:      []string{"testCondition1=test2"},
			expectedEvictedPodCount: 1, // pod gets evicted, as excluded condition value does not match node1's condition value
		},
		{
			description:             "Critical and non critical pods, pods not tolerating node condition can't be evicted because the only available node does not have enough resources.",
			pods:                    []*v1.Pod{p2, p7, p9, p10},
			nodes:                   []*v1.Node{node1, node6},
			evictLocalStoragePods:   false,
			evictSystemCriticalPods: true,
			expectedEvictedPodCount: 0, // p2 and p7 can't be evicted
			nodeFit:                 true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var objs []runtime.Object
			for _, node := range tc.nodes {
				objs = append(objs, node)
			}
			for _, pod := range tc.pods {
				objs = append(objs, pod)
			}
			fakeClient := fake.NewSimpleClientset(objs...)

			sharedInformerFactory := informers.NewSharedInformerFactory(fakeClient, 0)
			podInformer := sharedInformerFactory.Core().V1().Pods().Informer()

			getPodsAssignedToNode, err := podutil.BuildGetPodsAssignedToNodeFunc(podInformer)
			if err != nil {
				t.Errorf("Build get pods assigned to node function error: %v", err)
			}

			sharedInformerFactory.Start(ctx.Done())
			sharedInformerFactory.WaitForCacheSync(ctx.Done())

			eventRecorder := &events.FakeRecorder{}

			podEvictor := evictions.NewPodEvictor(
				fakeClient,
				policyv1.SchemeGroupVersion.String(),
				false,
				tc.maxPodsToEvictPerNode,
				tc.maxNoOfPodsToEvictPerNamespace,
				tc.nodes,
				false,
				eventRecorder,
			)

			defaultevictorArgs := &defaultevictor.DefaultEvictorArgs{
				EvictLocalStoragePods:   tc.evictLocalStoragePods,
				EvictSystemCriticalPods: tc.evictSystemCriticalPods,
				IgnorePvcPods:           false,
				EvictFailedBarePods:     false,
				NodeFit:                 tc.nodeFit,
			}

			evictorFilter, err := defaultevictor.New(
				defaultevictorArgs,
				&frameworkfake.HandleImpl{
					ClientsetImpl:                 fakeClient,
					GetPodsAssignedToNodeFuncImpl: getPodsAssignedToNode,
					SharedInformerFactoryImpl:     sharedInformerFactory,
				},
			)
			if err != nil {
				t.Fatalf("Unable to initialize the plugin: %v", err)
			}

			handle := &frameworkfake.HandleImpl{
				ClientsetImpl:                 fakeClient,
				GetPodsAssignedToNodeFuncImpl: getPodsAssignedToNode,
				PodEvictorImpl:                podEvictor,
				EvictorFilterImpl:             evictorFilter.(frameworktypes.EvictorPlugin),
				SharedInformerFactoryImpl:     sharedInformerFactory,
			}

			plugin, err := New(&RemovePodsViolatingNodeConditionsArgs{
				IncludePreferNoSchedule: tc.includePreferNoSchedule,
				ExcludedConditions:      tc.excludedConditions,
			},
				handle,
			)
			if err != nil {
				t.Fatalf("Unable to initialize the plugin: %v", err)
			}

			plugin.(frameworktypes.DeschedulePlugin).Deschedule(ctx, tc.nodes)
			actualEvictedPodCount := podEvictor.TotalEvicted()
			if actualEvictedPodCount != tc.expectedEvictedPodCount {
				t.Errorf("Test %#v failed, Unexpected no of pods evicted: pods evicted: %d, expected: %d", tc.description, actualEvictedPodCount, tc.expectedEvictedPodCount)
			}
		})
	}
}
