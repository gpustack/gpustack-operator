package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

func newTestModelDeploymentForger(mutate ...func(*workercore.ModelDeploymentForger)) *workercore.ModelDeploymentForger {
	mdf := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-forger"},
		Spec: workercore.ModelDeploymentForgerSpec{
			TargetRef: core.LocalObjectReference{Name: "qwen-pd"},
			Mode:      workercore.ModelDeploymentForgerModeActive,
			Roles: []workercore.ModelDeploymentForgerRoleSpec{
				{
					Name:                    "decode",
					MaxKernelsPerGeneration: 4,
					MinSpeedupRatio:         "1.05",
				},
			},
			Probe: workercore.ModelDeploymentForgerProbeSpec{
				Steps:             4,
				CooldownSeconds:   600,
				MinActiveRequests: 1,
			},
			Sandbox: workercore.ModelDeploymentForgerSandboxSpec{
				RunnerMode:      workercore.ModelDeploymentForgerRunnerModeExternal,
				RunnerImage:     "gpustack/forge-runner:v0.1.0",
				TimeoutSeconds:  1800,
				OutputSizeLimit: ptr.To(resource.MustParse("2Gi")),
			},
			Rollout: workercore.ModelDeploymentForgerRolloutSpec{
				MaxRetainedGenerations: 3,
				FailureTimeoutSeconds:  600,
			},
		},
	}
	for _, m := range mutate {
		m(mdf)
	}
	return mdf
}

func newTestForgerTargetDeployment(engine string, roles ...workercore.ModelDeploymentRole) *workercore.ModelDeployment {
	if len(roles) == 0 {
		roles = []workercore.ModelDeploymentRole{{
			Name:            "decode",
			Kind:            workercore.ModelDeploymentRoleKindDecode,
			Replicas:        2,
			AcceleratorType: "h20-8x",
		}}
	}
	return &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-pd"},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: "Qwen/Qwen2.5-72B-Instruct"},
			Engine: workercore.ModelDeploymentEngine{
				Name:  engine,
				Image: "vllm/vllm-openai:v0.8.5",
			},
			Roles: roles,
		},
	}
}

func newTestForgerWebhook(objs ...ctrlcli.Object) *ModelDeploymentForgerWebhook {
	builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	return &ModelDeploymentForgerWebhook{
		APIReader: builder.Build(),
		worker:    testWorkerUsername,
	}
}

