package mooncake

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// TestLeaderWorkload_TheQuotaPolicySeedTellsAbsentFromFailed RUNS the seed command rather than
// matching it, because what is under test is a shell exit status and no assertion about the string
// reaches that.
//
// The init container has one job with two legitimate outcomes — copy the pool's policy, or seed an
// empty one when no pool has written it — and exactly one illegitimate one: seeding an empty policy
// when a document DID exist. That last case is invisible by construction. The container exits 0, the
// master starts against an empty ledger, and the pool reports Ready over quotas that no longer exist.
func TestLeaderWorkload_TheQuotaPolicySeedTellsAbsentFromFailed(t *testing.T) {
	empty, err := RenderQuotaPolicy(nil)
	require.NoError(t, err)

	const policy = "tenant_quotas:\n  team-a: 1073741824\n"

	cases := []struct {
		name string
		// seed prepares the source and returns the path the command should read.
		seed      func(t *testing.T, dir string) string
		wantErr   bool
		wantFile  string
		wantWhy   string
		rootBreak bool
	}{
		{
			name:     "no ConfigMap is mounted, which is a backend with no pool bound to it yet",
			seed:     func(_ *testing.T, dir string) string { return filepath.Join(dir, "absent.yaml") },
			wantFile: string(empty),
			wantWhy:  "the optional volume mounts empty, and the master will not start on no file at all",
		},
		{
			name: "the pool's policy is there",
			seed: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, "policy.yaml")
				require.NoError(t, os.WriteFile(p, []byte(policy), 0o644))
				return p
			},
			wantFile: policy,
			wantWhy:  "the ConfigMap is the desired state and this is the master's copy of it",
		},
		{
			// The case the old `cp || fallback` could not express. Everything about it looks like
			// the first case from the outside: no source arrives at the destination.
			name: "the policy is there but cannot be read",
			seed: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, "policy.yaml")
				require.NoError(t, os.WriteFile(p, []byte(policy), 0o000))
				return p
			},
			wantErr:   true,
			wantWhy:   "a copy that failed must not be laundered into the no-pool case",
			rootBreak: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rootBreak && os.Geteuid() == 0 {
				t.Skip("running as root, which reads a mode-000 file and cannot stage this failure")
			}

			dir := t.TempDir()
			target := filepath.Join(dir, "tenant-quota-policy.yaml")

			// The command as shipped, read from the constant the init container is built from.
			cmd := exec.Command("sh", "-c", quotaPolicySeedCommand)
			cmd.Env = append(os.Environ(),
				quotaPolicySeedEnv+"="+tc.seed(t, dir),
				quotaPolicyFileEnv+"="+target,
				quotaPolicyEmptyEnv+"="+string(empty))
			out, runErr := cmd.CombinedOutput()

			if tc.wantErr {
				require.Error(t, runErr, "%s (command said: %s)", tc.wantWhy, out)

				got, readErr := os.ReadFile(target)
				if readErr == nil {
					assert.NotEqual(t, string(empty), string(got),
						"an empty policy written here is the ledger silently emptied")
				}
				return
			}

			require.NoError(t, runErr, "%s (command said: %s)", tc.wantWhy, out)
			got, readErr := os.ReadFile(target)
			require.NoError(t, readErr, "the master reads this file and will not start without it")
			assert.Equal(t, tc.wantFile, string(got), tc.wantWhy)
		})
	}
}

// testBackend is the canonical managed backend, as admission leaves it. Every case starts from this
// and mutates the one field it is about, so a case says what it changes and nothing else.
func testBackend(mutate ...func(*workercore.KVCacheBackend)) *workercore.KVCacheBackend {
	kvcb := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{
			Name: "mooncake-dram",
			UID:  "1c1b3a0e-0000-4000-8000-000000000001",
		},
		Spec: workercore.KVCacheBackendSpec{
			Type: "Mooncake",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Leader: workercore.KVCacheBackendLeader{
						Replicas:           ptr.To[int32](1),
						AllocationStrategy: "FreeRatioFirst",
					},
					Members: []workercore.KVCacheBackendMember{
						{Medium: "DRAM"},
					},
				},
			},
		},
	}
	for _, m := range mutate {
		m(kvcb)
	}
	return kvcb
}

