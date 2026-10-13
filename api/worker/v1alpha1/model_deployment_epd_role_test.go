package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
)

// TestModelDeploymentRoleKindEncodeSpelling pins the public spelling of the new kind constant.
func TestModelDeploymentRoleKindEncodeSpelling(t *testing.T) {
	assert.Equal(t, "Encode", string(ModelDeploymentRoleKindEncode))
}

// TestModelDeploymentAdditionalVolumePersistentVolumeClaimDeepCopyDoesNotAlias pins the new
// volume source: a DeepCopy of the entry does not share the backing claim name with the source.
func TestModelDeploymentAdditionalVolumePersistentVolumeClaimDeepCopyDoesNotAlias(t *testing.T) {
	src := &core.PersistentVolumeClaimVolumeSource{
		ClaimName: "shared-cache",
		ReadOnly:  false,
	}
	original := &ModelDeploymentAdditionalVolume{
		MountPath:             "/shared/cache",
		ReadOnly:              false,
		PersistentVolumeClaim: src,
	}

	got := original.DeepCopy()
	require.NotNil(t, got, "DeepCopy of an entry with a PVC source returns the entry")
	require.NotNil(t, got.PersistentVolumeClaim, "the PVC source survives DeepCopy")
	assert.Equal(t, "shared-cache", got.PersistentVolumeClaim.ClaimName)
	assert.False(t, got.PersistentVolumeClaim.ReadOnly)

	got.PersistentVolumeClaim.ClaimName = "mutated"
	assert.Equal(t, "shared-cache", original.PersistentVolumeClaim.ClaimName,
		"the copy does not alias the source")
}