func TestModelDeploymentForgerWebhookValidateCreate(t *testing.T) {
	cases := []struct {
		name      string
		objs      []ctrlcli.Object
		in        *workercore.ModelDeploymentForger
		wantField string
	}{
		{
			name: "valid Active External forger",
			in:   newTestModelDeploymentForger(),
		},
		{
			name: "valid defaults with omitted optional structs and Mock runnerMode",
			in: &workercore.ModelDeploymentForger{
				ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-mock"},
				Spec: workercore.ModelDeploymentForgerSpec{
					TargetRef: core.LocalObjectReference{Name: "qwen-pd"},
					Roles:     []workercore.ModelDeploymentForgerRoleSpec{{Name: "decode"}},
					Sandbox: workercore.ModelDeploymentForgerSandboxSpec{
						RunnerMode: workercore.ModelDeploymentForgerRunnerModeMock,
					},
				},
			},
		},
		{
			name: "valid ObserveOnly mode with boundary values",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Mode = workercore.ModelDeploymentForgerModeObserveOnly
				m.Spec.Roles[0].MaxKernelsPerGeneration = 1
				m.Spec.Roles[0].MinSpeedupRatio = "1.01"
				m.Spec.Probe.Steps = 3
				m.Spec.Probe.CooldownSeconds = 60
				m.Spec.Probe.MinActiveRequests = 0
				m.Spec.Sandbox.TimeoutSeconds = 60
				m.Spec.Sandbox.OutputSizeLimit = ptr.To(resource.MustParse("64Mi"))
				m.Spec.Sandbox.Resources = &workercore.ModelDeploymentForgerSandboxResources{
					CPU: ptr.To(resource.MustParse("4")),
					RAM: ptr.To(resource.MustParse("16Gi")),
				}
				m.Spec.Rollout.MaxRetainedGenerations = 1
				m.Spec.Rollout.FailureTimeoutSeconds = 1
			}),
		},
		{
			name: "valid Shadow mode with upper boundary values",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Mode = workercore.ModelDeploymentForgerModeShadow
				m.Spec.Roles[0].MaxKernelsPerGeneration = 16
				m.Spec.Probe.Steps = 5
				m.Spec.Sandbox.TimeoutSeconds = 14400
				m.Spec.Sandbox.OutputSizeLimit = ptr.To(resource.MustParse("16Gi"))
				m.Spec.Rollout.MaxRetainedGenerations = 16
			}),
		},
		{
			name: "rejects empty targetRef name",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.TargetRef.Name = ""
			}),
			wantField: "spec.targetRef.name",
		},
		{
			name: "rejects whitespace-padded targetRef name",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.TargetRef.Name = " qwen-pd "
			}),
			wantField: "spec.targetRef.name",
		},
		{
			name: "rejects invalid mode",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Mode = "AutoPilot"
			}),
			wantField: "spec.mode",
		},
		{
			name: "rejects empty roles",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles = nil
			}),
			wantField: "spec.roles",
		},
		{
			name: "rejects empty role name",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].Name = ""
			}),
			wantField: "spec.roles[0].name",
		},
		{
			name: "rejects duplicate role names",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles = append(m.Spec.Roles, workercore.ModelDeploymentForgerRoleSpec{Name: "decode"})
			}),
			wantField: "spec.roles[1].name",
		},
		{
			name: "rejects maxKernelsPerGeneration above 16",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].MaxKernelsPerGeneration = 17
			}),
			wantField: "spec.roles[0].maxKernelsPerGeneration",
		},
		{
			name: "rejects negative maxKernelsPerGeneration",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].MaxKernelsPerGeneration = -1
			}),
			wantField: "spec.roles[0].maxKernelsPerGeneration",
		},
		{
			name: "rejects minSpeedupRatio below 1.01",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].MinSpeedupRatio = "1.009"
			}),
			wantField: "spec.roles[0].minSpeedupRatio",
		},
		{
			name: "rejects non-decimal minSpeedupRatio",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].MinSpeedupRatio = "105/100"
			}),
			wantField: "spec.roles[0].minSpeedupRatio",
		},
		{
			name: "rejects probe steps below 3",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Probe.Steps = 2
			}),
			wantField: "spec.probe.steps",
		},
		{
			name: "rejects probe steps above 5",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Probe.Steps = 6
			}),
			wantField: "spec.probe.steps",
		},
		{
			name: "rejects probe cooldownSeconds below 60",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Probe.CooldownSeconds = 59
			}),
			wantField: "spec.probe.cooldownSeconds",
		},
		{
			name: "rejects negative probe minActiveRequests",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Probe.MinActiveRequests = -1
			}),
			wantField: "spec.probe.minActiveRequests",
		},
		{
			name: "rejects invalid sandbox runnerMode",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.RunnerMode = "Local"
			}),
			wantField: "spec.sandbox.runnerMode",
		},
		{
			name: "rejects External mode without runnerImage",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.RunnerImage = ""
			}),
			wantField: "spec.sandbox.runnerImage",
		},
		{
			name: "rejects External runnerImage with whitespace",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.RunnerImage = "gpustack/forge runner:v1"
			}),
			wantField: "spec.sandbox.runnerImage",
		},
		{
			name: "rejects sandbox timeoutSeconds below 60",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.TimeoutSeconds = 59
			}),
			wantField: "spec.sandbox.timeoutSeconds",
		},
		{
			name: "rejects sandbox timeoutSeconds above 14400",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.TimeoutSeconds = 14401
			}),
			wantField: "spec.sandbox.timeoutSeconds",
		},
		{
			name: "rejects sandbox outputSizeLimit below 64Mi",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.OutputSizeLimit = ptr.To(resource.MustParse("32Mi"))
			}),
			wantField: "spec.sandbox.outputSizeLimit",
		},
		{
			name: "rejects sandbox outputSizeLimit above 16Gi",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.OutputSizeLimit = ptr.To(resource.MustParse("32Gi"))
			}),
			wantField: "spec.sandbox.outputSizeLimit",
		},
		{
			name: "rejects non-positive sandbox cpu resource",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.Resources = &workercore.ModelDeploymentForgerSandboxResources{
					CPU: ptr.To(resource.MustParse("0")),
				}
			}),
			wantField: "spec.sandbox.resources.cpu",
		},
		{
			name: "rejects non-positive sandbox ram resource",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Sandbox.Resources = &workercore.ModelDeploymentForgerSandboxResources{
					RAM: ptr.To(resource.MustParse("-1Gi")),
				}
			}),
			wantField: "spec.sandbox.resources.ram",
		},
		{
			name: "rejects rollout maxRetainedGenerations above 16",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Rollout.MaxRetainedGenerations = 17
			}),
			wantField: "spec.rollout.maxRetainedGenerations",
		},
		{
			name: "rejects negative rollout failureTimeoutSeconds",
			in: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Rollout.FailureTimeoutSeconds = -1
			}),
			wantField: "spec.rollout.failureTimeoutSeconds",
		},
		{
			name: "allowed runner image prefix permits matching image",
			objs: []ctrlcli.Object{
				&core.Secret{
					ObjectMeta: meta.ObjectMeta{
						Namespace: setting.DelegatedSecretNamespace,
						Name:      setting.DelegatedSecretName,
					},
					Data: map[string][]byte{
						forgerAllowedRunnerImagePrefixesSettingName: []byte("registry.corp.io/forge/, gpustack/"),
					},
				},
			},
			in: newTestModelDeploymentForger(),
		},
		{
			name: "allowed runner image prefix forbids non-matching image",
			objs: []ctrlcli.Object{
				&core.Secret{
					ObjectMeta: meta.ObjectMeta{
						Namespace: setting.DelegatedSecretNamespace,
						Name:      setting.DelegatedSecretName,
					},
					Data: map[string][]byte{
						forgerAllowedRunnerImagePrefixesSettingName: []byte("registry.corp.io/forge/"),
					},
				},
			},
			in:        newTestModelDeploymentForger(),
			wantField: "spec.sandbox.runnerImage",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wh := newTestForgerWebhook(c.objs...)
			_, err := wh.ValidateCreate(context.Background(), c.in)
			if c.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertInvalidField(t, err, c.wantField)
		})
	}

	t.Run("runner image prefix lookup failure returns internal error", func(t *testing.T) {
		orig := forgerAllowedRunnerImagePrefixesValue
		t.Cleanup(func() { forgerAllowedRunnerImagePrefixesValue = orig })
		forgerAllowedRunnerImagePrefixesValue = func(context.Context, ctrlcli.Reader) (string, error) {
			return "", errors.New("api unreachable")
		}

		wh := newTestForgerWebhook()
		_, err := wh.ValidateCreate(context.Background(), newTestModelDeploymentForger())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "api unreachable")
	})
}

