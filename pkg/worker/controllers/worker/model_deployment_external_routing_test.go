package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	gpustackcore "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestExternalDPRoutingSelectsQualifiedMembers(t *testing.T) {
	for _, routerName := range []string{
		workercore.ModelDeploymentRouterLLMD,
		workercore.ModelDeploymentRouterVLLM,
		workercore.ModelDeploymentRouterSGLang,
	} {
		for _, decided := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/decided=%t", routerName, decided), func(t *testing.T) {
				md := newRenderDeployment()
				md.Spec.KVCache = nil
				md.Spec.Engine.Version = "0.29.0"
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: routerName}
				md.Spec.Roles[0].ReplicaSize = 2
				md.Spec.Roles[0].ExtraArgs = []string{"--data-parallel-size", "2", "--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
				internal := *md.Spec.Roles[0].DeepCopy()
				internal.Name = "internal"
				internal.ExtraArgs = nil
				normalized := *internal.DeepCopy()
				normalized.Name = "normalized"
				normalized.ExtraArgs = []string{"--data-parallel-hybrid-lb", "--data-parallel-size=4", "--data-parallel-size-local=4"}
				md.Spec.Roles = append(md.Spec.Roles, internal, normalized)
				if decided {
					ModelDeploymentConditionEndpointEligibility.True(md, modelDeploymentReasonEndpointsQualified, "all groups qualified")
				}
				objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, false)
				require.NoError(t, err)
				discovery := routingDiscoverySelector(t, objects.Deployment.Spec.Template.Spec.Containers[0].Args, objects.ConfigMap)
				for i := range md.Spec.Roles {
					role := &md.Spec.Roles[i]
					published := labels.SelectorFromSet(objects.Contract.Roles[i].Selector)
					service := labels.SelectorFromSet(renderModelDeploymentRoleService(md, role, nil, nil, false).Spec.Selector)
					for member := range 2 {
						pod, renderErr := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
							Deployment: md, Role: role, InstanceType: newRenderInstanceType(),
						})
						require.NoError(t, renderErr)
						stampModelDeploymentPod(pod, md, role, 0, member)
						answers := i == 0 || member == 0
						if answers {
							pod.Labels[modelDeploymentLabelKeyEndpointEligible] = modelDeploymentEndpointEligibleValue
						}
						want := member == 0 || (decided && i == 0)
						for name, selector := range map[string]labels.Selector{"discovery": discovery, "published": published, "service": service} {
							assert.Equal(t, want, selector.Matches(labels.Set(pod.Labels)), "%s/%s/member-%d", name, role.Name, member)
							if decided {
								withdrawn := pod.DeepCopy()
								delete(withdrawn.Labels, modelDeploymentLabelKeyEndpointEligible)
								assert.False(t, selector.Matches(labels.Set(withdrawn.Labels)), "%s must exclude withdrawn endpoints", name)
							}
						}
					}
					headless := renderModelDeploymentReplicaService(md, role, 0)
					assert.Equal(t, core.ClusterIPNone, headless.Spec.ClusterIP)
					assert.True(t, headless.Spec.PublishNotReadyAddresses)
					assert.NotContains(t, headless.Spec.Selector, modelDeploymentLabelKeyEndpointEligible)
				}
				assert.False(t, discovery.Matches(labels.Set(objects.Deployment.Spec.Template.Labels)), "the router is not an engine endpoint")
			})
		}
	}
}

func routingDiscoverySelector(t *testing.T, args []string, config *core.ConfigMap, flags ...string) labels.Selector {
	t.Helper()
	if config != nil {
		selector, err := labels.Parse(config.Data[modelDeploymentRouterSelectorKey])
		require.NoError(t, err)
		return selector
	}
	flag := "--selector"
	if len(flags) > 0 {
		flag = flags[0]
	}
	terms := map[string]string{}
	for i, arg := range args {
		if arg == flag {
			for _, term := range args[i+1:] {
				if len(term) > 1 && term[:2] == "--" {
					break
				}
				parsed, err := labels.ConvertSelectorToLabelsMap(term)
				require.NoError(t, err)
				for key, value := range parsed {
					terms[key] = value
				}
			}
			break
		}
	}
	require.NotEmpty(t, terms)
	return labels.SelectorFromSet(terms)
}

