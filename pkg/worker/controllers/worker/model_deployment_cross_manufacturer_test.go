package worker

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// These cases answer what this operator WRITES for a deployment whose two roles sit on two
// manufacturers. They do not answer whether the two halves can then exchange anything: that depends
// on the engines' own store backends, and no fixture in this package reaches one. The two readings
// are one sentence apart and unlock completely different next steps, so the distinction is stated
// here rather than left to a reader of a green run.
//
// The shape is reachable. Nothing in admission compares one role's manufacturer with another's, so
// an administrator may pair roles across vendors -- which is why what gets rendered is worth
// pinning.
//
// What decides whether anything is rendered at all is the POOL's transport, and the first case below
// is about that. Its rule is ONE-SIDED rather than about spanning: an Ascend role requires a
// transport that no other engine requires, so a same-manufacturer all-Ascend deployment meets the
// same refusal. Reading it as a cross-manufacturer rule predicts that same-manufacturer pairs are
// safe, which is the one conclusion the fixture disproves.

// crossManufacturerAscendType is the second InstanceType, carrying the second manufacturer.
//
// THE NAME SPELLS THE HARDWARE IT REPORTS. The existing second fixture is an NVIDIA part under an
// NVIDIA name, and reusing it here while overriding the manufacturer would leave a reader unable to
// tell a deliberate mismatch from a careless one.
//
// The family and the runtime version are Ascend's own rather than the first fixture's. Image
// synthesis reads both, so carrying NVIDIA's runtime version here would render a tag whose backend
// and version disagree while still satisfying an assertion written against it.
func crossManufacturerAscendType() *worker.InstanceType {
	return newRenderInstanceType(func(it *worker.InstanceType) {
		it.Name = "ascend-910c-8x"
		it.Status.Entrance = "queue-for-ascend-910c-8x"
		it.Status.Detail.Manufacturer = nodefeature.ManufacturerAscend
		it.Status.Detail.Family = "910C"
		it.Status.Detail.RuntimeVersion = "9.1"
		it.Status.Detail.RuntimeVersions = []string{"9.1"}
	})
}

// newAscendTransportBackend is the pool backend offering the transport vLLM-Ascend's store backend
// requires. The default fixture offers TCP, which that engine refuses -- see the refusal case below.
func newAscendTransportBackend() *workercore.KVCacheBackend {
	return &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "kvcb"},
		Spec: workercore.KVCacheBackendSpec{
			Transport: workercore.KVCacheBackendTransport{Protocol: "Ascend"},
		},
	}
}

// crossManufacturerDeployment puts the two roles on the two InstanceTypes above.
//
// Two instanceTypes are two pod groups, which is what makes each half independently observable: one
// group per queue, per image and per connector.
func crossManufacturerDeployment(
	mutate ...func(*workercore.ModelDeployment),
) *workercore.ModelDeployment {
	return twoRoleDeployment(append([]func(*workercore.ModelDeployment){
		func(md *workercore.ModelDeployment) {
			md.Spec.Roles[1].InstanceType = "ascend-910c-8x"
		},
	}, mutate...)...)
}

// asPD sets the prefill/decode kinds on a two-role deployment.
func asPD(md *workercore.ModelDeployment) {
	md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
	md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
}

// synthesizingImages clears the image each role's template names, so the runner image is derived
// from the hardware instead. A case keeping the shared fixture's template would assert that
// literal rather than anything the manufacturer decided.
func synthesizingImages(md *workercore.ModelDeployment) {
	md.Spec.Roles[0].Template.Image = ""
	md.Spec.Roles[1].Template.Image = ""
}

// crossManufacturerClient converges the deployment against both types, on a pool offering the
// transport the Ascend half requires.
//
// The pool, the binding and the backend are in the fixture for the reason the per-role connector
// case already states: a fixture carrying only the Binding resolves no connection, and every claim
// about a connector then holds vacuously.
func crossManufacturerClient(md *workercore.ModelDeployment) ctrlcli.Client {
	return newModelDeploymentClient(md,
		newRenderInstanceType(), crossManufacturerAscendType(),
		newRenderBinding(), newRenderPool(), newAscendTransportBackend())
}

