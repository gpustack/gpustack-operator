package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// onManufacturer makes a serving InstanceType report a manufacturer other than the fixture's own.
func onManufacturer(manufacturer string) func(*worker.InstanceType) {
	return func(instType *worker.InstanceType) {
		instType.Status.Detail.Manufacturer = manufacturer
	}
}

// TestModelDeploymentWebhook_CrossManufacturerIsAdmitted states what admission does with a
// deployment whose two roles sit on two manufacturers: it accepts it.
//
// NO RULE COMPARES ONE ROLE'S MANUFACTURER WITH ANOTHER'S, so this is not a decision any rule makes
// -- it is what the absence of such a rule produces, and the shape therefore reaches the reconciler.
// Whether anything is then rendered is decided by the pool's transport, which admission does not
// read; that half is pinned in the controllers package.
//
// THE REFUSED ROW IS THE POSITIVE BASELINE, and it has to be one this fixture can actually reach.
// Without it, "the cross-manufacturer rows are accepted" is equally consistent with a fixture that
// never reaches the kind rules at all, and every acceptance would hold vacuously. SGLang has no
// rendering term for prefill or decode, so that pair is refused on the kind -- by a rule this same
// path runs, on this same fixture.
//
// THE REFUSAL IS CHECKED BY WHICH RULE ANSWERED, not merely that an error came back. Two
// instanceTypes also engage the barrier rule, and a case asserting only that something was refused
// would go green on that one while the kind rule stopped working.
func TestModelDeploymentWebhook_CrossManufacturerIsAdmitted(t *testing.T) {
	wholeCard := func() *workercore.ModelDeploymentRoleResources {
		return &workercore.ModelDeploymentRoleResources{
			Accelerator: resource.NewQuantity(1, resource.DecimalSI),
		}
	}

	live := []ctrlcli.Object{
		servingInstanceType("h20-8x", 8),
		servingInstanceType("ascend-910c-8x", 8,
			onManufacturer(nodefeature.ManufacturerAscend)),
	}

	cases := []struct {
		name   string
		engine string
		roles  []workercore.ModelDeploymentRole
		refuse string
	}{
		{
			name:   "pd_across_manufacturers",
			engine: workercore.ModelDeploymentEngineVLLM,
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", wholeCard()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "ascend-910c-8x", wholeCard()),
			},
		},
		{
			// The shape the field defaults produce: an unset kind is a server role, and nothing
			// refuses two of them. A user who names two instanceTypes and sets no kind is here.
			name:   "two_servers_across_manufacturers",
			engine: workercore.ModelDeploymentEngineVLLM,
			roles: []workercore.ModelDeploymentRole{
				pdRole("on-nvidia", "", "h20-8x", wholeCard()),
				pdRole("on-ascend", "", "ascend-910c-8x", wholeCard()),
			},
		},
		{
			name:   "sglang_pd_is_refused_on_the_kind",
			engine: workercore.ModelDeploymentEngineSGLang,
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", wholeCard()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "ascend-910c-8x", wholeCard()),
			},
			refuse: "has no rendering term for kind",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Two instanceTypes are what every row here declares, and the barrier rule refuses that
			// unless it can be installed. That rule has its own test; here it must not be what
			// answers, or these rows would be asserting it by accident.
			withDerivedFromNode(t, true)

			md := modelDeployment(tc.engine, tc.roles...)

			_, err := newModelDeploymentWebhookWith(live).ValidateCreate(context.Background(), md)
			if tc.refuse == "" {
				assert.NoError(t, err,
					"nothing compares the manufacturers of two roles, so the pair is admitted")

				return
			}

			require.Error(t, err)
			assert.True(t, errsContain(err.Error(), tc.refuse),
				"the refusal has to come from the kind rule, and this one reads: %v", err)
		})
	}
}