func TestPDRoutingKeepsReplicaLeaders(t *testing.T) {
	for _, routerName := range []string{workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentRouterVLLM, workercore.ModelDeploymentRouterSGLang} {
		for _, sizes := range [][2]int{{1, 1}, {2, 1}, {1, 2}, {2, 2}} {
			t.Run(fmt.Sprintf("%s/%d-%d", routerName, sizes[0], sizes[1]), func(t *testing.T) {
				md := twoRoleDeployment()
				md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
				md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: routerName}
				if routerName == workercore.ModelDeploymentRouterSGLang {
					md.Spec.Engine.Name = workercore.ModelDeploymentEngineSGLang
				}
				if routerName != workercore.ModelDeploymentRouterLLMD {
					md.Spec.Router.ExtraArgs = []string{"--policy", "round_robin"}
				}
				for i := range md.Spec.Roles {
					md.Spec.Roles[i].ReplicaSize = int32(sizes[i])
					md.Spec.Roles[i].Replicas = 2
				}
				ModelDeploymentConditionEndpointEligibility.True(md, modelDeploymentReasonEndpointsQualified, "qualified")
				objects, err := renderModelDeploymentRouterObjects(context.Background(), md, nil, true)
				require.NoError(t, err)

				for i := range md.Spec.Roles {
					role := &md.Spec.Roles[i]
					discovery := routingDiscoverySelector(t, objects.Deployment.Spec.Template.Spec.Containers[0].Args, objects.ConfigMap, "--"+role.Name+"-selector")
					service := renderModelDeploymentRoleService(md, role, nil, nil, false)
					selectors := []labels.Selector{labels.SelectorFromSet(service.Spec.Selector), labels.SelectorFromSet(objects.Contract.Roles[i].Selector)}
					if i == 0 {
						selectors = append(selectors, labels.SelectorFromSet(renderModelDeploymentService(md, nil).Spec.Selector))
					}
					for ordinal := range 2 {
						for member := range sizes[i] {
							pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{Deployment: md, Role: role, InstanceType: newRenderInstanceType()})
							require.NoError(t, err)
							stampModelDeploymentPod(pod, md, role, ordinal, member)
							assert.Equal(t, member == 0, discovery.Matches(labels.Set(pod.Labels)))
							for _, selector := range selectors {
								assert.Equal(t, member == 0, selector.Matches(labels.Set(pod.Labels)))
							}
							other := labels.SelectorFromSet(objects.Contract.Roles[1-i].Selector)
							assert.False(t, other.Matches(labels.Set(pod.Labels)))
							peer := renderModelDeploymentReplicaService(md, role, ordinal)
							assert.True(t, peer.Spec.PublishNotReadyAddresses)
							assert.True(t, labels.SelectorFromSet(peer.Spec.Selector).Matches(labels.Set(pod.Labels)))
						}
					}
				}
			})
		}
	}
}

func TestExternalDPServiceEligibilityRetention(t *testing.T) {
	md := newRenderDeployment()
	md.Spec.Engine.Version = "0.29.0"
	md.Spec.KVCache = nil
	role := &md.Spec.Roles[0]
	role.ReplicaSize = 2
	role.ExtraArgs = []string{"--data-parallel-rank", "$(GPUSTACK_MEMBER_INDEX)"}
	cli := newModelDeploymentClient(md, newRenderInstanceType())
	reconciler := &ModelDeploymentReconciler{Client: cli, APIReader: cli, Recorder: ctrlrecord.NewFakeRecorder(16)}
	for _, status := range []meta.ConditionStatus{meta.ConditionTrue, meta.ConditionFalse, meta.ConditionUnknown} {
		md.Status.Conditions = []gpustackcore.Condition{{Type: string(ModelDeploymentConditionEndpointEligibility), Status: status}}
		require.NoError(t, reconciler.syncModelDeploymentService(context.Background(), md, false, nil))
		for _, name := range []string{md.Name, md.Name + "-" + role.Name} {
			service := new(core.Service)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: md.Namespace, Name: name}, service))
			assert.NotContains(t, service.Spec.Selector, modelDeploymentMemberIndexLabel)
			assert.Equal(t, modelDeploymentEndpointEligibleValue, service.Spec.Selector[modelDeploymentLabelKeyEndpointEligible])
		}
	}
}
