package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/systemname"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/utils/slicex"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
)

const (
	// ModelDeploymentResourceType is the resource note every object a ModelDeployment renders
	// carries, so a watch can tell them from every other Pod in the namespace.
	ModelDeploymentResourceType = "modeldeployments"
	// ModelDeploymentResourceNoteRole names the role a rendered object belongs to.
	ModelDeploymentResourceNoteRole = "role"

	// modelDeploymentLabelKeyName, modelDeploymentLabelKeyInstance and
	// modelDeploymentLabelKeyComponent are the identity labels a replica's Pod carries.
	//
	// They exist alongside the resource note because a Service selects on LABELS and cannot read an
	// annotation, and because they carry only identity — which deployment, which role — and never
	// anything a spec update can move. A selector that moved would orphan the Pods it used to
	// front. The same three keys, with the same meanings, front the KV cache backend's leader.
	modelDeploymentLabelKeyName      = "app.kubernetes.io/name"
	modelDeploymentLabelKeyInstance  = "app.kubernetes.io/instance"
	modelDeploymentLabelKeyComponent = "app.kubernetes.io/component"
	modelDeploymentLabelValueName    = "model-deployment"

	// modelDeploymentPodSpecHashAnnotation carries the fingerprint of the Pod a role's spec renders
	// to. The rollout is recreate rather than surge, so this is the whole of how a replica built
	// before a spec change is told from one built after it.
	modelDeploymentPodSpecHashAnnotation = "modeldeployment." + systemname.LabelPrefix + "pod-spec-hash"

	// modelDeploymentDefaultPort is the port a replica serves on when the role's template names
	// none. It is the port every supported engine's OpenAI-compatible server listens on by default.
	modelDeploymentDefaultPort int32 = 8000
	// modelDeploymentDefaultPortName names that port on the container and on the Service fronting it.
	modelDeploymentDefaultPortName = "http"
)

// ModelDeploymentRenderInput is everything one replica's Pod is rendered from.
//
// The InstanceType and the connector arrive as values rather than being read here, because the
// render must stay a pure function of its inputs: the reconciler converges the same object on every
// pass, so a render that reached for a client could return two different Pods for one spec and roll
// the deployment forever.
type ModelDeploymentRenderInput struct {
	// Deployment is the object being rendered.
	Deployment *workercore.ModelDeployment
	// Role is the entry of Deployment.Spec.Roles this replica belongs to.
	Role *workercore.ModelDeploymentRole
	// Ordinal is the replica's index within the role, starting at zero.
	Ordinal int32
	// InstanceType is the type the role's Pods are admitted against. It supplies how to spell the
	// accelerator keys and the per-card unit resources the host request is derived from.
	InstanceType *worker.InstanceType
	// Connector is what the engine needs to reach the pool. Its zero value renders a replica with
	// no connector at all, which is what a deployment whose Binding has not been resolved yet gets.
	Connector ModelDeploymentConnectorRender
	// There is NO ConfigMap name here, and there is no object to name: the client configuration
	// travels in Connector.PodAnnotations and reaches the container as a downwardAPI projection of
	// it. The field this struct used to carry was never filled by anything, so the mount it guarded
	// was dead code -- see T14 in the spec for why the carrier is the annotation.
	// RuntimeClassName is the runtime class an accelerated replica needs. The reconciler resolves it,
	// because deciding it requires reading whether the class exists on the cluster.
	RuntimeClassName string
	// GeneralResourcesOvercommit mirrors the Instance path's overcommit setting, which decides
	// whether the derived CPU and memory are requested at full size or scaled down.
	GeneralResourcesOvercommit bool
}

// modelDeploymentPodName is the name of one replica's Pod: <deployment>-<role>-<ordinal>.
//
// The ordinal is part of the name rather than a random suffix so that scaling down is decidable
// without reading anything: the replicas to remove are the ones whose ordinal is at or above the
// new count, and a hand-deleted replica is recreated under the name it had.
func modelDeploymentPodName(md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, ordinal int32) string {
	return md.Name + "-" + role.Name + "-" + strconvx.Itoa(int(ordinal))
}