// leaderContainer returns the one container the Deployment runs, failing the test rather than
// panicking when the render produced a shape no assertion below would make sense against.
func leaderContainer(t *testing.T, kvcb *workercore.KVCacheBackend, image string) core.Container {
	t.Helper()
	deploy := RenderLeaderDeployment(kvcb, image)
	require.Len(t, deploy.Spec.Template.Spec.Containers, 1,
		"the leader runs exactly one container")
	return deploy.Spec.Template.Spec.Containers[0]
}

// TestLeaderWorkload_Shape pins the parts of the Deployment that are not about a single
// field: where it lands, how many of it there are, and what it runs.
func TestLeaderWorkload_Shape(t *testing.T) {
	deploy := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")

	assert.Equal(t, "mooncake-dram-leader", deploy.Name)
	assert.Equal(t, kuberess.SystemNamespaceName, deploy.Namespace,
		"the leader runs in the operator's own namespace, not in one derived from a cluster-scoped object")
	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)

	container := leaderContainer(t, testBackend(), "mooncake:v0.3.13")
	assert.Equal(t, "mooncake:v0.3.13", container.Image)
	assert.Equal(t, []string{"mooncake_master"}, container.Command,
		"the entrypoint is named rather than inherited, so the argv is what kubectl shows")
	assert.Equal(t, RenderLeaderFlags(testBackend().Spec.Connection.Managed.Leader), container.Args,
		"the args are T4's renderer verbatim; a second flag source is what makes them ambiguous")
}

// TestLeaderWorkload_ImageIsWhatItIsGiven asserts the renderer does not resolve the image
// itself. Whether the value came from spec.image or from the Setting is the caller's decision, and a
// renderer that reached for a default would make the fallback untestable from here.
func TestLeaderWorkload_ImageIsWhatItIsGiven(t *testing.T) {
	kvcb := testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Image = "from-the-spec:v1"
	})

	container := leaderContainer(t, kvcb, "from-the-setting:v2")
	assert.Equal(t, "from-the-setting:v2", container.Image,
		"the renderer publishes the image it was handed, having no opinion about where it came from")
}

// TestLeaderWorkload_NeverSurgesASecondMaster is the other half of "one replica, and only one".
//
// The replica count alone does not say it. A Deployment naming no strategy gets RollingUpdate, whose
// maxSurge defaults to 25% and ROUNDS UP — to one, against one desired replica — so every image or
// flag change would start the new master before stopping the old one and run two at once. That is
// the split brain the replica count exists to prevent, and it would happen on every update.
func TestLeaderWorkload_NeverSurgesASecondMaster(t *testing.T) {
	deploy := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")

	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(1), *deploy.Spec.Replicas)
	assert.Equal(t, apps.RecreateDeploymentStrategyType, deploy.Spec.Strategy.Type,
		"an unset strategy is RollingUpdate, which surges a second master past a single replica")
	assert.Nil(t, deploy.Spec.Strategy.RollingUpdate,
		"and the rolling fields go with the type, or they describe a strategy not in use")
}

// withLeaderReplicas sets how many leader processes the object asks for.
func withLeaderReplicas(replicas int32) func(*workercore.KVCacheBackend) {
	return func(kvcb *workercore.KVCacheBackend) {
		kvcb.Spec.Connection.Managed.Leader.Replicas = ptr.To(replicas)
	}
}

