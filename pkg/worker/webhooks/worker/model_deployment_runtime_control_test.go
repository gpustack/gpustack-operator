package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

func runtimeControlWebhook(extraObjs ...ctrlcli.Object) *ModelDeploymentWebhook {
	objs := make([]ctrlcli.Object, 0, 3+len(extraObjs))
	objs = append(objs,
		servingInstanceType("h20-8x", 8),
		servingInstanceType("ascend-910b", 8, func(it *worker.InstanceType) {
			it.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
		}),
		servingInstanceType("mi300x-8x", 8, func(it *worker.InstanceType) {
			it.Status.Detail.Manufacturer = nodefeature.ManufacturerAMD
		}),
	)
	objs = append(objs, epdPlatformTypes()...)
	for _, extra := range extraObjs {
		if extra != nil {
			objs = append(objs, extra.DeepCopyObject().(ctrlcli.Object))
		}
	}
	return newModelDeploymentWebhookWith(objs)
}

func runtimeControlMD(engine string, mutate func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	md := modelDeployment(engine)
	md.Spec.Roles[0].Resources = cards(1)
	if engine == workercore.ModelDeploymentEngineSGLang {
		md.Spec.Engine.Image = "lmsysorg/sglang:v0.5.0"
		md.Spec.Engine.Version = "0.5.0"
	}
	if mutate != nil {
		mutate(md)
	}
	return md
}

