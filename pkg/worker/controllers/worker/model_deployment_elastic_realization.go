package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const (
	modelDeploymentElasticSharedMemoryPath   = "/dev/shm"
	modelDeploymentElasticSharedMemoryVolume = "elastic-shm"
	modelDeploymentElasticSharedMemorySize   = "512Mi"
)

// ModelDeploymentElasticTensorParallelSize reads the fixed TP degree from extraArgs.
// Non-elastic roles retain their ordinary accelerator default of one.
func ModelDeploymentElasticTensorParallelSize(role *workercore.ModelDeploymentRole) (int, error) {
	if role == nil || role.ElasticEP == nil {
		return 1, nil
	}
	declared, err := ParseModelDeploymentDeclaredParallelism(workercore.ModelDeploymentEngineVLLM, role.ExtraArgs, role.Env)
	if err != nil {
		return 0, fmt.Errorf("elastic member parallelism is unreadable: %w", err)
	}
	return declared.TensorParallel, nil
}

func modelDeploymentElasticHeadName(md *workercore.ModelDeployment) string {
	return md.Name + "-elastic-head"
}

func modelDeploymentElasticMasters(md *workercore.ModelDeployment, pods []core.Pod) []core.Pod {
	role := ModelDeploymentElasticRole(md)
	var masters []core.Pod
	for i := range pods {
		if modelDeploymentPodRole(&pods[i]) == role.Name && modelDeploymentOrdinalOrFloor(&pods[i]) == 0 {
			masters = append(masters, pods[i])
		}
	}
	return masters
}