// TestLeaderWorkload_UpdateStrategyInvertsWithStandbys asserts the two update shapes ARE opposites,
// not variations, and each case names the failure the other one produces.
//
// One replica has no election, so a surging second master is a split brain. Several replicas have an
// election, so the surge is safe -- and now the danger is the reverse: exactly one replica is ever
// ready, because the standbys deliberately are not, so any maxUnavailable below replicas-1 asks for
// more available replicas than this workload ever has and the rollout never moves.
func TestLeaderWorkload_UpdateStrategyInvertsWithStandbys(t *testing.T) {
	cases := []struct {
		name     string
		replicas int32
		// wantSurge and wantUnavailable are only read for the rolling case.
		wantRolling     bool
		wantSurge       int32
		wantUnavailable int32
		why             string
	}{
		{
			name:     "one replica recreates",
			replicas: 1,
			why:      "without an election a surged second master is a split brain",
		},
		{
			name:            "two replicas roll, tolerating the one standby",
			replicas:        2,
			wantRolling:     true,
			wantSurge:       1,
			wantUnavailable: 1,
			why:             "one of the two is a standby and never ready",
		},
		{
			name:            "five replicas tolerate four standbys",
			replicas:        5,
			wantRolling:     true,
			wantSurge:       1,
			wantUnavailable: 4,
			why:             "maxUnavailable tracks replicas-1 rather than a fixed number",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deploy := RenderLeaderDeployment(
				testBackend(withLeaderReplicas(c.replicas)), "mooncake:v0.3.13")

			require.NotNil(t, deploy.Spec.Replicas)
			assert.Equal(t, c.replicas, *deploy.Spec.Replicas)

			if !c.wantRolling {
				assert.Equal(t, apps.RecreateDeploymentStrategyType, deploy.Spec.Strategy.Type, c.why)
				assert.Nil(t, deploy.Spec.Strategy.RollingUpdate,
					"the rolling fields go with the type, or they describe a strategy not in use")
				assert.Nil(t, deploy.Spec.ProgressDeadlineSeconds,
					"a single replica keeps the server's default deadline, which discriminates there")
				return
			}

			assert.Equal(t, apps.RollingUpdateDeploymentStrategyType, deploy.Spec.Strategy.Type, c.why)
			require.NotNil(t, deploy.Spec.Strategy.RollingUpdate)
			require.NotNil(t, deploy.Spec.Strategy.RollingUpdate.MaxSurge)
			require.NotNil(t, deploy.Spec.Strategy.RollingUpdate.MaxUnavailable)

			assert.Equal(t, c.wantSurge,
				deploy.Spec.Strategy.RollingUpdate.MaxSurge.IntVal,
				"the lease admits one leader however many processes run, so surging is safe here")
			assert.Equal(t, c.wantUnavailable,
				deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal,
				"anything smaller stalls: only the leader is ever ready")
		})
	}
}

// TestLeaderWorkload_ProgressDeadlineIsDisabledOnlyWithStandbys pins the one field whose absence and
// presence mean opposite things.
//
// With standbys the deadline cannot discriminate -- DeploymentComplete requires availableReplicas to
// equal spec.replicas, which is permanently false, so "rolled out" and "the image will not pull"
// both present as a stale Progressing timestamp. With one replica it discriminates normally and is
// left to the server.
func TestLeaderWorkload_ProgressDeadlineIsDisabledOnlyWithStandbys(t *testing.T) {
	single := RenderLeaderDeployment(testBackend(withLeaderReplicas(1)), "mooncake:v0.3.13")
	assert.Nil(t, single.Spec.ProgressDeadlineSeconds,
		"one replica reaches availableReplicas == replicas, so the timeout still means something")

	several := RenderLeaderDeployment(testBackend(withLeaderReplicas(3)), "mooncake:v0.3.13")
	require.NotNil(t, several.Spec.ProgressDeadlineSeconds)
	assert.Equal(t, int32(math.MaxInt32), *several.Spec.ProgressDeadlineSeconds,
		"a deadline that fires on every healthy rollout is worse than none")
}