// modelDeploymentSelectorLabels is what fronts a role's replicas: the identity of the deployment and
// of the role, and nothing a spec update can move.
func modelDeploymentSelectorLabels(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) map[string]string {
	return map[string]string{
		modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
		modelDeploymentLabelKeyInstance:  md.Name,
		modelDeploymentLabelKeyComponent: role.Name,
	}
}

// renderModelDeploymentPod renders one replica.
//
// It returns an error rather than a best-effort Pod whenever the request cannot be sized — an
// InstanceType whose accelerator detail has not been computed yet, a role with no image. Falling
// back to a whole-card or an empty request would produce a Pod that runs and charges the wrong
// quota, which is the failure this whole path exists to avoid.
func renderModelDeploymentPod(in ModelDeploymentRenderInput) (*core.Pod, error) {
	md, role := in.Deployment, in.Role

	tmpl := role.Template
	if tmpl == nil {
		// A role may name no template at all and still render, because the image can be
		// synthesized. Every other template field then takes its zero value.
		tmpl = new(workercore.ModelDeploymentTemplate)
	}

	// A STATED IMAGE ALWAYS WINS, and synthesis is the fallback rather than the rule: it is how a
	// role runs a private build, a vendor with no published runner backend, or an Ascend family the
	// matrix does not carry. The formula reads no release matrix, so it cannot know the tag it
	// assembles was ever published - that is the accepted trade, and its failure is an
	// ImagePullBackOff rather than a silent misconfiguration.
	image := tmpl.Image
	if image == "" {
		synthesized, err := SynthesizeModelDeploymentImage(
			md.Spec.Engine, md.Spec.EngineVersion, in.InstanceType.Status.Detail)
		if err != nil {
			return nil, fmt.Errorf("role %q names no image and none could be synthesized: %w", role.Name, err)
		}
		image = synthesized
	}

	ress, err := deriveModelDeploymentResources(role, in.InstanceType)
	if err != nil {
		return nil, err
	}

	// THE ENTRANCE IS READ, NOT RECOMPUTED, and the difference is the whole point of this line.
	// role.InstanceType names an InstanceType; the label Kueue admits on names the LocalQueue that
	// fronts that type's ClusterQueue, and the type's own status is where that name is published --
	// by the same reconcile that creates the LocalQueue. Deriving it here from the name instead
	// would be a second answer to one question, written by a second function: equal today, because
	// the ClusterQueue carries the InstanceType's name and both sides spell it with
	// FormatLocalQueueName, and silently divergent the day either half of that stops holding. The
	// half that drifted would be this one, whose symptom is a Pod routed at a LocalQueue that does
	// not exist -- admitted by nothing, with nothing naming the field.
	entrance := in.InstanceType.Status.Entrance
	if entrance == "" {
		// An InstanceType whose status has not been computed yet, exactly like an accelerator detail
		// that is not there: refuse to render rather than fall back. A Pod carrying no queue-name
		// label at all is not queued by Kueue, it is admitted by the kubelet directly -- charging no
		// quota and bypassing every gate the chain exists to apply.
		return nil, fmt.Errorf(
			"instance type %q publishes no queue entrance yet", in.InstanceType.Name)
	}

	// A role that replaces the command owns the whole argv, so the operator contributes neither
	// engine arguments nor the client environment that only its own arguments would have used.
	takeOver := len(tmpl.Command) > 0

	command := tmpl.Command
	if !takeOver {
		command, err = ModelDeploymentEngineCommand(md.Spec.Engine, md.Spec.Model.Name)
		if err != nil {
			return nil, err
		}
		command = append(command, in.Connector.Args...)
		command = append(command, role.ExtraArgs...)
	}

	// The connector's volume and mount arrive already built, and they are taken as a unit with the
	// annotations applied below: the volume is a downwardAPI projection of one of them, so applying
	// it without the annotation mounts an empty file. Both are empty for an engine configured
	// through the environment, which is why neither is conditional on the engine here.
	//
	// A take-over role gets NEITHER, along with no synthesized argument and no client environment.
	// The operator did not build that command line and cannot claim the container uses the cache.
	vols, mounts := convertAdditionalVolumes(tmpl.AdditionalVolumes)
	if !takeOver {
		vols = append(vols, in.Connector.Volumes...)
		mounts = append(mounts, in.Connector.VolumeMounts...)
	}

	mainC := core.Container{
		Name:            "main",
		Image:           image,
		ImagePullPolicy: tmpl.ImagePullPolicy,
		Command:         command,
		Resources: getResourceRequirements(
			ress, in.InstanceType, true, in.GeneralResourcesOvercommit, true, false),
		Ports:        modelDeploymentContainerPorts(tmpl),
		Env:          mergeModelDeploymentEnv(md.Spec.Engine, role, in.Connector, takeOver),
		VolumeMounts: mounts,
	}
	if tmpl.Privileged {
		mainC.SecurityContext = &core.SecurityContext{Privileged: ptr.To(true)}
	}

	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      modelDeploymentPodName(md, role, in.Ordinal),
			Namespace: md.Namespace,
			Labels:    modelDeploymentPodLabels(md, role, entrance),
		},
		Spec: core.PodSpec{
			// A replica is not a shell box: nothing nsenters into it, so it needs neither the host
			// IPC namespace nor a shared process namespace, and it never reads the API.
			AutomountServiceAccountToken: ptr.To(false),
			EnableServiceLinks:           ptr.To(false),
			// Recreate is the rollout policy, and a replica that exits is replaced by the
			// reconciler under the same name rather than restarted in place with a stale spec.
			RestartPolicy: core.RestartPolicyAlways,
			ImagePullSecrets: func() []core.LocalObjectReference {
				if tmpl.ImagePullSecret == nil {
					return nil
				}
				return []core.LocalObjectReference{*tmpl.ImagePullSecret}
			}(),
			Volumes:    vols,
			Containers: []core.Container{mainC},
		},
	}
	if in.RuntimeClassName != "" {
		pod.Spec.RuntimeClassName = ptr.To(in.RuntimeClassName)
	}

	// No nodeSelector entry is rendered for the accelerator model. A role takes whatever flavor its
	// pool assigns, which is what every role did before per-role model selection existed and what
	// they all do again now that it is gone: Kueue evaluates a candidate flavor per PodSet, and with
	// no selector to match against there is nothing to narrow the choice within one pool.
	systemmeta.NoteResource(pod, ModelDeploymentResourceType, map[string]string{
		ModelDeploymentResourceNoteRole: role.Name,
	})
	kubemeta.ControlOnWithoutBlock(pod, md, workercore.SchemeGroupVersionKind("ModelDeployment"))

	// The Kueue group metadata, which is what makes every replica of every role ONE Workload rather
	// than one Workload each. It goes on here, before the fingerprint, for the same reason the
	// connector's annotations do: the group's declared total is one of the values a spec change
	// moves, and a fingerprint blind to it would leave every replica declaring a size the deployment
	// no longer has.
	//
	// The labels and the annotations are applied together because the group's own type returns them
	// together -- a Pod carrying the membership label without the total count joins a group whose
	// size Kueue cannot learn, and no Workload is composed at all.
	group := ModelDeploymentPodGroup(md, role)
	for k, v := range group.Labels {
		pod.Labels[k] = v
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string, len(group.Annotations)+1)
	}
	for k, v := range group.Annotations {
		pod.Annotations[k] = v
	}

	// THE CONNECTOR'S ANNOTATIONS GO ON BEFORE THE FINGERPRINT, and the order is the whole reason
	// this carrier works. The client configuration lives in one of these annotations rather than in
	// a ConfigMap, and the fingerprint below covers {Labels, Annotations, PodSpec} -- so changing
	// the pool endpoint or the domain moves the hash and the replicas are recreated to pick it up.
	// Moving this after the fingerprint would leave the hash blind to the configuration and every
	// replica holding a stale one, with nothing failing.
	//
	// Gated on the same take-over check as the volume that projects them: a role that replaced the
	// command line gets no part of the connector, and half of it would be worse than none.
	if !takeOver && len(in.Connector.PodAnnotations) > 0 {
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string, len(in.Connector.PodAnnotations)+1)
		}
		for k, v := range in.Connector.PodAnnotations {
			pod.Annotations[k] = v
		}
	}

	// The fingerprint is written last so that it covers everything above it, and it is read back on
	// every pass to decide whether a running replica was built from the current spec.
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string, 1)
	}
	pod.Annotations[modelDeploymentPodSpecHashAnnotation] = modelDeploymentPodSpecHash(pod)

	return pod, nil
}

