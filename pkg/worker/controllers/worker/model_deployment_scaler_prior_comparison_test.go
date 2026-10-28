package worker

import (
	"testing"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

func TestModelDeploymentScalerCapacityPriorControllerComparison(t *testing.T) {
	fixture := scalerSimulationFixture(t)
	for _, scenario := range fixture.Scenarios {
		if scenario.Name != "step_and_cooldown" && scenario.Name != "coverage_and_counter_reset" && scenario.Name != "restart" {
			continue
		}
		for _, predictive := range []bool{false, true} {
			name := "token"
			if predictive {
				name = "predictive"
			}
			t.Run(scenario.Name+"/"+name, func(t *testing.T) {
				priorScaler, priorTarget, catalog, instance := capacityPriorCompleteFixture(t)
				configure := func(target *workercore.ModelDeployment, scaler *workercore.ModelDeploymentScaler) {
					replicas := target.Spec.Roles[0].Replicas
					target.Spec = *priorTarget.Spec.DeepCopy()
					target.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
					target.Spec.Roles[0].Replicas = replicas
					target.Annotations = priorTarget.DeepCopy().Annotations
					token := scaler.Spec.Algorithm.TokenThroughput
					if predictive {
						token = &scaler.Spec.Algorithm.Predictive.TokenThroughput
					}
					token.TTFTTargetMilliseconds = quantityPointer("500")
					token.ITLTargetMilliseconds = quantityPointer("50")
					token.ServerTargetTokensPerSecond = quantityPointer("1500")
					if predictive {
						scaler.Spec.Algorithm.Predictive.ServerTokenCapacityPerReplica = quantityPointer("1500")
					}
				}
				manual := runScalerSimulation(t, scenario, scalerSimulationArm{Name: "manual", Predictive: predictive, Configure: configure})
				fromCatalog := runScalerSimulation(t, scenario, scalerSimulationArm{
					Name: "catalog", Predictive: predictive, InitialObjects: []ctrlcli.Object{catalog, instance},
					Configure: func(target *workercore.ModelDeployment, scaler *workercore.ModelDeploymentScaler) {
						configure(target, scaler)
						scaler.Spec.Algorithm.CapacityPrior = priorScaler.Spec.Algorithm.CapacityPrior.DeepCopy()
						if predictive {
							scaler.Spec.Algorithm.Predictive.ServerTokenCapacityPerReplica = nil
							scaler.Spec.Algorithm.Predictive.TokenThroughput.ServerTargetTokensPerSecond = nil
						} else {
							scaler.Spec.Algorithm.TokenThroughput.ServerTargetTokensPerSecond = nil
						}
					},
				})
				if fromCatalog.FinalScaler == nil {
					t.Fatal("missing final scaler status")
				}
				ready := scalerCondition(fromCatalog.FinalScaler, workercore.ModelDeploymentScalerConditionTypeCapacityPriorsReady)
				if ready.Status != meta.ConditionTrue || len(fromCatalog.FinalScaler.Status.ResolvedCapacityPriors) != 1 {
					t.Fatalf("Catalog never resolved: ready=%#v status=%#v", ready, fromCatalog.FinalScaler.Status.ResolvedCapacityPriors)
				}
				for _, field := range fromCatalog.FinalScaler.Status.ResolvedCapacityPriors[0].Fields {
					if field.Source != workercore.ModelDeploymentScalerCapacityFieldSourceCatalog || field.EffectiveTokensPerSecond.Cmp(*quantityPointer("1500")) != 0 {
						t.Fatalf("unexpected effective capacity evidence: %#v", field)
					}
				}
				// The trace records configured spec capacity, which intentionally remains absent in Catalog.
				for i := range manual.Trace {
					manual.Trace[i].EffectiveCapacity = 0
					manual.Trace[i].TokenCapacityConfigured = false
				}
				for i := range fromCatalog.Trace {
					fromCatalog.Trace[i].EffectiveCapacity = 0
					fromCatalog.Trace[i].TokenCapacityConfigured = false
				}
				manual.FinalScaler, fromCatalog.FinalScaler = nil, nil
				if !kubemeta.DeepEqual(manual, fromCatalog) {
					t.Fatalf("manual and Catalog diverged: manual changes=%d catalog changes=%d", manual.Changes, fromCatalog.Changes)
				}
				if scenario.Name == "step_and_cooldown" && manual.Changes == 0 {
					t.Fatal("comparison never exercised a replica commit")
				}
				assertScalerSimulationSafety(t, scenario, scalerSimulationArm{Name: "catalog", Predictive: predictive}, fromCatalog)
			})
		}
	}
}