// connectorNameByRole reads the connector name out of each replica's rendered argument, keyed by the
// role the replica belongs to.
//
// It parses the document rather than matching the whole argument as a string, so the assertion is
// about the connector name alone: the kv_role beside it varies per case, and a whole-string
// comparison would make every case depend on both.
func connectorNameByRole(t *testing.T, cli ctrlcli.Client) map[string]string {
	t.Helper()

	byRole := map[string]string{}
	pods := replicaPods(t, cli)
	for i := range pods {
		pod := &pods[i]
		argv := pod.Spec.Containers[0].Command
		at := slices.Index(argv, "--kv-transfer-config")
		require.GreaterOrEqual(t, at, 0, "%s carries no transfer configuration at all", pod.Name)
		require.Less(t, at+1, len(argv), "%s carries the flag with no document", pod.Name)

		var cfg struct {
			KVConnector string `json:"kv_connector"`
		}
		require.NoError(t, json.Unmarshal([]byte(argv[at+1]), &cfg),
			"%s renders a document that is not JSON: %s", pod.Name, argv[at+1])
		require.NotEmpty(t, cfg.KVConnector, "%s renders no connector name", pod.Name)

		byRole[modelDeploymentPodRole(pod)] = cfg.KVConnector
	}

	return byRole
}

// imageByRole reads the image each replica runs, keyed by the role it belongs to.
func imageByRole(t *testing.T, cli ctrlcli.Client) map[string]string {
	t.Helper()

	byRole := map[string]string{}
	pods := replicaPods(t, cli)
	for i := range pods {
		pod := &pods[i]
		byRole[modelDeploymentPodRole(pod)] = pod.Spec.Containers[0].Image
	}

	return byRole
}

// entranceByRole reads the queue entrance each replica carries, keyed by the role it belongs to.
func entranceByRole(t *testing.T, cli ctrlcli.Client) map[string]string {
	t.Helper()

	byRole := map[string]string{}
	pods := replicaPods(t, cli)
	for i := range pods {
		pod := &pods[i]
		byRole[modelDeploymentPodRole(pod)] = pod.Labels[kueuectrlconst.QueueLabel]
	}

	return byRole
}

// TestModelDeploymentCrossManufacturer_ThePoolTransportDecidesWhetherItRendersAtAll is the first
// thing such a deployment meets, and it is NOT the connector.
//
// THE RULE IS ONE-SIDED, and stating it as a cross-manufacturer rule would be wrong in a way that
// predicts the opposite of the truth. Only vLLM-Ascend's store backend requires a transport: it
// accepts one and raises on every other, while plain vLLM's and SGLang's never compare the value at
// all. So what is refused is ANY deployment holding an Ascend role on a pool offering anything else
// -- whether the other role is on another manufacturer, or on the same one, or absent.
//
// A cross-manufacturer pair is therefore refused BECAUSE it contains an Ascend role, not because it
// spans manufacturers. The all-Ascend row is what separates those two readings: a table without it
// is equally consistent with a rule about spanning, and that rule would go on to predict that
// same-manufacturer pairs are safe. They are not.
//
// THE DEFAULT IS THE REFUSING VALUE. An unset protocol resolves to TCP, so the pool an administrator
// never configured is exactly the pool these shapes cannot use, which makes the refusal the ordinary
// outcome rather than a corner case.
//
// THE ALL-NVIDIA ROW IS THE POSITIVE BASELINE. Without it, every refusal here could equally be
// produced by a fixture that renders nothing at all, and the assertions would hold for a reason that
// has nothing to do with transports.
//
// KIND IS NOT WHAT DECIDES IT, which is why both shapes are rows. The engine is selected by the
// role's manufacturer, so two server roles reach the same refusal as a prefill/decode pair.
func TestModelDeploymentCrossManufacturer_ThePoolTransportDecidesWhetherItRendersAtAll(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*workercore.ModelDeployment)
		live      []ctrlcli.Object
		roleTypes [2]string
		refused   bool
	}{
		{
			name:      "pd_across_manufacturers_on_the_default_transport",
			mutate:    asPD,
			live:      []ctrlcli.Object{newRenderInstanceType(), crossManufacturerAscendType()},
			roleTypes: [2]string{"h20-8x", "ascend-910c-8x"},
			refused:   true,
		},
		{
			name:      "two_servers_across_manufacturers_on_the_default_transport",
			mutate:    func(*workercore.ModelDeployment) {},
			live:      []ctrlcli.Object{newRenderInstanceType(), crossManufacturerAscendType()},
			roleTypes: [2]string{"h20-8x", "ascend-910c-8x"},
			refused:   true,
		},
		{
			// NOT A CROSS-MANUFACTURER SHAPE AT ALL, and that is the row's entire purpose. Both
			// roles sit on one Ascend type, so nothing here spans anything -- and it is refused
			// just the same. A reader carrying "cross-manufacturer is the broken one" away from
			// this file would have been carrying a rule that says this shape works.
			name:      "one_manufacturer_is_no_help_when_that_manufacturer_is_ascend",
			mutate:    asPD,
			live:      []ctrlcli.Object{crossManufacturerAscendType()},
			roleTypes: [2]string{"ascend-910c-8x", "ascend-910c-8x"},
			refused:   true,
		},
		{
			name:      "pd_on_nvidia_is_what_the_default_transport_serves",
			mutate:    asPD,
			live:      []ctrlcli.Object{newRenderInstanceType(), newRenderInstanceTypeB()},
			roleTypes: [2]string{"h20-8x", "a100-8x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].InstanceType = tc.roleTypes[0]
				md.Spec.Roles[1].InstanceType = tc.roleTypes[1]
			}, tc.mutate)

			objs := append([]ctrlcli.Object{md}, tc.live...)
			objs = append(objs, newRenderBinding(), newRenderPool(), newRenderBackend())
			cli := newModelDeploymentClient(objs...)

			_, err := reconcileModelDeployment(t, cli)
			if !tc.refused {
				require.NoError(t, err)
				assert.NotEmpty(t, replicaPods(t, cli), "the baseline has to actually render")

				return
			}

			require.Error(t, err)
			// The message has to name the pair, not just report a refusal: the transport is a legal
			// value and the engine is a legal engine, and it is the combination that raises.
			assert.Contains(t, err.Error(),
				`accepts only the "ascend" transport and this pool offers "tcp"`,
				"the refusal names which transport the engine needs and which one the pool has")
			assert.Contains(t, err.Error(), "vllm-ascend",
				"and names the engine the role's manufacturer selected")
		})
	}
}