// modelDeploymentPodLabels is what a replica carries: the selector, the queue-name entrance label
// that routes it into the role's pool, and the part-of label every object this operator renders
// carries.
//
// The entrance label sits outside the selector deliberately. It follows the role's InstanceType,
// which a spec update can change, and a selector that moved with it would orphan every replica
// already running.
//
// The entrance arrives as a VALUE, read from the InstanceType's status by the caller rather than
// derived from role.InstanceType here, so that this operator and the reconcile that creates the
// LocalQueue cannot disagree about the queue's name. See renderModelDeploymentPod for what deriving
// it would cost.
func modelDeploymentPodLabels(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, entrance string,
) map[string]string {
	labels := modelDeploymentSelectorLabels(md, role)
	labels[kueuectrlconst.QueueLabel] = entrance
	labels["app.kubernetes.io/part-of"] = "gpustack-operator-worker"

	return labels
}

// modelDeploymentContainerPorts renders the ports a replica exposes.
//
// A role that names none still gets one, because a replica nothing can reach serves nothing and the
// Service fronting the deployment needs a target. Every supported engine's OpenAI-compatible server
// listens on 8000 by default.
func modelDeploymentContainerPorts(tmpl *workercore.ModelDeploymentTemplate) []core.ContainerPort {
	if len(tmpl.Ports) == 0 {
		return []core.ContainerPort{{
			Name:          modelDeploymentDefaultPortName,
			Protocol:      core.ProtocolTCP,
			ContainerPort: modelDeploymentDefaultPort,
		}}
	}

	return slicex.Transform(tmpl.Ports, func(p workercore.InstancePort) core.ContainerPort {
		return core.ContainerPort{
			Name:          getPortName(p),
			Protocol:      p.Protocol,
			ContainerPort: p.Port,
		}
	})
}

