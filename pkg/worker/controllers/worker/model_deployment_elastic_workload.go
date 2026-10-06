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
	"strconv"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/elasticprofile"
)

// ModelDeploymentElasticBootWidthAnnotation records the bootstrap width on the master Pod.
const ModelDeploymentElasticBootWidthAnnotation = "worker.gpustack.ai/elastic-boot-width"

// ModelDeploymentElasticMasterBootWidthAnnotation is the master Pod's bootstrap record.
const ModelDeploymentElasticMasterBootWidthAnnotation = ModelDeploymentElasticBootWidthAnnotation

// ModelDeploymentElasticRayNodeIdentityLabel binds Ray membership to the owning Pod UID.
const ModelDeploymentElasticRayNodeIdentityLabel = "gpustack-pod-uid"

// ModelDeploymentElasticHeadCommand enables the state service required by native vLLM.
const ModelDeploymentElasticHeadCommand = "ray start --head --include-dashboard=true"

// ModelDeploymentElasticRole returns the role that requests the managed elastic profile.
func ModelDeploymentElasticRole(md *workercore.ModelDeployment) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		if md.Spec.Roles[i].ElasticEP != nil {
			return &md.Spec.Roles[i]
		}
	}
	return nil
}

// ModelDeploymentElasticBootstrapFromMaster retains a live master's captured bootstrap width.
// Missing or invalid live records hold reconciliation. A new master uses the current request.
//
// The width is held against the shared elastic profile bounds rather than numbers of its own,
// so a rendered bootstrap can never sit outside the range admission accepts or native
// observation can measure.
func ModelDeploymentElasticBootstrapFromMaster(masterUID, raw string, md *workercore.ModelDeployment) (int32, bool) {
	width := int64(0)
	if masterUID != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return 0, false
		}
		width = parsed
	} else if role := ModelDeploymentElasticRole(md); role != nil {
		width = int64(role.ElasticEP.Width)
	}
	if width < elasticprofile.WidthMin || width > elasticprofile.WidthMax {
		return 0, false
	}
	return int32(width), true
}
