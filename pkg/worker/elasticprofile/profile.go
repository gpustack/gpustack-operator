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

// Package elasticprofile owns the supported profile of the elastic collective: the one width
// range the operator renders, admits and observes.
//
// THE BOUNDS ARE DECLARED HERE, ONCE, and every consumer reads them from this package instead
// of carrying a copy of the numbers. A second copy is a second answer, and the two answers
// drift apart in the direction nobody reads: widening admission alone leaves native observation
// refusing a width the deployment is already allowed to ask for, and narrowing rendering alone
// leaves a width that nothing refuses. A consumer that needs a different bound does not restate
// it; it states why it is bound to something else, as the native observer does.
//
// THE PROFILE ITSELF IS NOT WIDENED HERE. These are the bounds the managed elastic-EP profile
// has always supported, and declaring them in one place is a statement about what they are,
// not an opening of the range. The API type's own kubebuilder markers carry the same pair for
// the API server's schema validation; admission below it is what enforces them for this
// operator.
package elasticprofile

import workercore "gpustack.ai/gpustack/api/worker/v1alpha1"

const (
	// WidthMin is the narrowest elastic collective: the engine world plus the one reserved
	// API/DP-master member the profile always holds back.
	WidthMin = 2

	// WidthMax is the widest elastic collective the operator supports, engine ranks included.
	// It bounds one native observation for the same reason it bounds the profile: an untrusted
	// engine answer must never decide how much probe traffic the operator generates.
	WidthMax = 64
)

// TensorParallelSize reads the effective immutable GPU group size.
// Legacy and non-elastic roles use one.
func TensorParallelSize(profile *workercore.ModelDeploymentRoleElasticEP) int32 {
	if profile == nil || profile.TensorParallelSize == nil {
		return 1
	}
	return *profile.TensorParallelSize
}