func TestModelDeploymentForgerWebhookValidateAgainstTarget(t *testing.T) {
	cases := []struct {
		name      string
		target    *workercore.ModelDeployment
		forger    *workercore.ModelDeploymentForger
		wantField string
		wantMsg   string
	}{
		{
			name:   "admits when target does not exist yet",
			forger: newTestModelDeploymentForger(),
		},
		{
			name:   "admits eligible vLLM Decode role",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineVLLM),
			forger: newTestModelDeploymentForger(),
		},
		{
			name: "admits eligible SGLang Server role",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineSGLang, workercore.ModelDeploymentRole{
				Name:            "server",
				Kind:            workercore.ModelDeploymentRoleKindServer,
				Replicas:        2,
				AcceleratorType: "h20-8x",
			}),
			forger: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].Name = "server"
			}),
		},
		{
			name:   "rejects missing target role",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineVLLM),
			forger: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].Name = "prefill"
			}),
			wantField: "spec.roles[0].name",
			wantMsg:   "does not declare a role",
		},
		{
			name: "rejects Encode role",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentRole{
				Name:            "encode",
				Kind:            workercore.ModelDeploymentRoleKindEncode,
				Replicas:        1,
				AcceleratorType: "h20-8x",
			}),
			forger: newTestModelDeploymentForger(func(m *workercore.ModelDeploymentForger) {
				m.Spec.Roles[0].Name = "encode"
			}),
			wantField: "spec.roles[0].name",
			wantMsg:   "Encode",
		},
		{
			name: "rejects CPU-only role",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentRole{
				Name:     "decode",
				Kind:     workercore.ModelDeploymentRoleKindDecode,
				Replicas: 1,
			}),
			forger:    newTestModelDeploymentForger(),
			wantField: "spec.roles[0].name",
			wantMsg:   "CPU-only",
		},
		{
			name: "rejects unmanaged role with command",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentRole{
				Name:            "decode",
				Kind:            workercore.ModelDeploymentRoleKindDecode,
				Replicas:        1,
				AcceleratorType: "h20-8x",
				Command:         []string{"python3", "-m", "my_server"},
			}),
			forger:    newTestModelDeploymentForger(),
			wantField: "spec.roles[0].name",
			wantMsg:   "unmanaged",
		},
		{
			name: "rejects Elastic EP role",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentRole{
				Name:            "decode",
				Kind:            workercore.ModelDeploymentRoleKindDecode,
				Replicas:        1,
				AcceleratorType: "h20-8x",
				RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
					Parameters: map[string]string{
						runtimecontrol.ParamElasticDataParallelWidth: "2",
					},
				},
			}),
			forger:    newTestModelDeploymentForger(),
			wantField: "spec.roles[0].name",
			wantMsg:   "Elastic EP",
		},
		{
			name: "rejects SGLang-Ray role",
			target: newTestForgerTargetDeployment(workercore.ModelDeploymentEngineSGLang, workercore.ModelDeploymentRole{
				Name:            "decode",
				Kind:            workercore.ModelDeploymentRoleKindDecode,
				Replicas:        1,
				AcceleratorType: "h20-8x",
				ExtraArgs:       []string{"--distributed-executor-backend=ray"},
			}),
			forger:    newTestModelDeploymentForger(),
			wantField: "spec.roles[0].name",
			wantMsg:   "SGLang Ray",
		},
		{
			name: "rejects unsupported engine",
			target: newTestForgerTargetDeployment("MindIE", workercore.ModelDeploymentRole{
				Name:            "decode",
				Kind:            workercore.ModelDeploymentRoleKindDecode,
				Replicas:        1,
				AcceleratorType: "ascend-910b",
			}),
			forger:    newTestModelDeploymentForger(),
			wantField: "spec.roles[0].name",
			wantMsg:   "supported engines are vLLM and SGLang",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.target != nil {
				objs = append(objs, c.target)
			}
			wh := newTestForgerWebhook(objs...)
			_, err := wh.ValidateCreate(context.Background(), c.forger)
			if c.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assertInvalidField(t, err, c.wantField)
			if c.wantMsg != "" {
				assert.Contains(t, err.Error(), c.wantMsg)
			}
		})
	}

	t.Run("nil APIReader fails closed", func(t *testing.T) {
		wh := &ModelDeploymentForgerWebhook{worker: testWorkerUsername}
		_, err := wh.ValidateCreate(context.Background(), newTestModelDeploymentForger())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no reader")
	})
}