// TestLeaderWorkload_PullPolicyAndSecrets covers the two fields that decide whether the image can be
// fetched at all.
//
// Neither is inherited from the cluster-wide "image-pull-policy" / "image-pull-secrets" Settings:
// those are values of the bundled-application chart install and reach nothing a controller renders.
// So without these fields no role of this backend could ever run an image from a private registry —
// there is no service account of ours carrying credentials either.
func TestLeaderWorkload_PullPolicyAndSecrets(t *testing.T) {
	t.Run("unset resolves to the default the tag implies", func(t *testing.T) {
		deploy := RenderLeaderDeployment(testBackend(), "mooncake:v1")

		assert.Equal(t, core.PullIfNotPresent, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy,
			"resolved here rather than left empty: an empty policy is filled in by the API server, "+
				"and an aligner cannot both converge that default and correct a stale value")
		assert.Empty(t, deploy.Spec.Template.Spec.ImagePullSecrets)
	})

	t.Run("both reach the rendered pod", func(t *testing.T) {
		kvcb := testBackend(func(k *workercore.KVCacheBackend) {
			k.Spec.ImagePullPolicy = core.PullAlways
			k.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry-creds"}}
		})

		deploy := RenderLeaderDeployment(kvcb, "private.example.com/mooncake:v1")

		assert.Equal(t, core.PullAlways, deploy.Spec.Template.Spec.Containers[0].ImagePullPolicy)
		assert.Equal(t, []core.LocalObjectReference{{Name: "registry-creds"}},
			deploy.Spec.Template.Spec.ImagePullSecrets)
	})
}

// TestLeaderWorkload_Probes is the case the F6 correction exists for.
//
// /health is in the master's UNGATED route class: it answers 200 in every state, including before
// the service plane is up. An httpGet readinessProbe pointed at it is not a weak probe, it is an
// inert one — the Pod goes Ready as soon as the HTTP server binds, and the Service publishes an
// endpoint that refuses RPC. So the two probes take different paths, and this test asserts the
// difference rather than each path in isolation: a render that pointed both at /health would satisfy
// any per-probe assertion and still be the bug.
func TestLeaderWorkload_Probes(t *testing.T) {
	container := leaderContainer(t, testBackend(), "mooncake:v0.3.13")

	require.NotNil(t, container.ReadinessProbe)
	require.NotNil(t, container.ReadinessProbe.HTTPGet)
	require.NotNil(t, container.LivenessProbe)
	require.NotNil(t, container.LivenessProbe.HTTPGet)

	readiness, liveness := container.ReadinessProbe.HTTPGet, container.LivenessProbe.HTTPGet

	assert.Equal(t, "/get_all_segments", readiness.Path,
		"readiness asks a gated route, which 503s until the service plane is up")
	assert.Equal(t, "/health", liveness.Path,
		"liveness asks an ungated route, which answers while the process answers at all")
	assert.NotEqual(t, readiness.Path, liveness.Path,
		"the two probes ask different questions; one path for both makes readiness inert")

	assert.Equal(t, int32(LeaderMetricsPort), readiness.Port.IntVal,
		"both probes go to the admin port, which is where every HTTP route lives")
	assert.Equal(t, int32(LeaderMetricsPort), liveness.Port.IntVal)

	assert.Greater(t, liveness.Port.IntVal, int32(0))
	assert.Greater(t, container.LivenessProbe.FailureThreshold, container.ReadinessProbe.FailureThreshold,
		"liveness tolerates more failures than readiness: withholding traffic is cheap, killing a "+
			"master that is merely slow to activate is not")
}

// TestLeaderWorkload_PodIdentityEnv pins that the argv's $(VAR) references resolve. T4
// renders -pod_name=$(KUBERNETES_POD_NAME); without the matching downward-API entry here the flag
// reaches the process as the literal string, and the master would take that for a pod name.
func TestLeaderWorkload_PodIdentityEnv(t *testing.T) {
	container := leaderContainer(t, testBackend(), "mooncake:v0.3.13")

	byName := make(map[string]core.EnvVar, len(container.Env))
	for _, env := range container.Env {
		byName[env.Name] = env
	}

	for name, field := range map[string]string{
		LeaderPodNameEnv:      "metadata.name",
		LeaderPodNamespaceEnv: "metadata.namespace",
	} {
		env, ok := byName[name]
		require.True(t, ok, "%s must be defined; the rendered argv refers to it", name)
		require.NotNil(t, env.ValueFrom, "%s comes from the downward API, not a literal", name)
		require.NotNil(t, env.ValueFrom.FieldRef)
		assert.Equal(t, field, env.ValueFrom.FieldRef.FieldPath)
	}
}

