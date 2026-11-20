// Copyright 2026 GPUStack Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package worker

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/runtimecontrol"
)

// modelDeploymentElasticOwnedFlags are the engine flags the elastic renderer writes itself.
// An entry on the role's own argv would race the operator for one argument, so it is refused
// rather than merged. The list is narrow by design: unrelated engine tuning stays the
// author's, and unrelated Ray_* tuning keys are not banned.
var modelDeploymentElasticOwnedFlags = sets.New(
	"--data-parallel-size",
	"--data-parallel-backend",
	"--data-parallel-address",
	"--enable-expert-parallel",
	"--data-parallel-size-local",
	"--enable-elastic-ep",
	"--enable-eplb",
	"--distributed-executor-backend",
	"--ray-address",
)

// modelDeploymentElasticOwnedEnv are the environment entries the elastic renderer writes
// itself, refused on the same terms as the flags.
var modelDeploymentElasticOwnedEnv = sets.New(
	"VLLM_DP_SIZE",
	"VLLM_DATA_PARALLEL_SIZE_LOCAL",
	"VLLM_ENABLE_ELASTIC_EP",
	"VLLM_ENABLE_EPLB",
	"VLLM_TENSOR_PARALLEL_SIZE",
	"VLLM_PIPELINE_PARALLEL_SIZE",
	"VLLM_RAY_ADDRESS",
	"RAY_ADDRESS",
	"GPUSTACK_ELASTIC_POD_UID",
	"GPUSTACK_ELASTIC_POD_IP",
	"VLLM_RAY_DP_PACK_STRATEGY",
	"RAY_OVERRIDE_LABELS",
)

// modelDeploymentElasticWidthPath returns the field path for elastic.dataParallelWidth on rolePath.
func modelDeploymentElasticWidthPath(rolePath *field.Path) *field.Path {
	return rolePath.Child("runtimeControl", "parameters").Key(runtimecontrol.ParamElasticDataParallelWidth)
}

// ValidateModelDeploymentElasticEP validates the elastic-EP profile of a deployment against
// its previous state. A nil old is a create. The check runs on every create and update; the
// immutability half only fires when the profile existed before.
func (r *ModelDeploymentWebhook) ValidateModelDeploymentElasticEP(
	_ context.Context, md, old *workercore.ModelDeployment,
) field.ErrorList {
	var errs field.ErrorList

	elasticRoles := modelDeploymentElasticRoles(md)
	if len(elasticRoles) == 0 {
		if old != nil {
			if oldElastic := elasticRoleOf(old); oldElastic != nil {
				rolePath := field.NewPath("spec", "roles")
				for i := range md.Spec.Roles {
					if md.Spec.Roles[i].Name == oldElastic.Name {
						rolePath = rolePath.Index(i)
						break
					}
				}
				errs = append(errs, field.Forbidden(
					modelDeploymentElasticWidthPath(rolePath), modelDeploymentIdentityMessage,
				))
			}
		}
		return errs
	}

	errs = append(errs, validateModelDeploymentElasticShape(md, elasticRoles)...)
	if old != nil {
		errs = append(errs, validateModelDeploymentElasticIdentity(md, old)...)
	}

	return errs
}

// elasticRoleOf returns the single elastic role of the deployment, or nil.
func elasticRoleOf(md *workercore.ModelDeployment) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		if runtimecontrol.RoleHasElasticEP(&md.Spec.Roles[i]) {
			return &md.Spec.Roles[i]
		}
	}
	return nil
}

func modelDeploymentElasticRoles(md *workercore.ModelDeployment) []*workercore.ModelDeploymentRole {
	var roles []*workercore.ModelDeploymentRole
	for i := range md.Spec.Roles {
		if runtimecontrol.RoleHasElasticEP(&md.Spec.Roles[i]) {
			roles = append(roles, &md.Spec.Roles[i])
		}
	}
	return roles
}

// modelDeploymentElasticRolePath returns the list path of the elastic role.
func modelDeploymentElasticRolePath(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) *field.Path {
	for i := range md.Spec.Roles {
		if &md.Spec.Roles[i] == role {
			return field.NewPath("spec", "roles").Index(i)
		}
	}
	return field.NewPath("spec", "roles")
}