// TestModelDeploymentCrossManufacturer_TheTwoHalvesRenderDifferentConnectors is the observation the
// file exists for, and the two expected values are NOT equal on purpose.
//
// vLLM-Ascend pins a vLLM release whose connector factory has no MooncakeStoreConnector registered
// at all, so rendering vLLM's name for that engine aborts it at startup. The two names being equal
// is therefore the defect the per-engine mapping exists to prevent, not the success condition -- a
// case written to accept equality would pass exactly when the deployment is broken.
//
// The rendering package already pins the name each ENGINE resolves to. What is pinned here is the
// step before it: a role's own manufacturer is what selects the engine, so one deployment holding
// two manufacturers renders two connectors. Every other case in this package puts both roles on one
// manufacturer, where the two names agree and the selection cannot be observed.
//
// REACHING THIS AT ALL TAKES THE POOL FROM THE CASE ABOVE. A pool on the default transport never
// gets here, so what this pins is the shape an administrator arrives at only after following that
// refusal's remediation.
func TestModelDeploymentCrossManufacturer_TheTwoHalvesRenderDifferentConnectors(t *testing.T) {
	cli := crossManufacturerClient(crossManufacturerDeployment(asPD))

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	byRole := connectorNameByRole(t, cli)
	require.Len(t, byRole, 2, "one connector name per role")

	assert.Equal(t, "MooncakeStoreConnector", byRole["prefill"],
		"the half on NVIDIA gets the name vLLM's own factory registers")
	assert.Equal(t, "AscendStoreConnector", byRole["decode"],
		"the half on Ascend gets the name vLLM-Ascend's factory registers")
	assert.NotEqual(t, byRole["prefill"], byRole["decode"],
		"the two halves are configured for two different connectors, and equal names here would "+
			"mean one of them cannot start")
}

// TestModelDeploymentCrossManufacturer_EachRoleRendersItsOwnImageBackend covers the runner image.
func TestModelDeploymentCrossManufacturer_EachRoleRendersItsOwnImageBackend(t *testing.T) {
	cli := crossManufacturerClient(crossManufacturerDeployment(asPD, synthesizingImages))

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	byRole := imageByRole(t, cli)
	require.Len(t, byRole, 2, "one image per role")

	// The backend token and the runtime version travel together: both come from the role's own
	// InstanceType, so a role reading the other one's detail would show up in either segment.
	assert.Equal(t, "gpustack/runner:cuda12.9-vllm0.25.1", byRole["prefill"],
		"the NVIDIA half renders the cuda backend at its own runtime version")
	assert.Equal(t, "gpustack/runner:cann9.1-a3-vllm0.25.1", byRole["decode"],
		"the Ascend half renders the cann backend, at its own runtime version and with the "+
			"variant that is the one entry no lowercasing produces")
	assert.NotEqual(t, byRole["prefill"], byRole["decode"],
		"the two halves run two different images")
}

// TestModelDeploymentCrossManufacturer_EachGroupEntersItsOwnQueue covers the entrance label, which
// is what routes a replica into a pool.
//
// The entrance is READ from each InstanceType's published field rather than derived from its name,
// and both fixtures spell it in a way no derivation produces, so this asserts a read rather than a
// coincidence.
func TestModelDeploymentCrossManufacturer_EachGroupEntersItsOwnQueue(t *testing.T) {
	cli := crossManufacturerClient(crossManufacturerDeployment(asPD))

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	byRole := entranceByRole(t, cli)
	require.Len(t, byRole, 2, "one entrance per role")

	assert.Equal(t, "queue-for-h20-8x", byRole["prefill"])
	assert.Equal(t, "queue-for-ascend-910c-8x", byRole["decode"])
	assert.NotEqual(t, byRole["prefill"], byRole["decode"],
		"the halves enter two pools, which is what makes them two Workloads rather than one")
}