func (r *ModelDeploymentReconciler) renderModelDeploymentElasticPods(ctx context.Context, md *workercore.ModelDeployment,
	connection *ModelDeploymentConnectorInput, protocols []string, weights *modelArtifactWeights,
) (map[string]map[int][]*core.Pod, error) {
	role := ModelDeploymentElasticRole(md)
	tpSize, err := ModelDeploymentElasticTensorParallelSize(role)
	if err != nil {
		return nil, err
	}
	boot := role.ElasticEP.Width
	pods, err := r.listModelDeploymentPods(ctx, md)
	if err != nil {
		return nil, err
	}
	masters := modelDeploymentElasticMasters(md, pods)
	if len(masters) > 1 {
		return nil, fmt.Errorf("elastic deployment has multiple master identities")
	}
	if len(masters) == 1 {
		if masters[0].UID == "" {
			return map[string]map[int][]*core.Pod{}, nil
		}
		var valid bool
		boot, valid = ModelDeploymentElasticBootstrapFromMaster(
			string(masters[0].UID), masters[0].Annotations[ModelDeploymentElasticMasterBootWidthAnnotation], md,
		)
		if !valid {
			return map[string]map[int][]*core.Pod{}, nil
		}
	}
	copyMD := md.DeepCopy()
	gpu := copyMD.Spec.Roles[0].DeepCopy()
	gpu.ElasticEP = nil
	gpu.Replicas = role.ElasticEP.Width
	for _, env := range []workercore.ModelDeploymentEnvVar{
		{Name: "RAY_LOG_TO_STDERR", Value: "1"},
		{Name: "RAY_LOGGER_LEVEL", Value: "info"},
		{Name: "RAY_BACKEND_LOG_LEVEL", Value: "info"},
	} {
		if !slices.ContainsFunc(gpu.Env, func(value workercore.ModelDeploymentEnvVar) bool { return value.Name == env.Name }) {
			gpu.Env = append(gpu.Env, env)
		}
	}
	copyMD.Spec.Roles = []workercore.ModelDeploymentRole{*gpu}
	desired, err := r.renderModelDeploymentPods(ctx, copyMD, connection, protocols, weights)
	if err != nil {
		return nil, err
	}
	address := modelDeploymentElasticHeadName(md) + "." + md.Namespace + ".svc:6379"
	for ordinal, members := range desired[role.Name] {
		pod := members[0]
		c := &pod.Spec.Containers[0]
		c.Env = append(c.Env,
			core.EnvVar{Name: "GPUSTACK_ELASTIC_POD_UID", ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.uid"},
			}},
			core.EnvVar{Name: "RAY_ADDRESS", Value: address},
			core.EnvVar{Name: "GPUSTACK_ELASTIC_POD_IP", ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{FieldPath: "status.podIP"},
			}},
			core.EnvVar{Name: "VLLM_RAY_DP_PACK_STRATEGY", Value: "strict"},
		)
		tp := strconv.Itoa(tpSize)
		join := "ray start --node-ip-address=\"$GPUSTACK_ELASTIC_POD_IP\" --address=" + address +
			" --num-gpus=" + tp + " --labels=" + ModelDeploymentElasticRayNodeIdentityLabel + "=\"$GPUSTACK_ELASTIC_POD_UID\""

		if ordinal == 0 {
			argv := slices.Clone(c.Command)
			argv = append(argv, "--data-parallel-backend", "ray", "--data-parallel-size", strconv.Itoa(int(boot)),
				"--data-parallel-size-local", "1",
				"--enable-expert-parallel", "--enable-eplb", "--enable-elastic-ep")
			c.Command = append([]string{"/bin/sh", "-c", "set -e; " + join +
				"; exec \"$@\" --data-parallel-address \"$GPUSTACK_ELASTIC_POD_IP\"", "elastic-serve"}, argv...)
			pod.Annotations[ModelDeploymentElasticMasterBootWidthAnnotation] = strconv.Itoa(int(boot))
			c.LivenessProbe = modelDeploymentElasticLivenessProbe()
			port := pod.Annotations["prometheus.io/port"]
			if port != "" {
				c.LivenessProbe.Exec.Command[2] = modelDeploymentElasticLivenessScript("http://127.0.0.1:" + port + modelDeploymentProbePath)
			}
		} else {
			c.Command = []string{"/bin/sh", "-ec", join + " --block"}
			c.StartupProbe = nil
			c.ReadinessProbe = nil
			c.LivenessProbe = nil
			c.Lifecycle = nil
		}
		modelDeploymentElasticSharedMemory(pod)
		pod.Annotations[modelDeploymentPodSpecHashAnnotation] = modelDeploymentPodSpecHash(pod)
	}
	head := gpu.DeepCopy()
	head.Name = modelDeploymentElasticHeadName(md)
	head.InstanceType = role.ElasticEP.HeadInstanceType
	head.Resources = nil
	head.Replicas = 1
	head.ExtraArgs = nil
	head.Ports = nil
	head.Command = []string{"/bin/sh", "-ec", ModelDeploymentElasticHeadCommand +
		" --num-gpus=0 --port=6379 --dashboard-host=0.0.0.0 --labels=" +
		ModelDeploymentElasticRayNodeIdentityLabel + "=\"$GPUSTACK_ELASTIC_POD_UID\" --block"}
	head.Env = slices.DeleteFunc(head.Env, func(env workercore.ModelDeploymentEnvVar) bool {
		switch env.Name {
		case "RAY_LOG_TO_STDERR", "RAY_LOGGER_LEVEL", "RAY_BACKEND_LOG_LEVEL", "RAY_DEDUP_LOGS":
			return false
		default:
			return true
		}
	})
	headMD := md.DeepCopy()
	headMD.Spec.Roles = []workercore.ModelDeploymentRole{*head}
	heads, err := r.renderModelDeploymentPods(ctx, headMD, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	headPod := heads[head.Name][0][0]
	headPod.Spec.Containers[0].Env = append(headPod.Spec.Containers[0].Env, core.EnvVar{
		Name: "GPUSTACK_ELASTIC_POD_UID", ValueFrom: &core.EnvVarSource{
			FieldRef: &core.ObjectFieldSelector{FieldPath: "metadata.uid"},
		},
	})
	headPod.Annotations[modelDeploymentPodSpecHashAnnotation] = modelDeploymentPodSpecHash(headPod)
	desired[head.Name] = heads[head.Name]
	return desired, nil
}

