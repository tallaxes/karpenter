/*
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

package consolidation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/cloudprovider/fake"
	"github.com/aws/karpenter/pkg/controllers/consolidation"
	"github.com/aws/karpenter/pkg/controllers/provisioning"
	"github.com/aws/karpenter/pkg/controllers/state"
	"github.com/aws/karpenter/pkg/test"
	. "github.com/aws/karpenter/pkg/test/expectations"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var ctx context.Context
var env *test.Environment
var cluster *state.Cluster
var controller *consolidation.Controller
var provisioner *provisioning.Provisioner
var cloudProvider *fake.CloudProvider
var clientSet *kubernetes.Clientset
var recorder *test.EventRecorder
var nodeStateController *state.NodeController
var fakeClock *clock.FakeClock
var cfg *test.Config
var mostExpensiveInstance cloudprovider.InstanceType
var leastExpensiveInstance cloudprovider.InstanceType

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Consolidation")
}

var _ = BeforeSuite(func() {
	env = test.NewEnvironment(ctx, func(e *test.Environment) {
		cloudProvider = &fake.CloudProvider{}
		cfg = test.NewConfig()
		fakeClock = clock.NewFakeClock(time.Now())
		cluster = state.NewCluster(fakeClock, cfg, env.Client, cloudProvider)
		nodeStateController = state.NewNodeController(env.Client, cluster)
		clientSet = kubernetes.NewForConfigOrDie(e.Config)
		recorder = test.NewEventRecorder()
		provisioner = provisioning.NewProvisioner(ctx, cfg, env.Client, clientSet.CoreV1(), recorder, cloudProvider, cluster)
	})
	Expect(env.Start()).To(Succeed(), "Failed to start environment")
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	cloudProvider.CreateCalls = nil
	cloudProvider.InstanceTypes = fake.InstanceTypesAssorted()
	mostExpensiveInstance = lo.MaxBy(cloudProvider.InstanceTypes, func(lhs, rhs cloudprovider.InstanceType) bool {
		return lhs.Price() > rhs.Price()
	})
	// los MaxBy & MinBy functions are identical.  https://github.com/samber/lo/issues/129
	leastExpensiveInstance = lo.MaxBy(cloudProvider.InstanceTypes, func(lhs, rhs cloudprovider.InstanceType) bool {
		return lhs.Price() < rhs.Price()
	})

	recorder.Reset()
	fakeClock.SetTime(time.Now())
	controller = consolidation.NewController(env.Ctx, fakeClock, env.Client, provisioner, cloudProvider, recorder, cluster, nil)
})
var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
	var nodes []client.ObjectKey
	cluster.ForEachNode(func(n *state.Node) bool {
		nodes = append(nodes, client.ObjectKeyFromObject(n.Node))
		return true
	})

	// inform cluster state of node deletion
	for _, nodeKey := range nodes {
		ExpectReconcileSucceeded(ctx, nodeStateController, nodeKey)
	}
})

var _ = Describe("Pod Eviction Cost", func() {
	const standardPodCost = 1.0
	It("should have a standard disruptionCost for a pod with no priority or disruptionCost specified", func() {
		cost := consolidation.GetPodEvictionCost(ctx, &v1.Pod{})
		Expect(cost).To(BeNumerically("==", standardPodCost))
	})
	It("should have a higher disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := consolidation.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a positive deletion disruptionCost", func() {
		cost := consolidation.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "-100",
			}},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
	It("should have higher costs for higher deletion costs", func() {
		cost1 := consolidation.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "101",
			}},
		})
		cost2 := consolidation.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "100",
			}},
		})
		cost3 := consolidation.GetPodEvictionCost(ctx, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1.PodDeletionCost: "99",
			}},
		})
		Expect(cost1).To(BeNumerically(">", cost2))
		Expect(cost2).To(BeNumerically(">", cost3))
	})
	It("should have a higher disruptionCost for a pod with a higher priority", func() {
		cost := consolidation.GetPodEvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: aws.Int32(1)},
		})
		Expect(cost).To(BeNumerically(">", standardPodCost))
	})
	It("should have a lower disruptionCost for a pod with a lower priority", func() {
		cost := consolidation.GetPodEvictionCost(ctx, &v1.Pod{
			Spec: v1.PodSpec{Priority: aws.Int32(-1)},
		})
		Expect(cost).To(BeNumerically("<", standardPodCost))
	})
})

var _ = Describe("Replace Nodes", func() {
	It("can replace node", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pod := test.Pod(test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("32")}})

		ExpectApplied(ctx, env.Client, rs, pod, node, prov)
		ExpectMakeNodesReady(ctx, env.Client, node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node))
		ExpectManualBinding(ctx, env.Client, pod, node)
		ExpectScheduled(ctx, env.Client, pod)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())

		// consolidation won't delete the old node until the new node is ready
		wg := ExpectMakeNewNodesReady(ctx, env.Client, 1, node)
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)
		wg.Wait()

		// should create a new node as there is a cheaper one that can hold the pod
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))
		// and delete the old one
		ExpectNotFound(ctx, env.Client, node)
	})
	It("can replace nodes, considers PDB", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		pdb := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         labels,
			MaxUnavailable: fromInt(0),
			Status: &policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 1,
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
			},
		})

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], node1, prov, pdb)
		ExpectApplied(ctx, env.Client, node1)
		// all pods on node1
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		ExpectManualBinding(ctx, env.Client, pods[2], node1)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need a new node
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and can't delete the node due to the PDB
		ExpectNodeExists(ctx, env.Client, node1.Name)
	})
	It("can replace nodes, considers do-not-consolidate annotation", func() {
		labels := map[string]string{
			"app": "test",
		}

		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{TTLSecondsUntilExpired: aws.Int64(30), Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		regularNode := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		annotatedNode := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.DoNotConsolidateNodeAnnotationKey: "true",
				},
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], prov)
		ExpectApplied(ctx, env.Client, regularNode, annotatedNode)
		ExpectApplied(ctx, env.Client, regularNode, annotatedNode)
		ExpectMakeNodesReady(ctx, env.Client, regularNode, annotatedNode)
		ExpectManualBinding(ctx, env.Client, pods[0], regularNode)
		ExpectManualBinding(ctx, env.Client, pods[1], regularNode)
		ExpectManualBinding(ctx, env.Client, pods[2], annotatedNode)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(regularNode))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(annotatedNode))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// we should delete the non-annotated node
		ExpectNotFound(ctx, env.Client, regularNode)
	})
})

var _ = Describe("Delete Node", func() {
	It("can delete nodes", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		node2 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], node1, node2, prov)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2)

		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		ExpectManualBinding(ctx, env.Client, pods[2], node2)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need a new node, but we should evict everything off one of node2 which only has a single pod
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and delete the old one
		ExpectNotFound(ctx, env.Client, node2)
	})
	It("can delete nodes, considers PDB", func() {
		var nl v1.NodeList
		Expect(env.Client.List(ctx, &nl)).To(Succeed())
		Expect(nl.Items).To(HaveLen(0))
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		// only pod[2] is covered by the PDB
		pods[2].Labels = labels
		pdb := test.PodDisruptionBudget(test.PDBOptions{
			Labels:         labels,
			MaxUnavailable: fromInt(0),
			Status: &policyv1.PodDisruptionBudgetStatus{
				ObservedGeneration: 1,
				DisruptionsAllowed: 0,
				CurrentHealthy:     1,
				DesiredHealthy:     1,
				ExpectedPods:       1,
			},
		})

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		node2 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], node1, node2, prov, pdb)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2)
		// two pods on node 1
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		// one on node 2, but it has a PDB with zero disruptions allowed
		ExpectManualBinding(ctx, env.Client, pods[2], node2)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need a new node
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// but we expect to delete the nmode with more pods (node1) as the pod on node2 has a PDB preventing
		// eviction
		ExpectNotFound(ctx, env.Client, node1)
	})
	It("can delete nodes, considers do-not-evict", func() {
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		// only pod[2] has a do not evict annotation
		pods[2].Annotations = map[string]string{
			v1alpha5.DoNotEvictPodAnnotationKey: "true",
		}

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		node2 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], node1, node2, prov)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2)
		// two pods on node 1
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		// one on node 2, but it has a do-not-evict annotation
		ExpectManualBinding(ctx, env.Client, pods[2], node2)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need a new node
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// but we expect to delete the node with more pods (node1) as the pod on node2 has a do-not-evict annotation
		ExpectNotFound(ctx, env.Client, node1)
	})
	It("can delete nodes, doesn't evict standalone pods", func() {
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		// pod[2] is a stand-alone (non ReplicaSet) pod
		pods[2].OwnerReferences = nil

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		node2 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], node1, node2, prov)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2)
		// two pods on node 1
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node1)
		// one on node 2, but it's a standalone pod
		ExpectManualBinding(ctx, env.Client, pods[2], node2)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need a new node
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// but we expect to delete the node with more pods (node1) as the pod on node2 doesn't have a controller to
		// recreate it
		ExpectNotFound(ctx, env.Client, node1)
	})
})

var _ = Describe("Node Lifetime Consideration", func() {
	It("should consider node lifetime remaining when calculating disruption cost", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(2, test.PodOptions{
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{TTLSecondsUntilExpired: aws.Int64(30), Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		node2 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], prov)
		ExpectApplied(ctx, env.Client, node1) // ensure node1 is the oldest node
		time.Sleep(1 * time.Second)           // this sleep is unfortunate, but necessary.  The creation time is from etcd and we can't mock it, so we
		// need to sleep to force the second node to be created a bit after the first node.
		ExpectApplied(ctx, env.Client, node2)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2)
		ExpectManualBinding(ctx, env.Client, pods[0], node1)
		ExpectManualBinding(ctx, env.Client, pods[1], node2)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// the nodes are identical (same size, price, disruption cost, etc.) except for age.  We should prefer to
		// delete the older one
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		ExpectNotFound(ctx, env.Client, node1)
	})
})

var _ = Describe("Topology Consideration", func() {
	It("can replace node maintaining zonal topology spread", func() {
		labels := map[string]string{
			"app": "test-zonal-spread",
		}

		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		tsc := v1.TopologySpreadConstraint{
			MaxSkew:           1,
			TopologyKey:       v1.LabelTopologyZone,
			WhenUnsatisfiable: v1.DoNotSchedule,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: labels},
		}
		pods := test.Pods(4, test.PodOptions{
			ResourceRequirements:      v1.ResourceRequirements{Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}},
			TopologySpreadConstraints: []v1.TopologySpreadConstraint{tsc},
			ObjectMeta: metav1.ObjectMeta{
				Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		zone1Node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-1",
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}})

		zone2Node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-2",
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}})

		zone3Node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-3",
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], zone1Node, zone2Node, zone3Node, prov)
		ExpectMakeNodesReady(ctx, env.Client, zone1Node, zone2Node, zone3Node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(zone1Node))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(zone2Node))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(zone3Node))
		ExpectManualBinding(ctx, env.Client, pods[0], zone1Node)
		ExpectManualBinding(ctx, env.Client, pods[1], zone2Node)
		ExpectManualBinding(ctx, env.Client, pods[2], zone3Node)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))

		// consolidation won't delete the old node until the new node is ready
		wg := ExpectMakeNewNodesReady(ctx, env.Client, 1, zone1Node, zone2Node, zone3Node)
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)
		wg.Wait()

		// should create a new node as there is a cheaper one that can hold the pod
		Expect(cloudProvider.CreateCalls).To(HaveLen(1))

		// we need to emulate the replicaset controller and bind a new pod to the newly created node
		ExpectApplied(ctx, env.Client, pods[3])
		var nodes v1.NodeList
		Expect(env.Client.List(ctx, &nodes)).To(Succeed())
		Expect(nodes.Items).To(HaveLen(3))
		for i, n := range nodes.Items {
			// bind the pod to the new node we don't recognize as it is the one that consolidation created
			if n.Name != zone1Node.Name && n.Name != zone2Node.Name && n.Name != zone3Node.Name {
				ExpectManualBinding(ctx, env.Client, pods[3], &nodes.Items[i])
			}
		}
		// we should maintain our skew, the new node must be in the same zone as the old node it replaced
		ExpectSkew(ctx, env.Client, "default", &tsc).To(ConsistOf(1, 1, 1))
	})
	It("won't delete node if it would violate pod anti-affinity", func() {
		labels := map[string]string{
			"app": "test",
		}
		// create our RS so we can link a pod to it
		rs := test.ReplicaSet()
		ExpectApplied(ctx, env.Client, rs)
		Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(rs), rs)).To(Succeed())

		pods := test.Pods(3, test.PodOptions{
			ResourceRequirements: v1.ResourceRequirements{Requests: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}},
			PodAntiRequirements: []v1.PodAffinityTerm{
				{
					LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
					TopologyKey:   v1.LabelHostname,
				},
			},
			ObjectMeta: metav1.ObjectMeta{Labels: labels,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "apps/v1",
						Kind:               "ReplicaSet",
						Name:               rs.Name,
						UID:                rs.UID,
						Controller:         aws.Bool(true),
						BlockOwnerDeletion: aws.Bool(true),
					},
				}}})

		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})
		zone1Node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-1",
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}})

		zone2Node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-2",
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}})

		zone3Node := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelTopologyZone:             "test-zone-3",
					v1.LabelInstanceTypeStable:       leastExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{v1.ResourceCPU: resource.MustParse("1")}})

		ExpectApplied(ctx, env.Client, rs, pods[0], pods[1], pods[2], zone1Node, zone2Node, zone3Node, prov)
		ExpectMakeNodesReady(ctx, env.Client, zone1Node, zone2Node, zone3Node)
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(zone1Node))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(zone2Node))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(zone3Node))
		ExpectManualBinding(ctx, env.Client, pods[0], zone1Node)
		ExpectManualBinding(ctx, env.Client, pods[1], zone2Node)
		ExpectManualBinding(ctx, env.Client, pods[2], zone3Node)
		ExpectScheduled(ctx, env.Client, pods[0])
		ExpectScheduled(ctx, env.Client, pods[1])
		ExpectScheduled(ctx, env.Client, pods[2])

		wg := ExpectMakeNewNodesReady(ctx, env.Client, 1, zone1Node, zone2Node, zone3Node)
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)
		wg.Wait()

		// our nodes are already the cheapest available, so we can't replace them.  If we delete, it would
		// violate the anti-affinity rule so we can't do anything.
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		ExpectNodeExists(ctx, env.Client, zone1Node.Name)
		ExpectNodeExists(ctx, env.Client, zone2Node.Name)
		ExpectNodeExists(ctx, env.Client, zone3Node.Name)

	})
})

var _ = Describe("Empty Nodes", func() {
	It("can delete empty nodes", func() {
		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})

		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
					v1alpha5.LabelNodeInitialized:    "true",
				},
			},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, node1, prov)

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need any new nodes
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and should delete the empty one
		ExpectNotFound(ctx, env.Client, node1)
	})
	It("can delete multiple empty nodes", func() {
		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})

		node1 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})
		node2 := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				}},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, node1, node2, prov)
		ExpectMakeNodesReady(ctx, env.Client, node1, node2)

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node1))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(node2))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need any new nodes
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and should delete both empty ones
		ExpectNotFound(ctx, env.Client, node1)
		ExpectNotFound(ctx, env.Client, node2)
	})
})

var _ = Describe("Special Cases", func() {
	It("doesn't consolidate in the presence of uninitialized nodes", func() {
		prov := test.Provisioner(test.ProvisionerOptions{Consolidation: &v1alpha5.Consolidation{Enabled: aws.Bool(true)}})

		uninitializedNode := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
				},
			},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		emptyNode := test.Node(test.NodeOptions{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					v1alpha5.ProvisionerNameLabelKey: prov.Name,
					v1.LabelInstanceTypeStable:       mostExpensiveInstance.Name(),
					v1alpha5.LabelNodeInitialized:    "true",
				},
			},
			Allocatable: map[v1.ResourceName]resource.Quantity{
				v1.ResourceCPU:  resource.MustParse("32"),
				v1.ResourcePods: resource.MustParse("100"),
			}})

		ExpectApplied(ctx, env.Client, emptyNode, uninitializedNode, prov)

		// inform cluster state about the nodes
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(emptyNode))
		ExpectReconcileSucceeded(ctx, nodeStateController, client.ObjectKeyFromObject(uninitializedNode))
		fakeClock.Step(10 * time.Minute)
		controller.ProcessCluster(ctx)

		// we don't need any new nodes
		Expect(cloudProvider.CreateCalls).To(HaveLen(0))
		// and shouldn't delete the empty one due to the un-initialized node
		ExpectNodeExists(ctx, env.Client, emptyNode.Name)
	})
})

func fromInt(i int) *intstr.IntOrString {
	v := intstr.FromInt(i)
	return &v
}

func ExpectMakeNewNodesReady(ctx context.Context, client client.Client, numNewNodes int, existingNodes ...*v1.Node) *sync.WaitGroup {
	var wg sync.WaitGroup

	existingNodeNames := sets.NewString()
	for _, existing := range existingNodes {
		existingNodeNames.Insert(existing.Name)
	}
	go func() {
		defer GinkgoRecover()
		wg.Add(1)
		defer wg.Add(-1)
		start := time.Now()
		for {
			select {
			case <-time.After(50 * time.Millisecond):
				// give up after 10 seconds
				if time.Since(start) > 10*time.Second {
					return
				}
				var nodeList v1.NodeList
				client.List(ctx, &nodeList)
				nodesMadeReady := 0
				for i := range nodeList.Items {
					n := &nodeList.Items[i]
					if existingNodeNames.Has(n.Name) {
						continue
					}
					ExpectMakeNodesReady(ctx, env.Client, n)
					nodesMadeReady++
					// did we make all of the nodes ready that we expected?
					if nodesMadeReady == numNewNodes {
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return &wg
}

func ExpectMakeNodesReady(ctx context.Context, c client.Client, nodes ...*v1.Node) {
	for _, node := range nodes {
		var n v1.Node
		Expect(c.Get(ctx, client.ObjectKeyFromObject(node), &n)).To(Succeed())
		n.Status.Phase = v1.NodeRunning
		n.Status.Conditions = []v1.NodeCondition{
			{
				Type:               v1.NodeReady,
				Status:             v1.ConditionTrue,
				LastHeartbeatTime:  metav1.Now(),
				LastTransitionTime: metav1.Now(),
				Reason:             "KubeletReady",
			},
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[v1alpha5.LabelNodeInitialized] = "true"
		n.Spec.Taints = nil
		ExpectApplied(ctx, c, &n)
	}
}
