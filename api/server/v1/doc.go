// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package v1 is the versioned API for server.gpustack.ai.
//
// +k8s:openapi-gen=true
// +k8s:openapi-model-package=ai.gpustack.server.v1
// +k8s:deepcopy-gen=package
// +k8s:protobuf-gen=package
// +k8s:apireg-gen:service
// +k8s:conversion-gen=gpustack.ai/gpustack/api/server/v1alpha1
// +k8s:conversion-gen-external-types=gpustack.ai/gpustack/api/server/v1
// +domain=gpustack.ai
// +groupName=server.gpustack.ai
// +versionName=v1
package v1