// validateModelDeploymentElasticShape enforces the profile's shape rules on the new object:
// one elastic role on the vLLM engine, a single managed Server instance, whole non-sliced
// accelerator per member, no argv take-over, no renderer-owned native flag or env entry.
func validateModelDeploymentElasticShape(
	md *workercore.ModelDeployment, elasticRoles []*workercore.ModelDeploymentRole,
) field.ErrorList {
	var errs field.ErrorList
	rolesPath := field.NewPath("spec", "roles")

	if md.Spec.Engine.Name != workercore.ModelDeploymentEngineVLLM {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "engine", "name"), md.Spec.Engine.Name,
			"the elastic-EP profile runs the vLLM engine; no other engine carries the elastic contract this profile renders",
		))
	}
	if len(elasticRoles) > 1 {
		errs = append(errs, field.Forbidden(
			rolesPath,
			fmt.Sprintf(
				"at most one role may carry runtimeControl.parameters[%q]: the profile is one managed vLLM Server role",
				runtimecontrol.ParamElasticDataParallelWidth,
			),
		))
	}
	if len(elasticRoles) > 0 && len(md.Spec.Roles) > 1 {
		errs = append(errs, field.Forbidden(
			rolesPath,
			fmt.Sprintf(
				"runtimeControl.parameters[%q] is the deployment's ONLY role: one managed vLLM Server role, "+
					"whose width -- not a second role -- sizes the collective",
				runtimecontrol.ParamElasticDataParallelWidth,
			),
		))
	}

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if !runtimecontrol.RoleHasElasticEP(role) {
			continue
		}
		rolePath := rolesPath.Index(i)
		widthPath := modelDeploymentElasticWidthPath(rolePath)
		rawWidth := role.RuntimeControl.Parameters[runtimecontrol.ParamElasticDataParallelWidth]

		if role.AcceleratorType == "" {
			errs = append(errs, field.Required(rolePath.Child("acceleratorType"), "the elastic-EP profile requires an acceleratorType"))
		}

		width, _, widthErr := runtimecontrol.ParseElasticWidth(role)
		if widthErr != nil {
			errs = append(errs, field.Invalid(widthPath, rawWidth, widthErr.Error()))
		}

		declared, err := workerctrl.ParseModelDeploymentDeclaredParallelism(md.Spec.Engine.Name, role.ExtraArgs, role.Env)
		if err != nil {
			errs = append(errs, field.Invalid(rolePath.Child("extraArgs"), role.ExtraArgs, err.Error()))
		} else {
			maxDP := int32(declared.ElasticEPMaxDataParallel)
			if maxDP == 0 {
				if ceilings := runtimecontrol.ParseExtraArgsBootCeilings(md.Spec.Engine.Name, role.ExtraArgs); ceilings.ElasticMaxDataParallel != nil {
					maxDP = *ceilings.ElasticMaxDataParallel
				}
			}
			if widthErr == nil && maxDP > 0 && width > maxDP {
				errs = append(errs, field.Invalid(
					widthPath, rawWidth,
					fmt.Sprintf("must not exceed the startup --elastic-ep-max-dp-size (%d)", maxDP),
				))
			}
			if declared.PipelineParallel != 1 || declared.PrefillContextParallel != 1 {
				errs = append(errs, field.Invalid(rolePath.Child("extraArgs"), role.ExtraArgs,
					"each elastic member holds one TP group; pipeline and prefill context parallel sizes must be one"))
			}
			errs = append(errs, validateModelDeploymentElasticResources(role, rolePath, declared.TensorParallel)...)
		}
		if role.Kind != workercore.ModelDeploymentRoleKindServer {
			errs = append(errs, field.Invalid(
				rolePath.Child("kind"), role.Kind,
				"the elastic-EP profile is one managed vLLM Server role; prefill/decode kinds do not compose with it",
			))
		}
		if role.Replicas != 1 {
			errs = append(errs, field.Invalid(
				rolePath.Child("replicas"), role.Replicas,
				"the elastic-EP profile is one serving instance; width, not replicas, sizes the collective",
			))
		}
		if role.ReplicaSize != 1 {
			errs = append(errs, field.Invalid(
				rolePath.Child("size"), role.ReplicaSize,
				"each elastic member is exactly one Pod; the member count is width, not size",
			))
		}
		if len(role.Command) > 0 {
			errs = append(errs, field.Forbidden(
				rolePath.Child("command"),
				"the elastic renderer owns the argv of the managed role; a taken-over command has no elastic rendering",
			))
		}
		errs = append(errs, validateModelDeploymentElasticOwnedKeys(role, rolePath)...)
	}

	return errs
}

// validateModelDeploymentElasticResources requires the complete fixed TP group per member.
// Every accelerator is whole, unsliced and unpartitioned.
func validateModelDeploymentElasticResources(
	role *workercore.ModelDeploymentRole, rolePath *field.Path, tp int,
) field.ErrorList {
	var errs field.ErrorList
	res := role.Resources
	if res == nil {
		return errs
	}
	resPath := rolePath.Child("resources")

	if res.Accelerator != nil && res.Accelerator.CmpInt64(int64(tp)) != 0 {
		errs = append(errs, field.Invalid(
			resPath.Child("accelerator"), res.Accelerator.String(),
			fmt.Sprintf("each elastic member takes exactly %d whole accelerators per Pod", tp),
		))
	}
	if res.AcceleratorSlicedMemoryPercentage != 0 {
		errs = append(errs, field.Forbidden(
			resPath.Child("acceleratorSlicedMemoryPercentage"),
			"the elastic profile takes whole, non-sliced accelerators",
		))
	}
	if res.AcceleratorSlicedCoresPercentage != 0 {
		errs = append(errs, field.Forbidden(
			resPath.Child("acceleratorSlicedCoresPercentage"),
			"the elastic profile takes whole, non-sliced accelerators",
		))
	}
	if res.AcceleratorPartitionedProfile != "" {
		errs = append(errs, field.Forbidden(
			resPath.Child("acceleratorPartitionedProfile"),
			"the elastic profile takes whole, non-partitioned accelerators",
		))
	}

	return errs
}