// TestLeaderWorkload_ClaimsNoHost pins the absences. The leader is a metadata service: it
// holds no cache bytes, so it needs neither the host's network namespace nor any device. The member
// is the side that needs those (F9), and a leader that acquired them would be a privilege nobody
// asked for.
func TestLeaderWorkload_ClaimsNoHost(t *testing.T) {
	deploy := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")
	podSpec := deploy.Spec.Template.Spec

	assert.False(t, podSpec.HostNetwork, "the leader is not hostNetwork")
	assert.Empty(t, podSpec.Volumes, "the leader mounts nothing")

	container := podSpec.Containers[0]
	assert.Empty(t, container.VolumeMounts)
	if container.SecurityContext != nil {
		assert.NotEqual(t, ptr.To(true), container.SecurityContext.Privileged,
			"never privileged")
	}
}

// multiTenantBackend is the canonical backend with the quota ledger turned on — the one shape that
// makes the leader mount anything at all.
func multiTenantBackend() *workercore.KVCacheBackend {
	return testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Leader.MultiTenancy = true
	})
}

// TestLeaderWorkload_QuotaPolicyVolumeIsWritable is about the one thing the master cannot start
// without, and the three shortcuts that each look like they would work.
//
// The master persists to its policy connector BEFORE applying an admin-API change and answers
// PERSISTENT_FAIL when that write fails, so the file has to be somewhere writable — which a ConfigMap
// mount is not. It also loads the file at startup and rethrows an unopenable one, so the volume
// cannot start out empty. The shape that satisfies both is an emptyDir seeded by an initContainer,
// and this test pins each half of it.
func TestLeaderWorkload_QuotaPolicyVolumeIsWritable(t *testing.T) {
	deploy := RenderLeaderDeployment(multiTenantBackend(), "mooncake:v0.3.13")
	podSpec := deploy.Spec.Template.Spec

	policy := volumeNamed(t, podSpec.Volumes, "tenant-quota-policy")
	require.NotNil(t, policy.EmptyDir,
		"the policy file lives on an emptyDir: the master renames a temp file over it on every "+
			"admin write, and a ConfigMap mount is read-only")

	seed := volumeNamed(t, podSpec.Volumes, "tenant-quota-policy-seed")
	require.NotNil(t, seed.ConfigMap)
	assert.Equal(t, "mooncake-dram-tenant-quota-policy", seed.ConfigMap.Name,
		"the seed is the ConfigMap the pool reconciler renders for this backend")
	require.NotNil(t, seed.ConfigMap.Optional)
	assert.True(t, *seed.ConfigMap.Optional,
		"optional, because the POOL reconciler writes that ConfigMap: a multi-tenant backend no "+
			"pool has bound yet would otherwise wait forever for a volume nobody is going to create")

	require.Len(t, podSpec.InitContainers, 1)
	initContainer := podSpec.InitContainers[0]
	assert.Equal(t, "mooncake:v0.3.13", initContainer.Image,
		"the leader's own image, so an air-gapped install pulls nothing extra for the copy")
	assert.Equal(t, podSpec.Containers[0].ImagePullPolicy, initContainer.ImagePullPolicy)

	// The initContainer writes the emptyDir and reads the seed; the leader only reads the emptyDir.
	// A leader that could see the seed would invite a future change to point the URI straight at it.
	assert.ElementsMatch(t,
		[]string{"tenant-quota-policy", "tenant-quota-policy-seed"},
		mountedVolumeNames(initContainer),
		"the initContainer is the only thing that reads the seed")
	assert.Equal(t, []string{"tenant-quota-policy"}, mountedVolumeNames(podSpec.Containers[0]))

	mount := podSpec.Containers[0].VolumeMounts[0]
	assert.False(t, mount.ReadOnly, "read-only here would break every quota write")
	assert.Equal(t, "/var/lib/mooncake", mount.MountPath)
	assert.Contains(t, podSpec.Containers[0].Args,
		"-tenant_quota_connector_uri=/var/lib/mooncake/tenant-quota-policy.yaml",
		"the URI names a file inside the writable mount, not inside the seed")
}