func TestModelDeploymentForgerWebhookValidateUpdate(t *testing.T) {
	target := newTestForgerTargetDeployment(workercore.ModelDeploymentEngineVLLM)
	wh := newTestForgerWebhook(target)

	t.Run("mutable fields update cleanly", func(t *testing.T) {
		old := newTestModelDeploymentForger()
		updated := old.DeepCopy()
		updated.Spec.Suspend = true
		updated.Spec.Mode = workercore.ModelDeploymentForgerModeShadow

		_, err := wh.ValidateUpdate(admissionContext(testKernelTenantUsername, ""), old, updated)
		require.NoError(t, err)
	})

	t.Run("immutable targetRef rejects change", func(t *testing.T) {
		old := newTestModelDeploymentForger()
		updated := old.DeepCopy()
		updated.Spec.TargetRef.Name = "other-deployment"

		_, err := wh.ValidateUpdate(admissionContext(testKernelTenantUsername, ""), old, updated)
		require.Error(t, err)
		assertInvalidField(t, err, "spec.targetRef.name")
		assert.Contains(t, strings.ToLower(err.Error()), "immutable")
	})

	t.Run("terminating forger skips spec validation so finalizers can clear", func(t *testing.T) {
		old := newTestModelDeploymentForger()
		now := meta.Now()
		updated := old.DeepCopy()
		updated.DeletionTimestamp = &now
		updated.Spec.TargetRef.Name = "other-deployment"

		_, err := wh.ValidateUpdate(admissionContext(testKernelTenantUsername, ""), old, updated)
		require.NoError(t, err)
	})

	t.Run("delete is allowed", func(t *testing.T) {
		_, err := wh.ValidateDelete(context.Background(), newTestModelDeploymentForger())
		require.NoError(t, err)
	})
}