// validateModelDeploymentElasticOwnedKeys refuses the renderer-owned native flags and env
// entries. The match is narrow: an entry the operator itself writes is a conflict, unrelated
// tuning keys are the author's.
func validateModelDeploymentElasticOwnedKeys(
	role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	for _, arg := range role.ExtraArgs {
		name := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
		switch name {
		case "dp":
			name = "data-parallel-size"
		case "dpl":
			name = "data-parallel-size-local"
		case "dpb":
			name = "data-parallel-backend"
		case "dpa":
			name = "data-parallel-address"
		case "tp":
			name = "tensor-parallel-size"
		case "pp":
			name = "pipeline-parallel-size"
		}
		flag := "--" + name
		if modelDeploymentElasticOwnedFlags.Has(flag) {
			errs = append(errs, field.Forbidden(
				rolePath.Child("extraArgs"), fmt.Sprintf("%s is written by the elastic renderer itself", flag),
			))
		}
	}
	for i := range role.Env {
		if modelDeploymentElasticOwnedEnv.Has(role.Env[i].Name) {
			errs = append(errs, field.Forbidden(
				rolePath.Child("env").Index(i).Child("name"),
				fmt.Sprintf("%s is written by the elastic renderer itself", role.Env[i].Name),
			))
		}
	}

	return errs
}

// validateModelDeploymentElasticIdentity enforces the frozen half of the profile: presence,
// the role's pool and card shape, and the declared TP and width ceiling are identity;
// elastic.dataParallelWidth is the running state. The profile must also stay on the role that carried it.
func validateModelDeploymentElasticIdentity(
	md, old *workercore.ModelDeployment,
) field.ErrorList {
	var errs field.ErrorList

	oldElastic := elasticRoleOf(old)
	newElastic := elasticRoleOf(md)
	if oldElastic == nil || newElastic == nil {
		rolePath := field.NewPath("spec", "roles")
		if newElastic != nil {
			rolePath = modelDeploymentElasticRolePath(md, newElastic)
		}
		errs = append(errs, field.Forbidden(
			modelDeploymentElasticWidthPath(rolePath), modelDeploymentIdentityMessage,
		))
		return errs
	}

	newRolePath := modelDeploymentElasticRolePath(md, newElastic)
	oldParallel, oldErr := workerctrl.ParseModelDeploymentDeclaredParallelism(old.Spec.Engine.Name, oldElastic.ExtraArgs, oldElastic.Env)
	newParallel, newErr := workerctrl.ParseModelDeploymentDeclaredParallelism(md.Spec.Engine.Name, newElastic.ExtraArgs, newElastic.Env)
	if oldErr != nil || newErr != nil || oldParallel.TensorParallel != newParallel.TensorParallel {
		errs = append(errs, field.Invalid(
			newRolePath.Child("extraArgs"),
			newElastic.ExtraArgs, "tensor parallel size is fixed at creation; "+modelDeploymentIdentityMessage,
		))
	}
	if oldErr == nil && newErr == nil && oldParallel.ElasticEPMaxDataParallel != newParallel.ElasticEPMaxDataParallel {
		errs = append(errs, field.Forbidden(
			newRolePath.Child("extraArgs"),
			"--elastic-ep-max-dp-size is fixed at creation; "+modelDeploymentIdentityMessage,
		))
	}
	if oldElastic.Name != newElastic.Name {
		errs = append(errs, field.Forbidden(
			newRolePath.Child("name"),
			"the elastic profile must stay on the role it was created with: "+modelDeploymentIdentityMessage,
		))
	}
	// The profile pins what the ordinary contract lets move: an elastic role's member shape is
	// fixed at creation, and width is the only running-state field. ReplicaSize stays at one and
	// a command stays forbidden through the shape rules.
	if newElastic.AcceleratorType != oldElastic.AcceleratorType {
		errs = append(errs, field.Invalid(
			newRolePath.Child("acceleratorType"),
			newElastic.AcceleratorType,
			"the elastic-EP profile pins the pool the members were admitted against: "+modelDeploymentIdentityMessage,
		))
	}
	if !kubemeta.DeepEqual(newElastic.Resources, oldElastic.Resources) {
		errs = append(errs, field.Invalid(
			newRolePath.Child("resources"),
			newElastic.Resources,
			"the elastic-EP profile pins the members' accelerator shape: "+modelDeploymentIdentityMessage,
		))
	}

	oldWidth, oldOK, oldWidthErr := runtimecontrol.ParseElasticWidth(oldElastic)
	newWidth, newOK, newWidthErr := runtimecontrol.ParseElasticWidth(newElastic)
	if oldOK && newOK && oldWidthErr == nil && newWidthErr == nil && newWidth < oldWidth {
		errs = append(errs, field.Forbidden(
			modelDeploymentElasticWidthPath(newRolePath),
			"elastic-EP scale-down is not supported in this release; keep width unchanged or increase it",
		))
	}

	return errs
}
