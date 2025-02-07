package consolidation

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/karpenter/pkg/apis/awsnodetemplate/v1alpha1"
	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	awsv1alpha1 "github.com/aws/karpenter/pkg/cloudprovider/aws/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/test"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/aws/karpenter/test/pkg/environment"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var env *environment.Environment

func TestConsolidation(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		var err error
		env, err = environment.NewEnvironment(t)
		Expect(err).ToNot(HaveOccurred())
	})
	RunSpecs(t, "Consolidation")
}

var _ = BeforeEach(func() {
	env.BeforeEach()
})

var _ = AfterEach(func() {
	env.AfterEach()
})

var _ = Describe("Consolidation", func() {
	It("should consolidate nodes (delete)", func() {
		provider := test.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{AWS: awsv1alpha1.AWS{
			SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": env.ClusterName},
			SubnetSelector:        map[string]string{"karpenter.sh/discovery": env.ClusterName},
		}})
		provisioner := test.Provisioner(test.ProvisionerOptions{
			Requirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1alpha5.LabelCapacityType,
					Operator: v1.NodeSelectorOpIn,
					// we don't replace spot nodes, so this forces us to only delete nodes
					Values: []string{"spot"},
				},
				{
					Key:      awsv1alpha1.LabelInstanceSize,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"medium", "large", "xlarge"},
				},
				{
					Key:      awsv1alpha1.LabelInstanceFamily,
					Operator: v1.NodeSelectorOpNotIn,
					// remove some cheap burstable and the odd c1 instance types so we have
					// more control over what gets provisioned
					Values: []string{"t2", "t3", "c1", "t3a", "t4g"},
				},
			},
			// prevent emptiness from deleting the nodes
			TTLSecondsAfterEmpty: aws.Int64(99999),
			ProviderRef:          &v1alpha5.ProviderRef{Name: provider.Name},
		})

		var numPods int32 = 100
		dep := test.Deployment(test.DeploymentOptions{
			Replicas: numPods,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "large-app"},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1")},
				},
			},
		})

		selector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
		env.ExpectCreatedNodeCount("==", 0)
		env.ExpectCreated(provisioner, provider, dep)

		env.EventuallyExpectHealthyPodCount(selector, int(numPods))

		// reduce the number of pods by 60%
		dep.Spec.Replicas = aws.Int32(40)
		env.ExpectUpdate(dep)
		env.EventuallyExpectAvgUtilization(v1.ResourceCPU, "<", 0.5)

		provisioner.Spec.TTLSecondsAfterEmpty = nil
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{
			Enabled: aws.Bool(true),
		}
		env.ExpectUpdate(provisioner)

		// With consolidation enabled, we now must delete nodes
		env.EventuallyExpectAvgUtilization(v1.ResourceCPU, ">", 0.6)

		env.ExpectDeleted(dep)
	})
	It("should consolidate nodes (replace)", func() {
		provider := test.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{AWS: awsv1alpha1.AWS{
			SecurityGroupSelector: map[string]string{"karpenter.sh/discovery": env.ClusterName},
			SubnetSelector:        map[string]string{"karpenter.sh/discovery": env.ClusterName},
		}})
		provisioner := test.Provisioner(test.ProvisionerOptions{
			Requirements: []v1.NodeSelectorRequirement{
				{
					Key:      v1alpha5.LabelCapacityType,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"on-demand"},
				},
				{
					Key:      awsv1alpha1.LabelInstanceSize,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"large", "2xlarge"},
				},
			},
			ProviderRef: &v1alpha5.ProviderRef{Name: provider.Name},
		})

		var numPods int32 = 3
		largeDep := test.Deployment(test.DeploymentOptions{
			Replicas: numPods,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "large-app"},
				},
				TopologySpreadConstraints: []v1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       v1.LabelHostname,
						WhenUnsatisfiable: v1.DoNotSchedule,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "large-app",
							},
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("4")},
				},
			},
		})
		smallDep := test.Deployment(test.DeploymentOptions{
			Replicas: numPods,
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "small-app"},
				},
				TopologySpreadConstraints: []v1.TopologySpreadConstraint{
					{
						MaxSkew:           1,
						TopologyKey:       v1.LabelHostname,
						WhenUnsatisfiable: v1.DoNotSchedule,
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app": "small-app",
							},
						},
					},
				},
				ResourceRequirements: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1.5")},
				},
			},
		})

		selector := labels.SelectorFromSet(largeDep.Spec.Selector.MatchLabels)
		env.ExpectCreatedNodeCount("==", 0)
		env.ExpectCreated(provisioner, provider, largeDep, smallDep)

		env.EventuallyExpectHealthyPodCount(selector, int(numPods))

		// 3 nodes due to the anti-affinity rules
		env.ExpectCreatedNodeCount("==", 3)

		// scaling down the large deployment leaves only small pods on each node
		largeDep.Spec.Replicas = aws.Int32(0)
		env.ExpectUpdate(largeDep)
		env.EventuallyExpectAvgUtilization(v1.ResourceCPU, "<", 0.5)

		provisioner.Spec.TTLSecondsAfterEmpty = nil
		provisioner.Spec.Consolidation = &v1alpha5.Consolidation{
			Enabled: aws.Bool(true),
		}
		env.ExpectUpdate(provisioner)

		// With consolidation enabled, we now must replace each node in turn to consolidate due to the anti-affinity
		// rules on the smaller deployment.  The 2xl nodes should go to a large
		env.EventuallyExpectAvgUtilization(v1.ResourceCPU, ">", 0.8)

		var nodes v1.NodeList
		Expect(env.Client.List(env.Context, &nodes)).To(Succeed())
		numLargeNodes := 0
		numOtherNodes := 0
		for _, n := range nodes.Items {
			// only count the nodes created by the provisoiner
			if n.Labels[v1alpha5.ProvisionerNameLabelKey] != provisioner.Name {
				continue
			}
			if strings.HasSuffix(n.Labels[v1.LabelInstanceTypeStable], ".large") {
				numLargeNodes++
			} else {
				numOtherNodes++
			}
		}

		// all of the 2xlarge nodes should have been replaced with large instance types
		Expect(numLargeNodes).To(Equal(3))
		// and we should have no other nodes
		Expect(numOtherNodes).To(Equal(0))

		env.ExpectDeleted(largeDep, smallDep)
	})
})