// mergeModelDeploymentEnv folds the three tiers into one environment list.
//
// The order of the result is the order of authority, most authoritative first, so that reading a
// rendered Pod answers "who set this" without consulting the table: what the operator OWNS, then
// what it merely DEFAULTS, then what the user appended, then the user's template overlay. Owned
// entries are refused at admission, so skipping them here is belt and braces rather than the
// enforcement — but a renderer that let one through would hand the failure to the engine, one layer
// away from the field that caused it.
//
// A role that takes over the command line gets none of the operator's entries. Its argv never names
// the file they point at, so setting them would describe a configuration nothing reads.
func mergeModelDeploymentEnv(
	engine string, role *workercore.ModelDeploymentRole,
	connector ModelDeploymentConnectorRender, takeOver bool,
) []core.EnvVar {
	userEnv := make([]workercore.InstanceEnvVar, 0, len(role.Env))
	userEnv = append(userEnv, role.Env...)
	if role.Template != nil {
		userEnv = append(userEnv, role.Template.Env...)
	}

	if takeOver {
		return mergeModelDeploymentUserEnv(nil, engine, userEnv)
	}

	env := make([]core.EnvVar, 0, len(connector.Env)+len(connector.DefaultedEnv)+len(userEnv))
	env = append(env, connector.Env...)
	for _, e := range connector.DefaultedEnv {
		if modelDeploymentUserSetsEnv(userEnv, e.Name) {
			continue
		}
		env = append(env, e)
	}

	return mergeModelDeploymentUserEnv(env, engine, userEnv)
}

// mergeModelDeploymentUserEnv appends the user's entries to what the operator already rendered,
// letting a later tier replace an earlier one by name and never replacing what the operator owns.
func mergeModelDeploymentUserEnv(
	env []core.EnvVar, engine string, userEnv []workercore.InstanceEnvVar,
) []core.EnvVar {
	for i := range userEnv {
		if ModelDeploymentOwnsEnv(engine, userEnv[i].Name) {
			continue
		}
		replaced := false
		for j := range env {
			if env[j].Name != userEnv[i].Name {
				continue
			}
			env[j].Value = userEnv[i].Value
			replaced = true

			break
		}
		if !replaced {
			env = append(env, core.EnvVar{Name: userEnv[i].Name, Value: userEnv[i].Value})
		}
	}

	return env
}

