package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extension "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestModelDeploymentForgerCRDSchemaBoundsAndDefaults(t *testing.T) {
	crd := GetCustomResourceDefinitions()["ModelDeploymentForger"]
	require.NotNil(t, crd, "ModelDeploymentForger CRD must be registered")
	assert.Equal(t, extension.NamespaceScoped, crd.Spec.Scope)
	assert.Contains(t, crd.Spec.Names.ShortNames, "mdf")
	assert.Contains(t, crd.Spec.Names.Categories, "gpustack")
	require.Len(t, crd.Spec.Versions, 1)
	require.NotNil(t, crd.Spec.Versions[0].Subresources)
	assert.NotNil(t, crd.Spec.Versions[0].Subresources.Status, "v1alpha1 must expose status subresource")

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]
	assert.ElementsMatch(t, []string{"targetRef", "roles"}, spec.Required)
	assert.ElementsMatch(t, []string{`"ObserveOnly"`, `"Shadow"`, `"Active"`}, enumValues(t, spec, "mode"))
	require.NotNil(t, spec.Properties["mode"].Default)
	assert.Equal(t, `"Shadow"`, string(spec.Properties["mode"].Default.Raw))

	role := spec.Properties["roles"].Items.Schema
	require.NotNil(t, role.Properties["maxKernelsPerGeneration"].Minimum)
	assert.InDelta(t, 1, *role.Properties["maxKernelsPerGeneration"].Minimum, 0)
	require.NotNil(t, role.Properties["maxKernelsPerGeneration"].Maximum)
	assert.InDelta(t, 16, *role.Properties["maxKernelsPerGeneration"].Maximum, 0)

	probe := spec.Properties["probe"]
	require.NotNil(t, probe.Default)
	assert.Equal(t, `{}`, string(probe.Default.Raw))
	assert.Equal(t, `4`, string(probe.Properties["steps"].Default.Raw))
	assert.InDelta(t, 3, *probe.Properties["steps"].Minimum, 0)
	assert.InDelta(t, 5, *probe.Properties["steps"].Maximum, 0)
	assert.Equal(t, `600`, string(probe.Properties["cooldownSeconds"].Default.Raw))
	assert.InDelta(t, 60, *probe.Properties["cooldownSeconds"].Minimum, 0)
	require.NotNil(t, probe.Properties["minActiveRequests"].Default)
	assert.Equal(t, `1`, string(probe.Properties["minActiveRequests"].Default.Raw))
	require.NotNil(t, probe.Properties["minActiveRequests"].Minimum)
	assert.InDelta(t, 0, *probe.Properties["minActiveRequests"].Minimum, 0)

	sandbox := spec.Properties["sandbox"]
	require.NotNil(t, sandbox.Default)
	assert.Equal(t, `{}`, string(sandbox.Default.Raw))
	assert.ElementsMatch(t, []string{`"External"`, `"Mock"`}, enumValues(t, sandbox, "runnerMode"))
	assert.Equal(t, `"External"`, string(sandbox.Properties["runnerMode"].Default.Raw))
	assert.Equal(t, `1800`, string(sandbox.Properties["activeDeadlineSeconds"].Default.Raw))
	assert.InDelta(t, 60, *sandbox.Properties["activeDeadlineSeconds"].Minimum, 0)
	assert.InDelta(t, 14400, *sandbox.Properties["activeDeadlineSeconds"].Maximum, 0)
	assert.Equal(t, `"2Gi"`, string(sandbox.Properties["outputSizeLimit"].Default.Raw))
	assert.Contains(t, sandbox.Properties, "imagePullSecrets")

	runner := sandbox.Properties["runner"]
	for _, fieldName := range []string{"image", "imagePullPolicy", "command", "args", "env", "envFrom"} {
		assert.Contains(t, runner.Properties, fieldName)
	}
	res := sandbox.Properties["resources"]
	for _, fieldName := range []string{
		"cpu", "ram", "localStorage",
		"acceleratorSlicedMemoryPercentage", "acceleratorSlicedCoresPercentage", "acceleratorPartitionedProfile",
	} {
		assert.Contains(t, res.Properties, fieldName)
	}

	rollout := spec.Properties["rollout"]
	require.NotNil(t, rollout.Default)
	assert.Equal(t, `{}`, string(rollout.Default.Raw))
	assert.Equal(t, `3`, string(rollout.Properties["maxRetainedGenerations"].Default.Raw))
	assert.InDelta(t, 1, *rollout.Properties["maxRetainedGenerations"].Minimum, 0)
	assert.InDelta(t, 16, *rollout.Properties["maxRetainedGenerations"].Maximum, 0)
	assert.Equal(t, `600`, string(rollout.Properties["failureTimeoutSeconds"].Default.Raw))

	status := root.Properties["status"]
	for _, fieldName := range []string{"observedGeneration", "binding", "roles", "conditions"} {
		assert.Contains(t, status.Properties, fieldName)
	}
	roleStatus := status.Properties["roles"].Items.Schema
	assert.ElementsMatch(t, []string{
		`"Idle"`, `"Probing"`, `"Forging"`, `"Verifying"`, `"Publishing"`, `"Promoting"`,
	}, enumValues(t, *roleStatus, "phase"))
	require.NotNil(t, roleStatus.Properties["generations"].MaxItems)
	assert.Equal(t, int64(16), *roleStatus.Properties["generations"].MaxItems)
	genStatus := roleStatus.Properties["generations"].Items.Schema
	assert.ElementsMatch(t, []string{
		`"Promoted"`, `"RolledBack"`, `"Failed"`, `"Skipped"`,
	}, enumValues(t, *genStatus, "outcome"))

	assert.Equal(t, "modeldeployment.gpustack.ai/forger-binding", ModelDeploymentForgerBindingAnnotationKey)
	assert.Equal(t, "modeldeployment.gpustack.ai/forger-decision", ModelDeploymentForgerDecisionAnnotationKey)
	assert.Equal(t, "KernelsReady", ModelDeploymentConditionTypeKernelsReady)
}