// TestLeaderWorkload_QuotaPolicySeedFallsBackToTheEmptyPolicy is about the case nobody would
// construct on purpose: multi-tenancy on, and no pool bound to the backend yet.
//
// Nothing has written the ConfigMap at that point, so the optional volume mounts as an empty
// directory and the copy finds no source. The master's parser refuses an empty file as firmly as a
// missing one — it wants a YAML map with version 1 and a tenants sequence — so the initContainer has
// to write a real document, and the document has to be the renderer's own.
func TestLeaderWorkload_QuotaPolicySeedFallsBackToTheEmptyPolicy(t *testing.T) {
	deploy := RenderLeaderDeployment(multiTenantBackend(), "mooncake:v0.3.13")
	initContainer := deploy.Spec.Template.Spec.InitContainers[0]

	// The document and both paths travel as environment variables so the command is a fixed string:
	// a policy interpolated into the shell would put a rendered YAML document inside quoting rules.
	env := map[string]string{}
	for _, e := range initContainer.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "/etc/mooncake/tenant-quota-policy/tenant-quota-policy.yaml", env["POLICY_SEED"])
	assert.Equal(t, "/var/lib/mooncake/tenant-quota-policy.yaml", env["POLICY_FILE"])

	rendered, err := RenderQuotaPolicy(nil)
	require.NoError(t, err)
	assert.Equal(t, string(rendered), env["POLICY_EMPTY"],
		"the fallback is what the renderer emits for an empty tenant set, not a second literal")

	require.Len(t, initContainer.Command, 3)
	assert.Equal(t, []string{"sh", "-c"}, initContainer.Command[:2])
	for _, want := range []string{"$POLICY_SEED", "$POLICY_FILE", "$POLICY_EMPTY"} {
		assert.Contains(t, initContainer.Command[2], want)
	}
}

// TestLeaderWorkload_QuotaPolicyVolumeIsGatedOnMultiTenancy is the negative half, asserted field by
// field rather than as one shape comparison, so the switch cannot half-apply.
func TestLeaderWorkload_QuotaPolicyVolumeIsGatedOnMultiTenancy(t *testing.T) {
	deploy := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")
	podSpec := deploy.Spec.Template.Spec

	assert.Empty(t, podSpec.Volumes)
	assert.Empty(t, podSpec.InitContainers)
	assert.Empty(t, podSpec.Containers[0].VolumeMounts)
	for _, arg := range podSpec.Containers[0].Args {
		assert.NotContains(t, arg, "tenant_quota_connector")
	}
}

// volumeNamed returns the named volume, failing the test rather than panicking when the render
// produced no such volume.
func volumeNamed(t *testing.T, volumes []core.Volume, name string) core.Volume {
	t.Helper()
	for _, v := range volumes {
		if v.Name == name {
			return v
		}
	}
	require.FailNow(t, "no volume named "+name)
	return core.Volume{}
}

// mountedVolumeNames is what a container reads, by volume name, so a case can assert the whole set
// at once instead of one mount at a time.
func mountedVolumeNames(container core.Container) []string {
	names := make([]string, 0, len(container.VolumeMounts))
	for _, m := range container.VolumeMounts {
		names = append(names, m.Name)
	}
	return names
}

// TestLeaderWorkload_SelectorSurvivesASpecChange is about an immutable field.
//
// A Deployment's spec.selector cannot be changed after creation: an update carrying a different one
// is rejected by the API server, and the object is then stuck until somebody deletes it by hand. So
// the selector must be built only from things that do not change over a backend's life. This test
// mutates every spec field the webhook permits an update to and asserts the selector does not move.
func TestLeaderWorkload_SelectorSurvivesASpecChange(t *testing.T) {
	before := RenderLeaderDeployment(testBackend(), "mooncake:v0.3.13")

	after := RenderLeaderDeployment(testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Image = "mooncake:v0.4.0"
		k.Spec.Transport.Protocol = "RDMA"
		leader := &k.Spec.Connection.Managed.Leader
		leader.AllocationStrategy = "Random"
		leader.ExtraArgs = map[string]string{"client_ttl": "30"}
	}), "mooncake:v0.4.0")

	require.NotNil(t, before.Spec.Selector)
	require.NotNil(t, after.Spec.Selector)
	assert.Equal(t, before.Spec.Selector, after.Spec.Selector,
		"the selector is immutable in the API, so it may not carry anything a spec update can move")

	assert.NotEqual(t, before.Spec.Template.Spec.Containers[0].Image,
		after.Spec.Template.Spec.Containers[0].Image,
		"the mutation did reach the template — otherwise the assertion above proves nothing")

	for key, value := range before.Spec.Selector.MatchLabels {
		assert.Equal(t, value, before.Spec.Template.Labels[key],
			"the template must carry every selector label, or the Deployment matches no Pod")
	}
}

