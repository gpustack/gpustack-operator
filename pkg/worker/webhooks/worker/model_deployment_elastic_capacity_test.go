package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

func TestElasticMaxDPCapacityAdmission(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		width     int32
		oldArgs   []string
		oldWidth  int32
		wantErr   string
		wantField string
	}{
		{name: "legacy omission", width: 2},
		{name: "legacy expansion", width: 4, oldWidth: 2},
		{name: "split maximum", args: []string{"--elastic-ep-max-dp-size", "4"}, width: 2},
		{name: "maximum equals width", args: []string{"--elastic-ep-max-dp-size=4"}, width: 4},
		{name: "larger startup maximum", args: []string{"--elastic-ep-max-dp-size=128"}, width: 4},
		{name: "create above maximum", args: []string{"--elastic-ep-max-dp-size=4"}, width: 5, wantErr: "--elastic-ep-max-dp-size", wantField: "elasticEp.width"},
		{name: "underscores", args: []string{"--elastic_ep_max_dp_size=4"}, width: 5, wantErr: "--elastic-ep-max-dp-size", wantField: "elasticEp.width"},
		{name: "unique prefix", args: []string{"--elastic-ep-max-dp", "4"}, width: 5, wantErr: "--elastic-ep-max-dp-size", wantField: "elasticEp.width"},
		{name: "last maximum wins", args: []string{"--elastic-ep-max-dp-size=2", "--elastic-ep-max-dp-size", "4"}, width: 4},
		{name: "last maximum refuses", args: []string{"--elastic-ep-max-dp-size=4", "--elastic-ep-max-dp-size=2"}, width: 4, wantErr: "--elastic-ep-max-dp-size", wantField: "elasticEp.width"},
		{name: "zero", args: []string{"--elastic-ep-max-dp-size=0"}, width: 2, wantErr: "below", wantField: "extraArgs"},
		{name: "negative", args: []string{"--elastic-ep-max-dp-size", "-1"}, width: 2, wantErr: "below", wantField: "extraArgs"},
		{name: "not integer", args: []string{"--elastic-ep-max-dp-size=4.5"}, width: 2, wantErr: "not an integer", wantField: "extraArgs"},
		{name: "overflow", args: []string{"--elastic-ep-max-dp-size=99999999999999999999999999"}, width: 2, wantErr: "out of range", wantField: "extraArgs"},
		{name: "missing value", args: []string{"--elastic-ep-max-dp-size"}, width: 2, wantErr: "has no value", wantField: "extraArgs"},
		{name: "end of options", args: []string{"--", "--elastic-ep-max-dp-size=2"}, width: 4},
		{name: "expand within maximum", args: []string{"--elastic-ep-max-dp-size=4"}, width: 4, oldArgs: []string{"--elastic-ep-max-dp-size", "4"}, oldWidth: 2},
		{name: "update above maximum", args: []string{"--elastic-ep-max-dp-size=4"}, width: 5, oldArgs: []string{"--elastic-ep-max-dp-size=4"}, oldWidth: 2, wantErr: "--elastic-ep-max-dp-size", wantField: "elasticEp.width"},
		{name: "increase maximum", args: []string{"--elastic-ep-max-dp-size=8"}, width: 4, oldArgs: []string{"--elastic-ep-max-dp-size=4"}, oldWidth: 2, wantErr: "fixed at creation", wantField: "extraArgs"},
		{name: "decrease maximum", args: []string{"--elastic-ep-max-dp-size=4"}, width: 2, oldArgs: []string{"--elastic-ep-max-dp-size=8"}, oldWidth: 2, wantErr: "fixed at creation", wantField: "extraArgs"},
		{name: "add maximum", args: []string{"--elastic-ep-max-dp-size=4"}, width: 2, oldWidth: 2, wantErr: "fixed at creation", wantField: "extraArgs"},
		{name: "remove maximum", width: 2, oldArgs: []string{"--elastic-ep-max-dp-size=4"}, oldWidth: 2, wantErr: "fixed at creation", wantField: "extraArgs"},
		{name: "equivalent maximum", args: []string{"--elastic_ep_max_dp_size", "+4"}, width: 4, oldArgs: []string{"--elastic-ep-max-dp-size=4"}, oldWidth: 2},
		{name: "unrelated tuning", args: []string{"--elastic-ep-max-dp-size=4", "--max-model-len=4096"}, width: 4, oldArgs: []string{"--elastic-ep-max-dp-size=4", "--max-model-len=2048"}, oldWidth: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := elasticMD()
			md.Spec.Roles[0].ExtraArgs = tc.args
			md.Spec.Roles[0].ElasticEP.Width = tc.width
			var old *workercore.ModelDeployment
			if tc.oldWidth != 0 {
				old = elasticMD()
				old.Spec.Roles[0].ExtraArgs = tc.oldArgs
				old.Spec.Roles[0].ElasticEP.Width = tc.oldWidth
			}
			w := new(ModelDeploymentWebhook)
			errs := w.ValidateModelDeploymentElasticEP(context.Background(), md, old)
			if tc.wantErr == "" {
				assert.Empty(t, errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Equal(t, "spec.roles[0]."+tc.wantField, errs[0].Field)
			assert.Contains(t, errs[0].Detail, tc.wantErr)
		})
	}
}

func TestElasticEPDiscardsLegacyHeadInstanceType(t *testing.T) {
	for _, format := range []string{"json", "protobuf"} {
		t.Run(format, func(t *testing.T) {
			profile := new(workercore.ModelDeploymentRoleElasticEP)
			if format == "json" {
				require.NoError(t, json.Unmarshal([]byte(`{"width":2,"headInstanceType":"removed-cpu-type"}`), profile))
			} else {
				legacy := make([]byte, 0, 4+len("removed-cpu-type"))
				legacy = append(legacy, 0x08, 0x02, 0x12, byte(len("removed-cpu-type")))
				legacy = append(legacy, []byte("removed-cpu-type")...)
				require.NoError(t, profile.Unmarshal(legacy))
			}
			require.Equal(t, int32(2), profile.Width)
			encoded, err := json.Marshal(profile)
			require.NoError(t, err)
			require.JSONEq(t, `{"width":2}`, string(encoded))
			md := elasticMD()
			md.Spec.Roles[0].ElasticEP = profile
			w := new(ModelDeploymentWebhook)
			require.Empty(t, w.ValidateModelDeploymentElasticEP(context.Background(), md, nil))
		})
	}
}
