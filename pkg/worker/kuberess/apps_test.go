package kuberess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
)

// workerPod builds the Pod a lock holder names, with its container in the given state.
func workerPod(name string, running bool) *core.Pod {
	state := core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 137}}
	if running {
		state = core.ContainerState{Running: &core.ContainerStateRunning{}}
	}

	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Namespace: SystemNamespaceName, Name: name},
		Status: core.PodStatus{
			ContainerStatuses: []core.ContainerStatus{{Name: "main", State: state}},
		},
	}
}

// unreportedWorkerPod builds the Pod of a lock holder whose containers have not reported a
// state yet, which is the Pod the kubelet has said nothing about either way.
func unreportedWorkerPod(name string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Namespace: SystemNamespaceName, Name: name},
	}
}

// Test_predecessorHasStopped pins what a replica accepts as proof that the process holding
// the install lock before it is gone. Answering "stopped" while it is still applying puts
// two Helm actions on one release, so every case that cannot be checked answers "running".
func Test_predecessorHasStopped(t *testing.T) {
	testCases := []struct {
		name        string
		pods        []runtime.Object
		predecessor string
		want        bool
	}{
		{
			name:        "a lock found free had no predecessor to outlive",
			predecessor: "",
			want:        true,
		},
		{
			name:        "a pod that no longer exists took its process with it",
			predecessor: "pod/gpustack-operator-worker-0",
			want:        true,
		},
		{
			name:        "a pod whose container is not running has no process behind it",
			pods:        []runtime.Object{workerPod("gpustack-operator-worker-0", false)},
			predecessor: "pod/gpustack-operator-worker-0",
			want:        true,
		},
		{
			name:        "a pod whose containers have not reported answers neither way",
			pods:        []runtime.Object{unreportedWorkerPod("gpustack-operator-worker-0")},
			predecessor: "pod/gpustack-operator-worker-0",
			want:        false,
		},
		{
			name:        "a running pod may still be applying",
			pods:        []runtime.Object{workerPod("gpustack-operator-worker-0", true)},
			predecessor: "pod/gpustack-operator-worker-0",
			want:        false,
		},
		{
			name:        "a holder outside the cluster cannot be looked up",
			predecessor: "process/some-host-4242",
			want:        false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cli := kubefake.NewSimpleClientset(tc.pods...)
			assert.Equal(t, tc.want, predecessorHasStopped(t.Context(), cli, tc.predecessor))
		})
	}
}