// modelDeploymentElasticSharedMemory gives each GPU member an isolated shared-memory volume.
// Preserve an explicit mount, whose backing and capacity remain the user's responsibility.
func modelDeploymentElasticSharedMemory(pod *core.Pod) {
	if len(pod.Spec.Containers) == 0 {
		return
	}
	container := &pod.Spec.Containers[0]
	for _, m := range container.VolumeMounts {
		if m.MountPath == modelDeploymentElasticSharedMemoryPath {
			return
		}
	}
	size := resource.MustParse(modelDeploymentElasticSharedMemorySize)
	pod.Spec.Volumes = append(pod.Spec.Volumes, core.Volume{
		Name: modelDeploymentElasticSharedMemoryVolume,
		VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{
			Medium:    core.StorageMediumMemory,
			SizeLimit: &size,
		}},
	})
	container.VolumeMounts = append(container.VolumeMounts, core.VolumeMount{
		Name:      modelDeploymentElasticSharedMemoryVolume,
		MountPath: modelDeploymentElasticSharedMemoryPath,
	})
}

func renderModelDeploymentElasticHeadService(md *workercore.ModelDeployment) *core.Service {
	name := modelDeploymentElasticHeadName(md)
	svc := &core.Service{
		ObjectMeta: meta.ObjectMeta{
			Name: name, Namespace: md.Namespace,
			Labels: map[string]string{modelDeploymentLabelKeyName: modelDeploymentLabelValueName, modelDeploymentLabelKeyInstance: md.Name},
		},
		Spec: core.ServiceSpec{
			Selector: map[string]string{modelDeploymentLabelKeyInstance: md.Name, modelDeploymentLabelKeyComponent: name},
			Ports: []core.ServicePort{
				{Name: "gcs", Port: 6379, TargetPort: intstr.FromInt32(6379)},
				{Name: "dashboard", Port: 8265, TargetPort: intstr.FromInt32(8265)},
			},
		},
	}
	systemmeta.NoteResource(svc, ModelDeploymentResourceType, map[string]string{ModelDeploymentResourceNoteRole: name})
	kubemeta.ControlOnWithoutBlock(svc, md, workercore.SchemeGroupVersionKind("ModelDeployment"))
	return svc
}

// modelDeploymentTerminalPods returns the members that have already left and that nothing is
// deleting: a Succeeded or Failed Pod carrying no deletion timestamp. A failed Pod never runs again
// and Kubernetes never garbage-collects one, so a departure no other path initiates stays behind as an
// object, with its Workload and its share of the group's quota still held. A Pod already on its way
// out is not returned, because its departure is in flight and its Workload is released by the
// protocol that issued the delete.
func modelDeploymentTerminalPods(pods []core.Pod) []core.Pod {
	terminal := make([]core.Pod, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase != core.PodSucceeded && pod.Status.Phase != core.PodFailed {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		terminal = append(terminal, *pod)
	}

	return terminal
}

// deleteModelDeploymentTerminalPods removes the members that have already left and releases the
// Workloads that hold them. Both writes are needed: the Pod delete frees the ordinal the create gate
// reads, and the Workload delete is what releases Kueue's finalizer on the member and the quota its
// group held. Absence is success on both, because a pass may follow one that already issued them.
func (r *ModelDeploymentReconciler) deleteModelDeploymentTerminalPods(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) error {
	terminal := modelDeploymentTerminalPods(pods)
	if len(terminal) == 0 {
		return nil
	}
	for i := range terminal {
		logger := ctrllog.FromContext(ctx).WithValues("pod", terminal[i].Name,
			"phase", terminal[i].Status.Phase, "reason", terminal[i].Status.Reason)
		logger.Info("removing elastic member in a terminal phase")
		if err := r.Client.Delete(ctx, &terminal[i]); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete terminal elastic member %s: %w", terminal[i].Name, err)
		}
	}

	return r.deleteModelDeploymentGroupWorkload(ctx, md, terminal)
}