// modelDeploymentUserSetsEnv reports whether the user supplied the named variable in either tier.
func modelDeploymentUserSetsEnv(userEnv []workercore.InstanceEnvVar, name string) bool {
	for i := range userEnv {
		if userEnv[i].Name == name {
			return true
		}
	}

	return false
}

// deriveModelDeploymentResources turns a role's accelerator request into the full resource request
// one replica makes.
//
// CPU, memory and ephemeral storage are DERIVED here because they are not expressible on the role
// at all. The Instance path derives the same values in its mutating webhook and writes them onto the
// object; a ModelDeployment has no mutating webhook, so the derivation happens at render time and
// the values live only on the Pod. The arithmetic is the same one, restricted to the case that is
// the only one reachable here — nothing declared — so an InstanceType's per-unit resources scaled by
// the requested share is the whole of it.
//
// A request that cannot be sized is an error rather than a fallback. Sizing a partition or a slice
// as a whole card would charge quota for something other than what runs, and the state that causes
// it (an accelerator detail not computed yet) clears on its own, so the caller can retry.
//
// It takes no overcommit flag, unlike the webhook path it mirrors. Overcommit decides whether a
// value a user DECLARED is recomputed, and nothing is declared here; the request-versus-limit split
// it also drives is applied downstream by getResourceRequirements.
func deriveModelDeploymentResources(
	role *workercore.ModelDeploymentRole,
	instType *worker.InstanceType,
) (*workercore.InstanceResources, error) {
	ress := new(workercore.InstanceResources)
	if rr := role.Resources; rr != nil {
		ress.Accelerator = rr.Accelerator
		ress.AcceleratorSlicedMemoryPercentage = rr.AcceleratorSlicedMemoryPercentage
		ress.AcceleratorSlicedCoresPercentage = rr.AcceleratorSlicedCoresPercentage
		ress.AcceleratorPartitionedProfile = rr.AcceleratorPartitionedProfile
	}

	if err := sizeModelDeploymentHostResources(ress, instType); err != nil {
		return nil, err
	}

	// Default the local storage the way the Instance webhook does: 15Gi, never above what the
	// InstanceType offers.
	def := resource.NewQuantity(15<<30, resource.BinarySI) // 15Gi
	if instType.Spec.LocalStorage != "" {
		if maxStg, err := resource.ParseQuantity(instType.Spec.LocalStorage); err == nil && def.Cmp(maxStg) > 0 {
			def = &maxStg
		}
	}
	ress.LocalStorage = *def

	return ress, nil
}

// sizeModelDeploymentHostResources fills the CPU and memory a replica asks of its host.
func sizeModelDeploymentHostResources(
	ress *workercore.InstanceResources, instType *worker.InstanceType,
) error {
	if !instType.Spec.Acceleratable {
		// A non-accelerated pool sizes one unit's worth of host resources, matching what the
		// Instance path gives a request that declares no CPU of its own.
		cpu, ram, err := modelDeploymentUnitResources(instType, 1)
		if err != nil {
			return err
		}
		ress.CPU, ress.RAM = cpu, ram

		return nil
	}

	slicing := ress.AcceleratorSlicedMemoryPercentage != 0 || ress.AcceleratorSlicedCoresPercentage != 0
	partitioning := ress.AcceleratorPartitionedProfile != ""
	if (slicing || partitioning) && !instType.Status.Detail.AcceleratorReady() {
		// The share of a card being asked for cannot be read yet. This clears once the detail is
		// computed, so it is a retryable error and never a whole-card fallback.
		return fmt.Errorf("instance type %s is not ready yet (accelerator detail not computed); retry", instType.Name)
	}

	partitionPct, sizeable := PartitionProfileMemoryPercent((*workercore.InstanceType)(instType),
		ress.AcceleratorPartitionedProfile)
	if !sizeable {
		return fmt.Errorf("instance type %s is not ready yet (partition profile %q cannot be sized "+
			"from the observed accelerator detail); retry", instType.Name, ress.AcceleratorPartitionedProfile)
	}

	switch {
	case partitionPct > 0:
		// A hardware partition holds a share of ONE card, so the host resources follow the share of
		// that card's VRAM the profile occupies — the same VRAM-anchored fraction a logical slice
		// uses, so a partition and a slice of equal size cost equal host resources.
		return modelDeploymentSizeByPercent(ress, instType, partitionPct)
	case instType.Status.Detail.IsLogicallySliceable() && slicing:
		// When only one of the two percentages is set, the other follows it, so a bare memory
		// request yields an equal compute share and the reverse.
		switch {
		case ress.AcceleratorSlicedMemoryPercentage > 0 && ress.AcceleratorSlicedCoresPercentage == 0:
			ress.AcceleratorSlicedCoresPercentage = ress.AcceleratorSlicedMemoryPercentage
		case ress.AcceleratorSlicedCoresPercentage > 0 && ress.AcceleratorSlicedMemoryPercentage == 0:
			ress.AcceleratorSlicedMemoryPercentage = ress.AcceleratorSlicedCoresPercentage
		}
		// The memory percentage is the fraction of the card actually reserved; the compute
		// percentage throttles GPU cores and not host resources.
		return modelDeploymentSizeByPercent(ress, instType, int64(ress.AcceleratorSlicedMemoryPercentage))
	}

	// A whole-card request scales the unit resources by the card count. A replica asking for no
	// accelerator at all — legitimate for a small model on an accelerated pool — still needs a host
	// to run on, and is sized as one unit rather than as nothing.
	cards := int64(1)
	if ress.Accelerator != nil && ress.Accelerator.Value() > 0 {
		cards = ress.Accelerator.Value()
	}
	cpu, ram, err := modelDeploymentUnitResources(instType, cards)
	if err != nil {
		return err
	}
	ress.CPU, ress.RAM = cpu, ram

	return nil
}

