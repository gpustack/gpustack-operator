package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// TestInstanceIsolationReconciler_EnsuresNamespacePolicy pins the isolation contract every
// namespace gets once the administrator opts in: Instance Pods may be reached from their own
// namespace and from outside the cluster (the NodePort path), never from another namespace's
// Pods — and may dial out under the same rule, so a shell inside one Instance cannot scan or
// reach a neighbor tenant's.
//
// The policy is one object per namespace, and the controller covers a namespace from the moment
// the namespace exists — before its first Instance lands. It selects the stable resource-type
// label every Instance Pod carries, so it outlives any single Instance and never churns on a
// stop/start. The pod CIDRs in the ipBlock exceptions come from the administrator's Setting — the
// one source that can see what a CNI's IPAM really allocates — and the render carries exactly
// what was configured, sorted and deduplicated.
func TestInstanceIsolationReconciler_EnsuresNamespacePolicy(t *testing.T) {
	ctx := context.Background()
	cli := buildInstanceClient(
		&core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "default"}},
		delegatedSettingsSecret("10.42.1.0/24,10.42.0.0/24"),
	)
	r := &InstanceIsolationReconciler{Client: cli}

	_, err := r.Reconcile(ctx, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: "default"}})
	require.NoError(t, err)

	np := new(networking.NetworkPolicy)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Namespace: "default", Name: instanceIsolationPolicyName,
	}, np),
		"reconciling a namespace ensures the namespace-isolation policy in it")

	assert.Equal(t, map[string]string{
		systemmeta.ResourceTypeLabel: "instances",
	}, np.Spec.PodSelector.MatchLabels, "the policy selects Instance Pods by their stable label")
	assert.ElementsMatch(t,
		[]networking.PolicyType{networking.PolicyTypeIngress, networking.PolicyTypeEgress},
		np.Spec.PolicyTypes, "isolation holds in both directions")

	// The Setting's value lands sorted and deduplicated, so the render is byte-stable across
	// reconciles whatever order the entries were typed in.
	wantExcept := []string{"10.42.0.0/24", "10.42.1.0/24"}

	require.Len(t, np.Spec.Ingress, 1, "one ingress rule carries both allowed peer kinds")
	ingressPeers := np.Spec.Ingress[0].From
	require.Len(t, ingressPeers, 2)
	assert.NotNil(t, ingressPeers[0].PodSelector, "same-namespace pods are allowed in")
	assert.Empty(t, ingressPeers[0].PodSelector.MatchLabels)
	assert.Empty(t, ingressPeers[0].PodSelector.MatchExpressions)
	require.NotNil(t, ingressPeers[1].IPBlock, "and so is everything outside the pod network")
	assert.Equal(t, "0.0.0.0/0", ingressPeers[1].IPBlock.CIDR)
	assert.Equal(t, wantExcept, ingressPeers[1].IPBlock.Except,
		"pod addresses of every namespace are the one thing not let in by the external rule")

	require.Len(t, np.Spec.Egress, 3, "same-namespace, external, and DNS each get an egress rule")
	assert.NotNil(t, np.Spec.Egress[0].To[0].PodSelector, "same-namespace pods stay reachable")
	require.NotNil(t, np.Spec.Egress[1].To[0].IPBlock)
	assert.Equal(t, "0.0.0.0/0", np.Spec.Egress[1].To[0].IPBlock.CIDR)
	assert.Equal(t, wantExcept, np.Spec.Egress[1].To[0].IPBlock.Except,
		"dialing another namespace's pod is blocked at the source too")
	assert.Empty(t, np.Spec.Egress[2].To, "DNS is port-scoped to survive ClusterIP DNAT ordering")
	assert.ElementsMatch(t, []networking.NetworkPolicyPort{
		{Protocol: ptr.To(core.ProtocolUDP), Port: ptr.To(intstr.FromInt32(53))},
		{Protocol: ptr.To(core.ProtocolTCP), Port: ptr.To(intstr.FromInt32(53))},
	}, np.Spec.Egress[2].Ports)

	// The policy names no owner: it is shared per namespace and must outlive the Instance that
	// first rendered it.
	assert.Empty(t, np.OwnerReferences, "a namespace-shared policy is never collected with an Instance")

	// A second pass renders the identical object: the align is a no-op, which the untouched
	// resource version pins.
	firstRV := np.ResourceVersion
	_, err = r.Reconcile(ctx, ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: "default"}})
	require.NoError(t, err)
	again := new(networking.NetworkPolicy)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{
		Namespace: "default", Name: instanceIsolationPolicyName,
	}, again))
	assert.Equal(t, firstRV, again.ResourceVersion,
		"an unchanged desired state must not rewrite the policy")
}