func TestModelDeploymentForgerStatusGuard(t *testing.T) {
	cases := []struct {
		name        string
		worker      string
		ctx         context.Context
		terminating bool
		wantRule    string
	}{
		{
			name:   "worker writes status",
			worker: testWorkerUsername,
			ctx:    admissionContext(testWorkerUsername, "status"),
		},
		{
			name:     "tenant cannot spoof activeSandbox podUID via status",
			worker:   testWorkerUsername,
			ctx:      admissionContext(testKernelTenantUsername, "status"),
			wantRule: modelDeploymentForgerStatusRuleWriter,
		},
		{
			name:     "admin cannot write status",
			worker:   testWorkerUsername,
			ctx:      admissionContext("kubernetes-admin", "status"),
			wantRule: modelDeploymentForgerStatusRuleWriter,
		},
		{
			name:        "tenant cannot write status of Terminating forger",
			worker:      testWorkerUsername,
			ctx:         admissionContext(testKernelTenantUsername, "status"),
			terminating: true,
			wantRule:    modelDeploymentForgerStatusRuleWriter,
		},
		{
			name:     "empty worker identity refuses status writes",
			worker:   "",
			ctx:      admissionContext(testWorkerUsername, "status"),
			wantRule: modelDeploymentForgerStatusRuleWriter,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := newTestModelDeploymentForger()
			if c.terminating {
				now := meta.Now()
				old.DeletionTimestamp = &now
				old.Finalizers = []string{workercore.ModelDeploymentForgerFinalizerKey}
			}
			updated := old.DeepCopy()
			updated.Status.Roles = []workercore.ModelDeploymentForgerRoleStatus{
				{
					Name:  "decode",
					Phase: workercore.ModelDeploymentForgerRolePhaseForging,
					ActiveSandbox: &workercore.ModelDeploymentForgerSandboxStatus{
						PodName: "spoofed-pod",
						PodUID:  "spoofed-uid",
					},
				},
			}

			_, err := (&ModelDeploymentForgerWebhook{worker: c.worker}).ValidateUpdate(c.ctx, old, updated)
			if c.wantRule == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantRule)
		})
	}

	t.Run("missing admission request returns internal error", func(t *testing.T) {
		wh := &ModelDeploymentForgerWebhook{worker: testWorkerUsername}
		err := wh.validateModelDeploymentForgerStatus(context.Background(), newTestModelDeploymentForger())
		require.Error(t, err)
		assert.True(t, kerrors.IsInternalError(err))
	})

	t.Run("implements ReceiveDeletionUpdate", func(t *testing.T) {
		_, ok := any(new(ModelDeploymentForgerWebhook)).(webhook.ReceiveDeletionUpdate)
		assert.True(t, ok)
	})
}