// modelDeploymentSizeByPercent sizes the host resources as a percentage of one card's worth.
func modelDeploymentSizeByPercent(
	ress *workercore.InstanceResources, instType *worker.InstanceType, pct int64,
) error {
	cpu, err := quantityx.StringPercentMultiply(instType.Spec.UnitResources.CPU, pct)
	if err != nil {
		return fmt.Errorf("invalid CPU unit of instance type %s: %w", instType.Name, err)
	}
	ram, err := quantityx.StringPercentMultiply(instType.Spec.UnitResources.RAM, pct)
	if err != nil {
		return fmt.Errorf("invalid RAM unit of instance type %s: %w", instType.Name, err)
	}
	ress.CPU, ress.RAM = cpu, ram

	return nil
}

// modelDeploymentUnitResources scales one card's worth of host resources by a whole-card count.
func modelDeploymentUnitResources(
	instType *worker.InstanceType, multiplier int64,
) (cpu, ram resource.Quantity, err error) {
	cpu, err = quantityx.StringMultiply(instType.Spec.UnitResources.CPU, multiplier)
	if err != nil {
		return cpu, ram, fmt.Errorf("invalid CPU unit of instance type %s: %w", instType.Name, err)
	}
	ram, err = quantityx.StringMultiply(instType.Spec.UnitResources.RAM, multiplier)
	if err != nil {
		return cpu, ram, fmt.Errorf("invalid RAM unit of instance type %s: %w", instType.Name, err)
	}

	return cpu, ram, nil
}

// modelDeploymentPodSpecHash fingerprints a rendered replica.
//
// It covers the labels, the annotations and the spec, because a change to any of them is a change
// the running replica does not have: the entrance label decides which pool admits it, and the
// resource note decides which watch sees it. The hash annotation itself is stripped, so the
// fingerprint never covers itself — a value that did would change on every render and roll the
// deployment forever.
func modelDeploymentPodSpecHash(pod *core.Pod) string {
	annotations := make(map[string]string, len(pod.Annotations))
	for k, v := range pod.Annotations {
		if k == modelDeploymentPodSpecHashAnnotation {
			continue
		}
		annotations[k] = v
	}

	subject := struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Spec        core.PodSpec      `json:"spec"`
	}{
		Labels:      pod.Labels,
		Annotations: annotations,
		Spec:        pod.Spec,
	}

	// JSON rather than fmt: it orders map keys, so two renders of one spec hash identically. A
	// marshal failure is not reachable for a PodSpec, and treating it as an empty digest would
	// silently disable every rollout, so it is surfaced as a value nothing can match.
	encoded, err := json.Marshal(subject)
	if err != nil {
		return "unhashable-" + err.Error()
	}

	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:])
}