// TestInstanceIsolationReconciler_NamespacePoliciesFollowTheSetting pins the opt-in
// contract around the isolation policy. A blank Setting renders nothing and, going the other way,
// collects only the object this operator noted as its own — the fixed name is a public contract,
// so a same-named policy nobody here rendered belongs to whoever wrote it, and a Setting edit
// never deletes their networking. A configured Setting pulls a hand-touched policy back to the
// rendered shape.
func TestInstanceIsolationReconciler_NamespacePoliciesFollowTheSetting(t *testing.T) {
	ownedPolicy := func() *networking.NetworkPolicy {
		return &networking.NetworkPolicy{
			ObjectMeta: meta.ObjectMeta{
				Namespace: "default",
				Name:      instanceIsolationPolicyName,
				Labels: map[string]string{
					systemmeta.ResourceTypeLabel: "instances",
				},
			},
			Spec: convertIsolationPolicy([]string{"10.42.0.0/16"}).Spec,
		}
	}

	cases := []struct {
		name        string
		setting     string
		seed        *networking.NetworkPolicy
		wantPresent bool
		want        func(t *testing.T, np *networking.NetworkPolicy)
	}{
		{
			name:    "blank setting, nothing seeded — nothing appears",
			setting: "",
		},
		{
			name:        "configured setting renders the policy with the configured CIDRs",
			setting:     "10.42.0.0/16",
			wantPresent: true,
			want: func(t *testing.T, np *networking.NetworkPolicy) {
				assert.Equal(t, convertIsolationPolicy([]string{"10.42.0.0/16"}).Spec, np.Spec,
					"the render carries exactly what the Setting configured")
			},
		},
		{
			name:    "blank setting collects the policy this operator rendered",
			setting: "",
			seed:    ownedPolicy(),
		},
		{
			name:    "blank setting leaves a same-named stranger exactly as found",
			setting: "",
			seed: &networking.NetworkPolicy{
				ObjectMeta: meta.ObjectMeta{
					Namespace: "default",
					Name:      instanceIsolationPolicyName,
				},
				Spec: networking.NetworkPolicySpec{
					PodSelector: meta.LabelSelector{
						MatchLabels: map[string]string{"hand-written": "true"},
					},
					PolicyTypes: []networking.PolicyType{networking.PolicyTypeIngress},
				},
			},
			wantPresent: true,
			want: func(t *testing.T, np *networking.NetworkPolicy) {
				assert.Equal(t, map[string]string{"hand-written": "true"},
					np.Spec.PodSelector.MatchLabels,
					"a policy this operator never rendered is never rewritten either")
				assert.Equal(t, []networking.PolicyType{networking.PolicyTypeIngress},
					np.Spec.PolicyTypes, "the stranger's own shape survives untouched")
			},
		},
		{
			name:    "a hand-widened policy converges back to the configured render",
			setting: "10.42.0.0/16",
			seed: func() *networking.NetworkPolicy {
				np := ownedPolicy()
				np.Spec.Ingress[0].From[1].IPBlock.Except = nil
				return np
			}(),
			wantPresent: true,
			want: func(t *testing.T, np *networking.NetworkPolicy) {
				assert.Equal(t, []string{"10.42.0.0/16"},
					np.Spec.Ingress[0].From[1].IPBlock.Except,
					"the configured Setting pulls a drifted policy back")
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			objs := []ctrlcli.Object{
				&core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "default"}},
				delegatedSettingsSecret(c.setting),
			}
			if c.seed != nil {
				objs = append(objs, c.seed)
			}
			cli := buildInstanceClient(objs...)
			r := &InstanceIsolationReconciler{Client: cli}

			_, err := r.Reconcile(context.Background(), ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: "default"}})
			require.NoError(t, err)

			np := new(networking.NetworkPolicy)
			err = cli.Get(context.Background(), ctrlcli.ObjectKey{
				Namespace: "default", Name: instanceIsolationPolicyName,
			}, np)
			if !c.wantPresent {
				require.True(t, kerrors.IsNotFound(err),
					"an opted-out namespace must not keep a namespace-isolation policy")
				return
			}
			require.NoError(t, err)
			c.want(t, np)
		})
	}
}

// delegatedSettingsSecret builds the delegated settings Secret carrying one Setting value: the
// namespace-isolation controller reads its Setting through this Secret, so the tests seed it
// exactly as the cluster stores it — a data key named after the Setting, absent when the Setting
// was never written.
func delegatedSettingsSecret(podCIDRs string) *core.Secret {
	data := map[string][]byte{}
	if podCIDRs != "" {
		data[instanceIsolationPodCIDRsSettingName] = []byte(podCIDRs)
	}
	return &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: setting.DelegatedSecretNamespace,
			Name:      setting.DelegatedSecretName,
		},
		Data: data,
	}
}
