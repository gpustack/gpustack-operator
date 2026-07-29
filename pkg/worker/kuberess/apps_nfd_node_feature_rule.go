package kuberess

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"gpustack.ai/gpustack/pkg/kubeappyaml"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/waitx"
)

const (
	// cpuInfoNodeFeatureRuleName is the name of the rule the scheduling chain starts at. It
	// is cluster-scoped and fixed, so every install mode converges on the same object.
	cpuInfoNodeFeatureRuleName = "gpustack-cpu-info"
	// cpuInfoNodeFeatureRuleTimeout bounds the wait for Node Feature Discovery's
	// NodeFeatureRule CRD. A chart install establishes it ahead of the release, since NFD
	// ships its CRDs under crds/, but a cluster bringing its own NFD installs them on its own
	// schedule, and a worker booting alongside that rollout can reach this step first.
	cpuInfoNodeFeatureRuleTimeout = 5 * time.Minute
	// cpuInfoNodeFeatureRuleInterval is the retry interval within that bound.
	cpuInfoNodeFeatureRuleInterval = 5 * time.Second
)

// cpuInfoNodeFeatureRuleTemplate is the rule that turns raw node hardware into the labels and
// annotations the rest of the chain reads: the CPU model annotations, and whether the node
// carries an acceleratable device at all.
//
// Its two matcher lists are what makes a node classified. The vendor IDs are those of the
// manufacturers this worker manages, so a manufacturer added to (or dropped from) the chart's
// `manufacturers` — which is where the worker's --manufacturer list comes from — is
// immediately detected (or ignored). The class prefixes are the ones NFD is configured to
// label, because a rule can only match a device NFD was told to publish.
const cpuInfoNodeFeatureRuleTemplate = `
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeatureRule
metadata:
  name: ` + cpuInfoNodeFeatureRuleName + `
  labels:
    "app.kubernetes.io/part-of": "gpustack-operator"
spec:
  rules:
    - name: "attached cpu info to annotations"
      annotations:
        feature.gpustack.ai/cpu-name: "@cpu.model.name"
        feature.gpustack.ai/cpu-family: "@cpu.model.family"
        feature.gpustack.ai/cpu-physical-cores: "@cpu.model.physical_cores"
        feature.gpustack.ai/cpu-threads-per-core: "@cpu.model.threads_per_core"
        feature.gpustack.ai/cpu-logical-cores: "@cpu.model.logical_cores"
        feature.gpustack.ai/cpu-stepping: "@cpu.model.stepping"
        feature.gpustack.ai/cpu-cache-line: "@cpu.model.cache_line"
        feature.gpustack.ai/cpu-hz: "@cpu.model.hz"
        feature.gpustack.ai/cpu-boost-freq: "@cpu.model.boost_freq"
        feature.gpustack.ai/cpu-cache-l1i: "@cpu.model.cache_l1i"
        feature.gpustack.ai/cpu-cache-l1d: "@cpu.model.cache_l1d"
        feature.gpustack.ai/cpu-cache-l2: "@cpu.model.cache_l2"
        feature.gpustack.ai/cpu-cache-l3: "@cpu.model.cache_l3"
      matchFeatures:
        - feature: cpu.model
          matchExpressions:
            vendor_id:
              op: Exists
    - name: "detect whether the node has acceleratable devices"
      vars:
        has-acceleratable-devices: "true"
      matchFeatures:
        - feature: pci.device
          matchExpressions:
            class:
              op: InRegexp
              value:
              {{- range $.PciClassPrefixes }}
                - {{ printf "^%s" . | quote }}
              {{- end }}
            vendor:
              op: In
              value:
              {{- range $.PciVendorIDs }}
                - {{ . | quote }}
              {{- end }}
    - name: "identify the node without any acceleratable device"
      labels:
        feature.gpustack.ai/acceleratable: "false"
      matchFeatures:
        - feature: rule.matched
          matchExpressions:
            has-acceleratable-devices:
              op: DoesNotExist
`

// InstallCPUInfoNodeFeatureRule applies the gpustack-cpu-info NodeFeatureRule, retrying until
// Node Feature Discovery's CRD is established.
//
// The chart cannot ship this object: its CRD belongs to NFD, and the rule is needed even where
// the chart deploys no NFD at all — a cluster running its own — which is precisely the install
// whose manifest Helm cannot map. Apply only sets spec, so the step is safe to repeat.
func InstallCPUInfoNodeFeatureRule(ctx context.Context, manufacturers []string) error {
	content, err := cpuInfoNodeFeatureRule(manufacturers)
	if err != nil {
		return fmt.Errorf("render the cpu-info NodeFeatureRule: %w", err)
	}

	restCfg := system.LoopbackKubeRestConfig.Get()

	err = waitx.PollUntilContextTimeout(ctx,
		cpuInfoNodeFeatureRuleInterval, cpuInfoNodeFeatureRuleTimeout, true,
		func(ctx context.Context) error {
			return kubeappyaml.Apply(ctx, content, restCfg)
		})
	if err != nil {
		return fmt.Errorf("waiting for node feature discovery's NodeFeatureRule CRD: %w", err)
	}

	return nil
}

// cpuInfoNodeFeatureRule renders the rule for the given manufacturers. A name no manufacturer
// answers to contributes no vendor ID, so an unknown one narrows the match instead of
// rendering an empty matcher.
func cpuInfoNodeFeatureRule(manufacturers []string) (string, error) {
	vendorIDs := sets.New[string]()
	for _, manufacturer := range manufacturers {
		if id := nodefeature.GetPciVendorID(manufacturer); id != "" {
			vendorIDs.Insert(id)
		}
	}

	return kubeappyaml.Template(cpuInfoNodeFeatureRuleTemplate).Render(map[string]any{
		"PciClassPrefixes": nodefeature.GetAcceleratablePciClassPrefixes(),
		"PciVendorIDs":     sets.List(vendorIDs),
	}, nil)
}
