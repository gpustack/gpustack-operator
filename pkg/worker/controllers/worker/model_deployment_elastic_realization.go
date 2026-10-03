package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const (
	modelDeploymentElasticSharedMemoryPath   = "/dev/shm"
	modelDeploymentElasticSharedMemoryVolume = "elastic-shm"
	modelDeploymentElasticSharedMemorySize   = "512Mi"
)

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
		join := "ray start --node-ip-address=\"$GPUSTACK_ELASTIC_POD_IP\" --address=" + address +
			" --num-gpus=1 --labels=" + ModelDeploymentElasticRayNodeIdentityLabel + "=\"$GPUSTACK_ELASTIC_POD_UID\""

		if ordinal == 0 {
			argv := slices.Clone(c.Command)
			argv = append(argv, "--data-parallel-backend", "ray", "--data-parallel-size", strconv.Itoa(int(boot)),
				"--data-parallel-size-local", "1", "--tensor-parallel-size", "1", "--pipeline-parallel-size", "1",
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
	head.Env = nil
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

func (r *ModelDeploymentReconciler) convergeModelDeploymentElastic(ctx context.Context, md *workercore.ModelDeployment,
	actual []core.Pod, desired map[string]map[int][]*core.Pod, weights *modelArtifactWeights,
) (bool, error) {
	role := ModelDeploymentElasticRole(md)
	if role == nil {
		return false, nil
	}
	if len(desired) == 0 {
		return true, r.reconcileModelDeploymentElasticResize(ctx, md)
	}
	for name, ordinals := range desired {
		taken, err := r.modelDeploymentTakenGroups(ctx, md, name)
		if err != nil {
			return true, err
		}
		gpuCount := 0
		for i := range actual {
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
