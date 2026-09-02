package mooncake

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// TestQuotaPolicy drives BOTH entry points from one table on every row: the validator the webhook
// calls and the renderer the reconciler calls. That is the spec's webhook_and_reconciler_same_verdict
// case, asserted for every input rather than once — a verdict the two disagree on is a file the
// webhook admitted and the master aborts on.
//
// Every refusal row asserts that nothing at all came back, because a file with the bad entry dropped
// is a quota nobody set.
func TestQuotaPolicy(t *testing.T) {
	cases := []struct {
		name    string
		tenants []QuotaPolicyTenant
		// want is the whole file, asserted exactly: a renderer is only diffable if its output is.
		// It is meaningful only on the rows that are accepted.
		want string
		// wantFields are the field paths the refusal must name, in order. Empty means accepted.
		wantFields []string
	}{
		{
			name: "renders_version_unconditionally",
			tenants: []QuotaPolicyTenant{
				{Name: "team-a", Quota: resource.MustParse("1Gi")},
			},
			want: "version: 1\ntenants:\n    - name: team-a\n      quota: 1073741824\n",
		},
		{
			name: "renders_quota_in_bytes",
			tenants: []QuotaPolicyTenant{
				{Name: "team-a", Quota: resource.MustParse("1G")},
			},
			want: "version: 1\ntenants:\n    - name: team-a\n      quota: 1000000000\n",
		},
		{
			name: "empty_tenant_set",
			want: "version: 1\ntenants: []\n",
		},
		{
			name: "quota_overflows_int64",
			tenants: []QuotaPolicyTenant{
				{Name: "team-a", Quota: resource.MustParse("9223372036854775808")},
				{Name: "team-b", Quota: resource.MustParse("1e30")},
			},
			wantFields: []string{"tenants[0].quota", "tenants[1].quota"},
		},
		{
			name: "name_empty",
			tenants: []QuotaPolicyTenant{
				{Name: "", Quota: resource.MustParse("1Gi")},
			},
			wantFields: []string{"tenants[0].name"},
		},
		{
			name: "name_leading_underscore",
			tenants: []QuotaPolicyTenant{
				{Name: "_bad", Quota: resource.MustParse("1Gi")},
			},
			wantFields: []string{"tenants[0].name"},
		},
		{
			name: "name_duplicate",
			tenants: []QuotaPolicyTenant{
				{Name: "team-a", Quota: resource.MustParse("1Gi")},
				{Name: "team-a", Quota: resource.MustParse("2Gi")},
			},
			wantFields: []string{"tenants[1].name"},
		},
		{
			name: "name_control_character",
			tenants: []QuotaPolicyTenant{
				{Name: "team\x01a", Quota: resource.MustParse("1Gi")},
			},
			wantFields: []string{"tenants[0].name"},
		},
		{
			name: "name_nul",
			tenants: []QuotaPolicyTenant{
				{Name: "team\x00a", Quota: resource.MustParse("1Gi")},
			},
			wantFields: []string{"tenants[0].name"},
		},
		{
			name: "quota_zero",
			tenants: []QuotaPolicyTenant{
				{Name: "team-a", Quota: resource.MustParse("0")},
			},
			wantFields: []string{"tenants[0].quota"},
		},
		{
			name: "quota_negative",
			tenants: []QuotaPolicyTenant{
				{Name: "team-a", Quota: resource.MustParse("-1")},
			},
			wantFields: []string{"tenants[0].quota"},
		},
		{
			name: "one_bad_entry_yields_no_file",
			tenants: []QuotaPolicyTenant{
				{Name: "team-a", Quota: resource.MustParse("1Gi")},
				{Name: "_bad", Quota: resource.MustParse("2Gi")},
			},
			wantFields: []string{"tenants[1].name"},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			errs := ValidateQuotaPolicyTenants(c.tenants, field.NewPath("tenants"))
			got, err := RenderQuotaPolicy(c.tenants)

			if len(c.wantFields) > 0 {
				fields := make([]string, 0, len(errs))
				for _, e := range errs {
					fields = append(fields, e.Field)
				}
				assert.Equal(t, c.wantFields, fields, "the validator must name every refused input")
				require.Error(t, err, "the renderer must refuse what the validator refuses")
				assert.Nil(t, got, "a refused input yields no file at all, not a shortened one")
				return
			}

			assert.Empty(t, errs, "the validator must accept what the renderer renders")
			require.NoError(t, err)
			assert.Equal(t, c.want, string(got))
		})
	}
}

// TestRenderQuotaPolicyRefusalNamesEveryEntry pins that the refusal an operator reads carries the
// validator's own field errors rather than a summary, because the index is the only thing telling
// them which tenant of the set to fix.
func TestRenderQuotaPolicyRefusalNamesEveryEntry(t *testing.T) {
	_, err := RenderQuotaPolicy([]QuotaPolicyTenant{
		{Name: "", Quota: resource.MustParse("1Gi")},
		{Name: "team-b", Quota: resource.MustParse("0")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenants[0].name")
	assert.Contains(t, err.Error(), "tenants[1].quota")
}

// TestRenderQuotaPolicyBeginsWithVersion asserts the property the master aborts over, on its own:
// omitting version: 1 is what makes it throw std::runtime_error and CrashLoopBackOff, so no input
// may reach an output that does not start with it.
func TestRenderQuotaPolicyBeginsWithVersion(t *testing.T) {
	for _, tenants := range [][]QuotaPolicyTenant{
		nil,
		{},
		{{Name: "team-a", Quota: resource.MustParse("1Gi")}},
	} {
		got, err := RenderQuotaPolicy(tenants)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(string(got), "version: 1\n"), "got %q", string(got))
	}
}