func TestValidateModelDeploymentModel_ReservedLoRAAdapters(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		old       func() *workercore.ModelDeployment
		md        func() *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "nil loraAdapters admitted",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, nil)
			},
		},
		{
			name: "empty loraAdapters slice admitted",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Model.LoRAAdapters = []workercore.ModelDeploymentLoRAAdapter{}
				})
			},
		},
		{
			name: "non-empty loraAdapters rejected on create",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Model.LoRAAdapters = []workercore.ModelDeploymentLoRAAdapter{
						{
							Name:        "sql-lora",
							ArtifactRef: core.LocalObjectReference{Name: "sql-lora-artifact"},
							SubPath:     "adapter",
							Pinned:      true,
						},
					}
				})
			},
			wantField: "spec.model.loraAdapters",
			wantMsg:   "ModelArtifact",
		},
		{
			name: "non-empty loraAdapters rejected on update",
			old: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, nil)
			},
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Model.LoRAAdapters = []workercore.ModelDeploymentLoRAAdapter{
						{
							Name:        "sql-lora",
							ArtifactRef: core.LocalObjectReference{Name: "sql-lora-artifact"},
						},
					}
				})
			},
			wantField: "spec.model.loraAdapters",
			wantMsg:   "reserved",
		},
	}

	w := runtimeControlWebhook()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.old == nil {
				_, err = w.ValidateCreate(context.Background(), tc.md())
			} else {
				_, err = w.ValidateUpdate(context.Background(), tc.old(), tc.md())
			}
			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantField)
			if tc.wantMsg != "" {
				assert.Contains(t, err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestValidateModelDeploymentRole_Pause(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		old  func() *workercore.ModelDeployment
		md   func() *workercore.ModelDeployment
	}{
		{
			name: "pause true admitted on managed vLLM role",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Pause = true
				})
			},
		},
		{
			name: "pause true admitted on unmanaged role with command",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Command = []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct"}
					md.Spec.Roles[0].Pause = true
				})
			},
		},
		{
			name: "pause true admitted on CPU-only role",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].AcceleratorType = ""
					md.Spec.Roles[0].Resources = nil
					md.Spec.Roles[0].Pause = true
				})
			},
		},
		{
			name: "pause true admitted on Encode role",
			md: func() *workercore.ModelDeployment {
				return epdDeployment(func(md *workercore.ModelDeployment) {
					epdRoleByName(md, "encode").Pause = true
				})
			},
		},
		{
			name: "pause true admitted on SGLang Ray role",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--distributed-executor-backend=ray"}
					md.Spec.Roles[0].Pause = true
				})
			},
		},
		{
			name: "pause mutable false to true on update",
			old: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, nil)
			},
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Pause = true
				})
			},
		},
		{
			name: "pause mutable true to false on update",
			old: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Pause = true
				})
			},
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Pause = false
				})
			},
		},
	}

	w := runtimeControlWebhook()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.old == nil {
				_, err = w.ValidateCreate(context.Background(), tc.md())
			} else {
				_, err = w.ValidateUpdate(context.Background(), tc.old(), tc.md())
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateModelDeploymentRuntimeControl_ScopeGates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		md        func() *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "vLLM managed server with runtimeControl parameters admitted",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "SGLang managed server with runtimeControl parameters admitted",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--max-running-requests", "64"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "vLLM managed server with empty runtimeControl admitted",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
		},
		{
			name: "unmanaged role with command rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Command = []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "command",
		},
		{
			name: "Encode role rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.KVCache = nil
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterVLLM}
					md.Spec.Roles = []workercore.ModelDeploymentRole{
						{
							Name:            "encode",
							Kind:            workercore.ModelDeploymentRoleKindEncode,
							Replicas:        1,
							ReplicaSize:     1,
							AcceleratorType: "h20-8x",
							Resources:       cards(1),
							RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
								Parameters: map[string]string{
									runtimecontrol.ParamSchedulingMaxActiveRequests: "16",
								},
							},
						},
						{
							Name:            "prefill",
							Kind:            workercore.ModelDeploymentRoleKindPrefill,
							Replicas:        1,
							AcceleratorType: "h20-8x",
							Resources:       cards(1),
						},
						{
							Name:            "decode",
							Kind:            workercore.ModelDeploymentRoleKindDecode,
							Replicas:        1,
							AcceleratorType: "h20-8x",
							Resources:       cards(1),
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "Encode",
		},
		{
			name: "CPU-only role rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].AcceleratorType = ""
					md.Spec.Roles[0].Resources = nil
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "16",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "acceleratorType",
		},
		{
			name: "SGLang Ray via --distributed-executor-backend=ray rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--distributed-executor-backend=ray"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "Ray",
		},
		{
			name: "SGLang Ray via --distributed-executor-backend ray rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--distributed-executor-backend", "ray"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "Ray",
		},
		{
			name: "SGLang Ray via --ray-address rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--ray-address=ray-head:6379"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "Ray",
		},
		{
			name: "SGLang Ray via --enable-ray rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--enable-ray"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "Ray",
		},
		{
			name: "SGLang Ray via RAY_ADDRESS env rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
						{Name: "RAY_ADDRESS", Value: "ray-head:6379"},
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "Ray",
		},
		{
			name: "SGLang standard torch distributed multi-node without Ray admitted",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ReplicaSize = 2
					md.Spec.Roles[0].ExtraArgs = []string{"--nnodes", "2", "--dist-init-addr", "leader:29500"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "vLLM with --distributed-executor-backend ray is not rejected by SGLang Ray gate",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--distributed-executor-backend=ray"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "unsupported engine rejects runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD("CustomEngine", func(md *workercore.ModelDeployment) {
					md.Spec.KVCache = nil
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl",
			wantMsg:   "not supported",
		},
	}

	w := runtimeControlWebhook()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateCreate(context.Background(), tc.md())
			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantField)
			if tc.wantMsg != "" {
				assert.Contains(t, err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestValidateModelDeploymentRuntimeControl_TranslatorParameters(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		md        func() *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "vLLM NVIDIA accepts all vLLM sentinel parameters with extraArgs ceilings and KV cache",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{
						"--max-num-seqs=64",
						"--max-num-batched-tokens=4096",
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests:         "32",
							runtimecontrol.ParamSchedulingMaxBatchedTokens:          "2048",
							runtimecontrol.ParamSchedulingLongPrefillTokenThreshold: "1024",
							runtimecontrol.ParamSchedulingCacheAwareWindow:          "8",
							runtimecontrol.ParamSchedulingCacheAwareThreshold:       "0.5",
							runtimecontrol.ParamSchedulingMaxInflightKVLoads:        "4",
							runtimecontrol.ParamSchedulingMaxAsyncKVLoadsPerStep:    "2",
						},
					}
				})
			},
		},
		{
			name: "vLLM Ascend accepts all vLLM sentinel parameters",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].AcceleratorType = "ascend-910b"
					md.Spec.Roles[0].ExtraArgs = []string{
						"--max-num-seqs", "64",
						"--max-num-batched-tokens", "4096",
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests:         "32",
							runtimecontrol.ParamSchedulingMaxBatchedTokens:          "2048",
							runtimecontrol.ParamSchedulingLongPrefillTokenThreshold: "512",
							runtimecontrol.ParamSchedulingCacheAwareWindow:          "4",
							runtimecontrol.ParamSchedulingCacheAwareThreshold:       "0.25",
							runtimecontrol.ParamSchedulingMaxInflightKVLoads:        "2",
							runtimecontrol.ParamSchedulingMaxAsyncKVLoadsPerStep:    "1",
						},
					}
				})
			},
		},
		{
			name: "vLLM AMD rejects vLLM parameters due to unsupported manufacturer",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].AcceleratorType = "mi300x-8x"
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.maxActiveRequests]",
			wantMsg:   "manufacturer",
		},
		{
			name: "SGLang NVIDIA accepts all SGLang sentinel parameters",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Resources = cards(2)
					md.Spec.Roles[0].ExtraArgs = []string{
						"--pp-size", "2",
						"--max-running-requests=64",
						"--chunked-prefill-size=4096",
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests:        "32",
							runtimecontrol.ParamSchedulingMaxBatchedTokens:         "2048",
							runtimecontrol.ParamSchedulingPrefillMaxRequests:       "16",
							runtimecontrol.ParamSchedulingScheduleConservativeness: "1.2",
							runtimecontrol.ParamSchedulingPrefillDecodeInterval:    "3",
							runtimecontrol.ParamSchedulingPPMaxMicroBatchSize:      "16",
							runtimecontrol.ParamSpeculativeAcceptThresholdSingle:   "0.5",
							runtimecontrol.ParamSpeculativeAcceptThresholdAcc:      "0.8",
							runtimecontrol.ParamSpeculativeDraftBudgetFraction:     "0.5",
						},
					}
				})
			},
		},
		{
			name: "unknown parameter key rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							"scheduling.nonExistentKey": "10",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.nonExistentKey]",
			wantMsg:   "unknown",
		},
		{
			name: "vLLM rejects SGLang-only parameter",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSpeculativeDraftBudgetFraction: "0.5",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[speculative.draftBudgetFraction]",
			wantMsg:   "not supported on engine",
		},
		{
			name: "SGLang rejects vLLM-only parameter",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingCacheAwareWindow: "4",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.cacheAwareWindow]",
			wantMsg:   "not supported on engine",
		},
		{
			name: "malformed integer parameter rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "abc",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.maxActiveRequests]",
			wantMsg:   "integer",
		},
		{
			name: "integer below lower bound rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "0",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.maxActiveRequests]",
			wantMsg:   "greater than or equal to 1",
		},
		{
			name: "decimal out of exclusive lower bound rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSpeculativeDraftBudgetFraction: "0.0",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[speculative.draftBudgetFraction]",
			wantMsg:   "(0.0, 1.0]",
		},
		{
			name: "vLLM maxActiveRequests exceeding --max-num-seqs rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "65",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.maxActiveRequests]",
			wantMsg:   "--max-num-seqs",
		},
		{
			name: "SGLang maxBatchedTokens exceeding --chunked-prefill-size rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--chunked-prefill-size", "2048"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxBatchedTokens: "4096",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.maxBatchedTokens]",
			wantMsg:   "--chunked-prefill-size",
		},
		{
			name: "vLLM longPrefillTokenThreshold exceeding effective maxBatchedTokens rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxBatchedTokens:          "1024",
							runtimecontrol.ParamSchedulingLongPrefillTokenThreshold: "2048",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.longPrefillTokenThreshold]",
			wantMsg:   "maxBatchedTokens",
		},
		{
			name: "SGLang prefillMaxRequests exceeding effective maxActiveRequests rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests:  "32",
							runtimecontrol.ParamSchedulingPrefillMaxRequests: "48",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.prefillMaxRequests]",
			wantMsg:   "maxActiveRequests",
		},
		{
			name: "SGLang ppMaxMicroBatchSize without pipeline parallelism rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingPPMaxMicroBatchSize: "4",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.ppMaxMicroBatchSize]",
			wantMsg:   "pipeline parallelism",
		},
		{
			name: "SGLang ppMaxMicroBatchSize exceeding maxActiveRequests / ppSize rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Resources = cards(2)
					md.Spec.Roles[0].ExtraArgs = []string{"--pp-size=2"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests:   "16",
							runtimecontrol.ParamSchedulingPPMaxMicroBatchSize: "9",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.ppMaxMicroBatchSize]",
			wantMsg:   "ppSize",
		},
		{
			name: "vLLM maxInflightKVLoads without KV connector rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.KVCache = nil
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxInflightKVLoads: "4",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[scheduling.maxInflightKVLoads]",
			wantMsg:   "KV connector",
		},
		{
			name: "vLLM maxAsyncKVLoadsPerStep with routed prefill/decode pair admitted",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.KVCache = nil
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
					md.Spec.Roles = []workercore.ModelDeploymentRole{
						{
							Name:            "prefill",
							Kind:            workercore.ModelDeploymentRoleKindPrefill,
							Replicas:        1,
							AcceleratorType: "h20-8x",
							Resources:       cards(1),
							RuntimeControl: &workercore.ModelDeploymentRoleRuntimeControl{
								Parameters: map[string]string{
									runtimecontrol.ParamSchedulingMaxAsyncKVLoadsPerStep: "2",
								},
							},
						},
						{
							Name:            "decode",
							Kind:            workercore.ModelDeploymentRoleKindDecode,
							Replicas:        1,
							AcceleratorType: "h20-8x",
							Resources:       cards(1),
						},
					}
				})
			},
		},
		{
			name: "empty elastic.dataParallelWidth rejected",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamElasticDataParallelWidth: "",
						},
					}
				})
			},
			wantField: "spec.roles[0].runtimeControl.parameters[elastic.dataParallelWidth]",
			wantMsg:   "integer",
		},
	}

	w := runtimeControlWebhook()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateCreate(context.Background(), tc.md())
			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantField)
			if tc.wantMsg != "" {
				assert.Contains(t, err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestValidateModelDeploymentRuntimeControl_AuthEnvAndRouter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		md        func() *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "vLLM user-supplied --api-key admitted alongside runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--api-key", "user-key"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "SGLang user-supplied --api-key admitted alongside runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].ExtraArgs = []string{"--api-key=user-key"}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "former v1 auth and dev env vars are not forbidden on vLLM",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
						{Name: "GPUSTACK_RUNTIME_CONTROL_TOKEN", Value: "custom"},
						{Name: "VLLM_SERVER_DEV_MODE", Value: "1"},
						{Name: "VLLM_ALLOW_RUNTIME_LORA_UPDATING", Value: "true"},
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "former SGLANG_ADMIN_API_KEY env var is not forbidden on SGLang",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
						{Name: "SGLANG_ADMIN_API_KEY", Value: "custom"},
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
		{
			name: "rejects operator-managed GPUSTACK_ENGINE_SENTINEL_ENABLE env when runtimeControl is set",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
						{Name: "GPUSTACK_ENGINE_SENTINEL_ENABLE", Value: "0"},
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
			wantField: "spec.roles[0].env[0]",
			wantMsg:   "GPUSTACK_ENGINE_SENTINEL_ENABLE",
		},
		{
			name: "vllm-router rejects --enable-igw on managed accelerated deployment",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{
						Name:      workercore.ModelDeploymentRouterVLLM,
						ExtraArgs: []string{"--enable-igw"},
					}
				})
			},
			wantField: "spec.router.extraArgs[0]",
			wantMsg:   "--enable-igw",
		},
		{
			name: "sglang-gateway rejects --admin-port when runtimeControl is set",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{
						Name:      workercore.ModelDeploymentRouterSGLang,
						ExtraArgs: []string{"--admin-port=9100"},
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
			wantField: "spec.router.extraArgs[0]",
			wantMsg:   "--admin-port",
		},
		{
			name: "sglang-gateway rejects --control-plane-port when runtimeControl is set",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineSGLang, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{
						Name:      workercore.ModelDeploymentRouterSGLang,
						ExtraArgs: []string{"--control-plane-port", "9200"},
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
				})
			},
			wantField: "spec.router.extraArgs[0]",
			wantMsg:   "--control-plane-port",
		},
		{
			name: "vllm-router rejects --host and --port",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{
						Name:      workercore.ModelDeploymentRouterVLLM,
						ExtraArgs: []string{"--host=0.0.0.0", "--port=8080"},
					}
				})
			},
			wantField: "spec.router.extraArgs[0]",
			wantMsg:   "--host",
		},
		{
			name: "llm-d-router rejects --enable-igw",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{
						Name:      workercore.ModelDeploymentRouterLLMD,
						ExtraArgs: []string{"--enable-igw"},
					}
				})
			},
			wantField: "spec.router.extraArgs[0]",
			wantMsg:   "--enable-igw",
		},
		{
			name: "vllm-router admits benign extraArgs alongside runtimeControl",
			md: func() *workercore.ModelDeployment {
				return runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
					md.Spec.Router = &workercore.ModelDeploymentRouter{
						Name:      workercore.ModelDeploymentRouterVLLM,
						ExtraArgs: []string{"--log-level=debug"},
					}
					md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
						Parameters: map[string]string{
							runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
						},
					}
				})
			},
		},
	}

	w := runtimeControlWebhook()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.ValidateCreate(context.Background(), tc.md())
			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantField)
			if tc.wantMsg != "" {
				assert.Contains(t, err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestValidateModelDeploymentRuntimeControl_ElasticFolded(t *testing.T) {
	t.Parallel()

	elasticWith := func(width int32, mutate func(*workercore.ModelDeploymentRole)) *workercore.ModelDeployment {
		return elasticMD(elasticRole(func(r *workercore.ModelDeploymentRole) {
			runtimecontrol.SetElasticWidth(r, width)
			r.Resources = cards(1)
			if mutate != nil {
				mutate(r)
			}
		}))
	}

	const elasticField = "spec.roles[0].runtimeControl.parameters[elastic.dataParallelWidth]"

	cases := []struct {
		name      string
		old       func() *workercore.ModelDeployment
		md        func() *workercore.ModelDeployment
		wantField string
		wantMsg   string
	}{
		{
			name: "vLLM NVIDIA elastic role with --elastic-ep-max-dp-size admitted",
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8"}
				})
			},
		},
		{
			name: "vLLM Ascend elastic role admitted",
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.AcceleratorType = "ascend-910b"
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8"}
				})
			},
		},
		{
			name: "vLLM AMD elastic role rejected by manufacturer check",
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.AcceleratorType = "mi300x-8x"
				})
			},
			wantField: elasticField,
			wantMsg:   "manufacturer",
		},
		{
			name: "elastic role width exceeding --elastic-ep-max-dp-size rejected",
			md: func() *workercore.ModelDeployment {
				return elasticWith(6, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=4"}
				})
			},
			wantField: elasticField,
			wantMsg:   "--elastic-ep-max-dp-size",
		},
		{
			name: "update scaling elastic.dataParallelWidth up within ceiling admitted",
			old: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8"}
				})
			},
			md: func() *workercore.ModelDeployment {
				return elasticWith(4, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8"}
				})
			},
		},
		{
			name: "update scaling elastic.dataParallelWidth down rejected",
			old: func() *workercore.ModelDeployment {
				return elasticWith(4, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8"}
				})
			},
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8"}
				})
			},
			wantField: elasticField,
			wantMsg:   "scale-down is not supported",
		},
		{
			name: "update removing elastic.dataParallelWidth rejected as immutable",
			old: func() *workercore.ModelDeployment {
				return elasticWith(2, nil)
			},
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					delete(r.RuntimeControl.Parameters, runtimecontrol.ParamElasticDataParallelWidth)
				})
			},
			wantField: elasticField,
		},
		{
			name: "update adding elastic.dataParallelWidth to non-elastic role rejected as immutable",
			old: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.RuntimeControl = nil
				})
			},
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, nil)
			},
			wantField: elasticField,
		},
		{
			name: "update changing --elastic-ep-max-dp-size rejected as immutable",
			old: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8"}
				})
			},
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=16"}
				})
			},
			wantField: "spec.roles[0].extraArgs",
			wantMsg:   "fixed at creation",
		},
		{
			name: "update editing other parameters and pause on elastic role admitted",
			old: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8", "--max-num-seqs=64"}
				})
			},
			md: func() *workercore.ModelDeployment {
				return elasticWith(2, func(r *workercore.ModelDeploymentRole) {
					r.ExtraArgs = []string{"--elastic-ep-max-dp-size=8", "--max-num-seqs=64"}
					r.Pause = true
					r.RuntimeControl.Parameters[runtimecontrol.ParamSchedulingMaxActiveRequests] = "32"
				})
			},
		},
	}

	w := runtimeControlWebhook()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.old == nil {
				_, err = w.ValidateCreate(context.Background(), tc.md())
			} else {
				_, err = w.ValidateUpdate(context.Background(), tc.old(), tc.md())
			}
			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantField)
			if tc.wantMsg != "" {
				assert.Contains(t, err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestValidateModelDeploymentRuntimeControl_UpdateAndScalerCoexistence(t *testing.T) {
	t.Parallel()

	scaler := &workercore.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-scaler", UID: "uid-1"},
	}

	buildBound := func() *workercore.ModelDeployment {
		md := boundModelDeployment()
		md.Spec.Router = &workercore.ModelDeploymentRouter{
			Name:     workercore.ModelDeploymentRouterLLMD,
			Replicas: ptr.To[int32](1),
		}
		md.Spec.Roles[0].Resources = cards(1)
		md.Spec.Roles[0].ExtraArgs = []string{"--max-num-seqs=64"}
		return md
	}

	t.Run("user may toggle pause on a scaler-bound deployment", func(t *testing.T) {
		t.Parallel()
		old := buildBound()
		md := old.DeepCopy()
		md.Spec.Roles[0].Pause = true

		w := runtimeControlWebhook(scaler)
		w.Namespace = testFenceNamespace
		w.ServiceAccount = testFenceServiceAccount
		_, err := w.ValidateUpdate(modelDeploymentFenceContext(testFenceUser), old, md)
		require.NoError(t, err)
	})

	t.Run("user may edit runtimeControl on a scaler-bound deployment", func(t *testing.T) {
		t.Parallel()
		old := buildBound()
		md := old.DeepCopy()
		md.Spec.Roles[0].Pause = true
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
			},
		}

		w := runtimeControlWebhook(scaler)
		w.Namespace = testFenceNamespace
		w.ServiceAccount = testFenceServiceAccount
		_, err := w.ValidateUpdate(modelDeploymentFenceContext(testFenceUser), old, md)
		require.NoError(t, err)
	})

	t.Run("user may edit router fields on a scaler-bound deployment", func(t *testing.T) {
		t.Parallel()
		old := buildBound()
		md := old.DeepCopy()
		md.Spec.Router.Replicas = ptr.To[int32](3)
		md.Spec.Router.RequestTimeoutSeconds = ptr.To[int32](600)
		md.Spec.Router.DisaggregationThresholdTokens = ptr.To[int32](256)

		w := runtimeControlWebhook(scaler)
		w.Namespace = testFenceNamespace
		w.ServiceAccount = testFenceServiceAccount
		_, err := w.ValidateUpdate(modelDeploymentFenceContext(testFenceUser), old, md)
		require.NoError(t, err)
	})

	t.Run("user cannot edit replicas alongside pause or runtimeControl on a scaler-bound deployment", func(t *testing.T) {
		t.Parallel()
		old := buildBound()
		md := old.DeepCopy()
		md.Spec.Roles[0].Replicas++
		md.Spec.Roles[0].Pause = true
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
			},
		}

		w := runtimeControlWebhook(scaler)
		w.Namespace = testFenceNamespace
		w.ServiceAccount = testFenceServiceAccount
		_, err := w.ValidateUpdate(modelDeploymentFenceContext(testFenceUser), old, md)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.roles")
		assert.Contains(t, err.Error(), modelDeploymentFenceRuleReplicaCount)
	})

	t.Run("user may remove runtimeControl on a scaler-bound deployment", func(t *testing.T) {
		t.Parallel()
		old := buildBound()
		old.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
			},
		}
		md := old.DeepCopy()
		md.Spec.Roles[0].RuntimeControl = nil

		w := runtimeControlWebhook(scaler)
		w.Namespace = testFenceNamespace
		w.ServiceAccount = testFenceServiceAccount
		_, err := w.ValidateUpdate(modelDeploymentFenceContext(testFenceUser), old, md)
		require.NoError(t, err)
	})

	t.Run("update rejects runtimeControl parameter exceeding extraArgs ceiling", func(t *testing.T) {
		t.Parallel()
		old := buildBound()
		md := old.DeepCopy()
		md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
			Parameters: map[string]string{
				runtimecontrol.ParamSchedulingMaxActiveRequests: "128",
			},
		}

		w := runtimeControlWebhook(scaler)
		w.Namespace = testFenceNamespace
		w.ServiceAccount = testFenceServiceAccount
		_, err := w.ValidateUpdate(modelDeploymentFenceContext(testFenceUser), old, md)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.roles[0].runtimeControl.parameters[scheduling.maxActiveRequests]")
	})

	t.Run("deletion update with missing AcceleratorType does not block finalizer removal", func(t *testing.T) {
		t.Parallel()
		old := runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].AcceleratorType = "deleted-pool"
			md.Spec.Roles[0].Pause = true
			md.Spec.Roles[0].RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{
				Parameters: map[string]string{
					runtimecontrol.ParamSchedulingMaxActiveRequests: "32",
				},
			}
		})
		md := old.DeepCopy()
		now := meta.NewTime(time.Now())
		md.DeletionTimestamp = &now

		w := runtimeControlWebhook()
		_, err := w.ValidateUpdate(context.Background(), old, md)
		require.NoError(t, err)
	})

	t.Run("pause-only and default sentinel roles reject reserved sentinel env vars", func(t *testing.T) {
		t.Parallel()
		for _, paused := range []bool{false, true} {
			md := runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Pause = paused
				md.Spec.Roles[0].RuntimeControl = nil
				md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
					{Name: "GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK", Value: "0"},
				}
			})
			w := runtimeControlWebhook()
			_, err := w.ValidateCreate(context.Background(), md)
			require.Error(t, err, "paused=%v", paused)
			assert.Contains(t, err.Error(), "GPUSTACK_ENGINE_SENTINEL_REQUIRE_ACK")
		}
	})

	t.Run("sentinel-injected role rejects additionalVolumes overlapping engine-sentinel mount path", func(t *testing.T) {
		t.Parallel()
		md := runtimeControlMD(workercore.ModelDeploymentEngineVLLM, func(md *workercore.ModelDeployment) {
			md.Spec.Model.ArtifactRef = nil
			md.Spec.Roles[0].AdditionalVolumes = []workercore.ModelDeploymentAdditionalVolume{
				{
					MountPath: "/var/lib/gpustack/operator/lib/engine-sentinel",
					ConfigMap: &core.LocalObjectReference{Name: "shadow-sentinel"},
				},
			}
		})
		w := runtimeControlWebhook()
		_, err := w.ValidateCreate(context.Background(), md)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "engine sentinel")
	})
}