// TestLeaderWorkload_IsDeterministic pins that one backend renders identically every time.
// The reconciler converges this object on every pass, so a render that wandered — a map range in the
// argv, say — would rewrite the Deployment forever and restart the master with it.
func TestLeaderWorkload_IsDeterministic(t *testing.T) {
	kvcb := testBackend(func(k *workercore.KVCacheBackend) {
		k.Spec.Connection.Managed.Leader.ExtraArgs = map[string]string{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5",
		}
	})

	first := RenderLeaderDeployment(kvcb, "mooncake:v0.3.13")
	for range 20 {
		assert.Equal(t, first, RenderLeaderDeployment(kvcb, "mooncake:v0.3.13"))
	}
}

// TestLeaderWorkload_IsOwnedByTheBackend asserts the garbage-collection path. The rendered
// objects are namespaced dependents of a cluster-scoped owner, which is the direction that works.
func TestLeaderWorkload_IsOwnedByTheBackend(t *testing.T) {
	kvcb := testBackend()

	for name, obj := range map[string]meta.Object{
		"deployment": RenderLeaderDeployment(kvcb, "mooncake:v0.3.13"),
		"service":    RenderLeaderService(kvcb),
	} {
		refs := obj.GetOwnerReferences()
		require.Len(t, refs, 1, "%s carries exactly one owner", name)
		assert.Equal(t, "KVCacheBackend", refs[0].Kind, "%s", name)
		assert.Equal(t, kvcb.Name, refs[0].Name, "%s", name)
		assert.Equal(t, kvcb.UID, refs[0].UID, "%s", name)
	}
}

func TestLeaderWorkload_Service(t *testing.T) {
	kvcb := testBackend()
	svc := RenderLeaderService(kvcb)
	deploy := RenderLeaderDeployment(kvcb, "mooncake:v0.3.13")

	assert.Equal(t, "mooncake-dram-leader", svc.Name)
	assert.Equal(t, kuberess.SystemNamespaceName, svc.Namespace)
	assert.Equal(t, core.ServiceTypeClusterIP, svc.Spec.Type,
		"the leader is reached from inside the cluster; nothing here publishes it outward")

	assert.Equal(t, deploy.Spec.Selector.MatchLabels, svc.Spec.Selector,
		"the Service selects exactly what the Deployment does, or it fronts nothing")

	ports := make(map[string]int32, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports[p.Name] = p.Port
	}
	assert.Equal(t, map[string]int32{
		"rpc":   LeaderRPCPort,
		"admin": LeaderMetricsPort,
	}, ports, "both ports are published: one is what engines connect to, one is what this operator reads")
}

// TestLeaderWorkload_Endpoints pins what status.endpoints publishes for a managed backend: a Client address
// an engine dials and an Admin address this operator scrapes. They are the same host on two ports,
// and the roles are named because a consumer handed the wrong one fails at connect time with nothing
// to point at.
func TestLeaderWorkload_Endpoints(t *testing.T) {
	got := LeaderEndpoints(testBackend())

	byName := make(map[string]string, len(got))
	for _, e := range got {
		byName[e.Name] = e.Address
	}

	host := "mooncake-dram-leader." + kuberess.SystemNamespaceName + ".svc"
	assert.Equal(t, map[string]string{
		workercore.KVCacheBackendEndpointNameClient: host + ":50051",
		workercore.KVCacheBackendEndpointNameAdmin:  host + ":9003",
	}, byName)
}
