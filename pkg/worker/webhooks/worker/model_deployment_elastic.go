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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/elasticprofile"
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
	"--tensor-parallel-size",
	"--pipeline-parallel-size",
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

// ValidateModelDeploymentElasticEP validates the elastic-EP profile of a deployment against
// its previous state. A nil old is a create. The check runs on every create and update; the
// immutability half only fires when the profile existed before.
func (r *ModelDeploymentWebhook) ValidateModelDeploymentElasticEP(
	ctx context.Context, md, old *workercore.ModelDeployment,
) field.ErrorList {
	elasticRoles := modelDeploymentElasticRoles(md)
	if len(elasticRoles) == 0 {
		if old != nil && elasticRoleOf(old) != nil {
			return field.ErrorList{field.Forbidden(
				field.NewPath("spec", "roles").Child("elasticEp"), modelDeploymentIdentityMessage,
			)}
		}
		return nil
	}

	errs := validateModelDeploymentElasticShape(md, elasticRoles)
	if old != nil {
		errs = append(errs, validateModelDeploymentElasticIdentity(md, old)...)
	}
	errs = append(errs, r.validateModelDeploymentElasticHeadInstanceType(ctx, md)...)

	return errs
}

// elasticRoleOf returns the single elastic role of the deployment, or nil.
func elasticRoleOf(md *workercore.ModelDeployment) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].ElasticEP != nil {
			return &md.Spec.Roles[i]
		}
	}
	return nil
}

func modelDeploymentElasticRoles(md *workercore.ModelDeployment) []*workercore.ModelDeploymentRole {
	var roles []*workercore.ModelDeploymentRole
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].ElasticEP != nil {
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
			rolesPath, "at most one role may carry spec.roles[].elasticEp: the profile is one managed vLLM Server role",
		))
	}
	if len(elasticRoles) > 0 && len(md.Spec.Roles) > 1 {
		errs = append(errs, field.Forbidden(
			rolesPath, "spec.roles[].elasticEp is the deployment's ONLY role: one managed vLLM Server role, "+
				"whose width -- not a second role -- sizes the collective",
		))
	}

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.ElasticEP == nil {
			continue
		}
		rolePath := rolesPath.Index(i)

		if role.ElasticEP.Width < elasticprofile.WidthMin || role.ElasticEP.Width > elasticprofile.WidthMax {
			errs = append(errs, field.Invalid(
				rolePath.Child("elasticEp", "width"), role.ElasticEP.Width,
				fmt.Sprintf("total GPU engines including the reserved master must be between %d and %d",
					elasticprofile.WidthMin, elasticprofile.WidthMax),
			))
		}
		tp := elasticprofile.TensorParallelSize(role.ElasticEP)
		if tp != 1 && tp != 2 {
			errs = append(errs, field.Invalid(rolePath.Child("elasticEp", "tensorParallelSize"), tp,
				"tensor parallel size must be one or two"))
		}
		if role.ElasticEP.HeadInstanceType == "" {
			errs = append(errs, field.Required(
				rolePath.Child("elasticEp", "headInstanceType"),
				"the Ray control-plane head needs a CPU-only InstanceType to run against",
			))
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
		errs = append(errs, validateModelDeploymentElasticResources(role, rolePath)...)
		errs = append(errs, validateModelDeploymentElasticOwnedKeys(role, rolePath)...)
	}

	return errs
}

// validateModelDeploymentElasticResources pins the member accelerator request to one whole,
// non-sliced, non-partitioned accelerator with no fabric interface.
func validateModelDeploymentElasticResources(
	role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	res := role.Resources
	if res == nil {
		return errs
	}
	resPath := rolePath.Child("resources")

	if res.Accelerator != nil && res.Accelerator.CmpInt64(int64(elasticprofile.TensorParallelSize(role.ElasticEP))) != 0 {
		errs = append(errs, field.Invalid(
			resPath.Child("accelerator"), res.Accelerator.String(),
			fmt.Sprintf("each elastic member takes exactly %d whole accelerators per Pod", elasticprofile.TensorParallelSize(role.ElasticEP)),
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
	if res.Interface != nil && res.Interface.CmpInt64(0) != 0 {
		errs = append(errs, field.Forbidden(
			resPath.Child("interface"),
			"the initial elastic profile requests no fabric interface",
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

// validateModelDeploymentElasticIdentity enforces the frozen half of the profile: presence
// and head placement are identity, width is the running state. The profile must also stay on
// the role that carried it.
func validateModelDeploymentElasticIdentity(
	md, old *workercore.ModelDeployment,
) field.ErrorList {
	var errs field.ErrorList

	oldElastic := elasticRoleOf(old)
	newElastic := elasticRoleOf(md)
	if oldElastic == nil || newElastic == nil {
		errs = append(errs, field.Forbidden(
			field.NewPath("spec", "roles").Child("elasticEp"), modelDeploymentIdentityMessage,
		))
		return errs
	}

	if oldElastic.ElasticEP.HeadInstanceType != newElastic.ElasticEP.HeadInstanceType {
		errs = append(errs, field.Invalid(
			modelDeploymentElasticRolePath(md, newElastic).Child("elasticEp", "headInstanceType"),
			newElastic.ElasticEP.HeadInstanceType, modelDeploymentIdentityMessage,
		))
	}
	if elasticprofile.TensorParallelSize(oldElastic.ElasticEP) != elasticprofile.TensorParallelSize(newElastic.ElasticEP) {
		errs = append(errs, field.Invalid(
			modelDeploymentElasticRolePath(md, newElastic).Child("elasticEp", "tensorParallelSize"),
			elasticprofile.TensorParallelSize(newElastic.ElasticEP), modelDeploymentIdentityMessage,
		))
	}
	if oldElastic.Name != newElastic.Name {
		errs = append(errs, field.Forbidden(
			modelDeploymentElasticRolePath(md, newElastic).Child("name"),
			"the elastic profile must stay on the role it was created with: "+modelDeploymentIdentityMessage,
		))
	}
	if newElastic.ElasticEP.Width < oldElastic.ElasticEP.Width {
		errs = append(errs, field.Forbidden(
			modelDeploymentElasticRolePath(md, newElastic).Child("elasticEp", "width"),
			"elastic-EP scale-down is not supported in this release; keep width unchanged or increase it",
		))
	}

	return errs
}

// validateModelDeploymentElasticHeadInstanceType resolves each named head InstanceType and
// refuses anything acceleratable: the head is CPU-only by construction.
func (r *ModelDeploymentWebhook) validateModelDeploymentElasticHeadInstanceType(
	ctx context.Context, md *workercore.ModelDeployment,
) field.ErrorList {
	var errs field.ErrorList

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.ElasticEP == nil || role.ElasticEP.HeadInstanceType == "" {
			continue
		}
		headPath := field.NewPath("spec", "roles").Index(i).Child("elasticEp", "headInstanceType")
		instType := &workercore.InstanceType{}
		instType.Name = role.ElasticEP.HeadInstanceType
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType); err != nil {
			if apierrors.IsNotFound(err) {
				errs = append(errs, field.NotFound(headPath, role.ElasticEP.HeadInstanceType))
			} else {
				errs = append(errs, field.InternalError(headPath, fmt.Errorf("get instance type: %w", err)))
			}
			continue
		}
		if instType.Spec.Acceleratable {
			errs = append(errs, field.Forbidden(
				headPath,
				"the Ray control-plane head runs CPU-only; an acceleratable InstanceType would put it on a GPU queue",
			))
		}
	}

	return errs
}
