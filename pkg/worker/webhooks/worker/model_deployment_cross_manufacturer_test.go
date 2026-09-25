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
// never reaches the kind rules at all, and every acceptance would hold vacuously. It is a duplicate
// kind, refused by a rule this same path runs on this same fixture. It used to be SGLang's
// prefill/decode pair, which the kind rules no longer refuse.
//
// THE REFUSAL IS CHECKED BY WHICH RULE ANSWERED, not merely that an error came back. A case
// asserting only that something was refused would go green on any other rule while the kind rule
// stopped working.
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
			// SGLang renders the split as its own disaggregation arguments, so this pair is
			// admitted where it used to be refused. The row is kept as the witness of that.
			name:   "sglang_pd_is_admitted_on_the_kind",
			engine: workercore.ModelDeploymentEngineSGLang,
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", wholeCard()),
				pdRole("decode", workercore.ModelDeploymentRoleKindDecode, "ascend-910c-8x", wholeCard()),
			},
		},
		{
			// THE POSITIVE BASELINE, which the row above used to be. It has to be a shape this
			// fixture reaches through the same kind rules, and no engine is refused on a kind any
			// more, so the refusal is the one a duplicate kind gets instead. Without a row that is
			// actually refused, every acceptance above holds vacuously.
			name:   "a_second_prefill_is_refused_on_the_kind",
			engine: workercore.ModelDeploymentEngineVLLM,
			roles: []workercore.ModelDeploymentRole{
				pdRole("prefill", workercore.ModelDeploymentRoleKindPrefill, "h20-8x", wholeCard()),
				pdRole("prefill-again", workercore.ModelDeploymentRoleKindPrefill,
					"ascend-910c-8x", wholeCard()),
			},
			refuse: "is already declared by role",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