func (r *ModelDeploymentReconciler) convergeModelDeploymentElastic(ctx context.Context, md *workercore.ModelDeployment,
	actual []core.Pod, desired map[string]map[int][]*core.Pod, weights *modelArtifactWeights,
) (bool, error) {
	role := ModelDeploymentElasticRole(md)
	if role == nil {
		return false, nil
	}
	// A TERMINAL MEMBER IS A DEPARTURE THIS OPERATOR DID NOT INITIATE, and the elastic path is the
	// only path that can leave one behind: the fixed desired-set comparison below, which deletes
	// terminal replicas and their Workloads, is behind this call. Kueue never garbage-collects a
	// Succeeded or Failed Pod, and the create loop further down deliberately reads a terminal member
	// as absent so its ordinal can be filled again. Left alone it stays an admitted member of the
	// group it shares with its own replacement, which Kueue reads as the excess member and deletes
	// again and again, so the slot never heals. The delete and the Workload that releases the
	// group's finalizer and quota are issued here instead, on the same terms the fixed path uses.
	if err := r.deleteModelDeploymentTerminalPods(ctx, md, actual); err != nil {
		return true, err
	}
	// A MEMBER ALREADY ON ITS WAY OUT IS THE OTHER DEPARTURE NOTHING HERE SENT AWAY, and the fixed
	// path's stranded sweep never reaches this branch, so the sweep runs here. Its Workload is what
	// holds Kueue's finalizer on the Pod and the group's quota, and the sweep keeps the fixed
	// path's condition: a Workload that also owns a member still standing is left alone, because
	// Kueue answers a deleted Workload by stopping the whole group.
	if err := r.releaseModelDeploymentStrandedWorkloads(ctx, md, actual); err != nil {
		return true, err
	}
	if len(desired) == 0 {
		return true, r.reconcileModelDeploymentElasticResize(ctx, md)
	}
	for name, ordinals := range desired {
		taken, err := r.modelDeploymentTakenGroups(ctx, md, name, false)
		if err != nil {
			return true, err
		}
		gpuCount := 0
		for i := range actual {
			if actual[i].Status.Phase == core.PodSucceeded || actual[i].Status.Phase == core.PodFailed {
				continue
			}
			if modelDeploymentPodRole(&actual[i]) == role.Name {
				gpuCount++
			}
		}
		for ordinal, members := range ordinals {
			if name == role.Name && gpuCount >= int(role.ElasticEP.Width) {
				continue
			}
			present := false
			for i := range actual {
				if actual[i].Status.Phase == core.PodSucceeded || actual[i].Status.Phase == core.PodFailed {
					continue
				}
				if modelDeploymentPodRole(&actual[i]) == name && modelDeploymentOrdinalOrFloor(&actual[i]) == ordinal {
					present = true
					break
				}
			}
			if present || taken.Has(modelDeploymentReplicaGroupName(md, name, ordinal)) {
				continue
			}
			pod := members[0].DeepCopy()
			if weights != nil && name == role.Name {
				injectModelArtifactAffinity(pod, weights.Affinity)
			}
			if err := r.Client.Create(ctx, pod); err != nil {
				return true, err
			}
			if name == role.Name {
				gpuCount++
			}
		}
	}
	if err := r.reconcileModelDeploymentElasticResize(ctx, md); err != nil {
		return true, err
	}
	if r.APIReader != nil {
		live, err := r.elasticLiveMembers(ctx, md)
		switch {
		case err == nil:
			actual = live
		case errors.Is(err, errElasticDeploymentChanged):
			// The deployment moved under this pass. The writes below would label the master, patch
			// the Service and rewrite router state from a member list this pass can no longer vouch
			// for, so the pass stops here. The spec watch requeues the deployment.
			return true, nil
		default:
			// A read that failed is not a member list. Refuse the writes rather than converge from
			// the caller's list, which is the one path that could confirm what it never observed.
			return true, err
		}
	}
	fetch := r.groupForwardFetch
	if fetch == nil {
		fetch = defaultGroupForwardFetch
	}
	masters := modelDeploymentElasticMasters(md, actual)
	qualifications := qualifyModelDeploymentInstances(ctx, md, masters, modelDeploymentPendingReplacement{}, fetch)
	byMember := modelDeploymentQualificationsByMember(qualifications)
	if err := r.convergeModelDeploymentEndpointEligibility(ctx, md, actual, byMember); err != nil {
		return true, err
	}
	decided := modelDeploymentQualificationsDecided(qualifications)
	if err := r.syncModelDeploymentService(ctx, md, decided); err != nil {
		return true, err
	}
	return true, r.syncModelDeploymentRouter(ctx, md, decided)
}