// TestModelDeploymentCrossManufacturer_EachQueueCoversItsOwnCredits is the other half of the
// scheduling observation, and it is deliberately NOT reached through a ModelDeployment.
//
// A queue's covered resource is a property of the pool, decided before any deployment names it. The
// claim being pinned is that two manufacturers produce two queues crediting two different resources
// -- so nothing here may depend on a deployment existing, and a fixture routed through one would be
// asserting the deployment's own reconcile instead.
func TestModelDeploymentCrossManufacturer_EachQueueCoversItsOwnCredits(t *testing.T) {
	const (
		nvidiaKey = "nvidia-h20"
		ascendKey = "ascend-910c"
	)

	cli := buildNodeQueueClient(
		newInstanceTypeQueue(nvidiaKey, true),
		newInstanceTypeQueue(ascendKey, true),
		newNodesFlavor("gpustack-nvidia-h20-linux-amd64-8d", nvidiaKey, 8, 8,
			accelerated(nodefeature.ManufacturerNVIDIA)),
		newNodesFlavor("gpustack-ascend-910c-linux-arm64-8d", ascendKey, 8, 8,
			accelerated(nodefeature.ManufacturerAscend)),
	)

	covered := map[string]core.ResourceName{}
	for _, key := range []string{nvidiaKey, ascendKey} {
		name := nodeQueueName(key)
		reconcileNodeQueueN(t, cli, name, 2)

		cq, err := getClusterQueue(t, cli, name)
		require.NoError(t, err)
		require.Len(t, cq.Spec.ResourceGroups, 1, "%s: one resource group", key)
		require.Len(t, cq.Spec.ResourceGroups[0].CoveredResources, 1,
			"%s: one covered resource", key)
		covered[key] = cq.Spec.ResourceGroups[0].CoveredResources[0]
	}

	assert.Equal(t, nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerNVIDIA),
		covered[nvidiaKey])
	assert.Equal(t, nodefeature.GetAcceleratableCreditsResourceName(nodefeature.ManufacturerAscend),
		covered[ascendKey])
	assert.NotEqual(t, covered[nvidiaKey], covered[ascendKey],
		"each manufacturer's queue credits its own resource, so neither queue can admit a "+
			"Workload asking for the other's")
}

// TestModelDeploymentCrossManufacturer_TwoServerRolesAreTheSameRendering covers the shape a user
// reaches WITHOUT asking for anything.
//
// The kind field defaults to server, so two roles that set no kind are two server roles -- and
// admission refuses mixing server with the others while saying nothing about two servers. That
// makes this, not the prefill/decode pair, the cross-manufacturer shape the field defaults produce.
//
// THE ROLES ARE RENAMED. The shared two-role fixture calls them prefill and decode, which are role
// NAMES there and not kinds; keeping them while every kind is server would leave the case reading as
// a P/D pair to anyone who did not check which of the two the label carries.
//
// What it pins is that the manufacturer split is not a property of the P/D split: each half still
// gets its own connector and its own image on a shape where neither role is a prefiller or a decoder
// at all.
func TestModelDeploymentCrossManufacturer_TwoServerRolesAreTheSameRendering(t *testing.T) {
	md := crossManufacturerDeployment(synthesizingImages, func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Name = "on-nvidia"
		md.Spec.Roles[1].Name = "on-ascend"
	})
	require.Empty(t, md.Spec.Roles[0].Kind, "the shape is the one where nothing sets a kind")
	require.Empty(t, md.Spec.Roles[1].Kind, "the shape is the one where nothing sets a kind")

	cli := crossManufacturerClient(md)

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	connectors := connectorNameByRole(t, cli)
	require.Len(t, connectors, 2, "one connector name per role")
	assert.Equal(t, "MooncakeStoreConnector", connectors["on-nvidia"])
	assert.Equal(t, "AscendStoreConnector", connectors["on-ascend"])
	assert.NotEqual(t, connectors["on-nvidia"], connectors["on-ascend"],
		"the connector follows the manufacturer, not the kind")

	images := imageByRole(t, cli)
	require.Len(t, images, 2, "one image per role")
	assert.Equal(t, "gpustack/runner:cuda12.9-vllm0.25.1", images["on-nvidia"])
	assert.Equal(t, "gpustack/runner:cann9.1-a3-vllm0.25.1", images["on-ascend"])
}
