package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/rest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/system"
)

const (
	// nodeModelStoreKubeletRefresh is how often a node's kubelet thresholds are read again. They
	// change only with a kubelet restart, and a read that finds them unchanged writes nothing.
	nodeModelStoreKubeletRefresh = 30 * time.Minute
	// nodeModelStoreKubeletRetry is how soon a node whose kubelet could not be read is tried again:
	// a brief unreachability heals at the next retry rather than the next refresh.
	nodeModelStoreKubeletRetry = time.Minute
	// nodeModelStoreKubeletTimeout bounds one read, so a node whose kubelet does not answer delays
	// its spec's other fields by no more than this.
	nodeModelStoreKubeletTimeout = 5 * time.Second
)

// kubeletReading is a node's last successful read of its kubelet thresholds.
type kubeletReading struct {
	thresholds *workercore.NodeModelStoreKubelet
	readTime   time.Time
}

// kubeletThresholds returns the node's kubelet thresholds for its spec: the last reading while it is
// younger than the refresh, a new reading otherwise. A failed read keeps the last reading, or the
// value the object already carries after a worker restart, so a kubelet that does not answer for a
// moment does not flip the node's cap to kubelet's defaults and back — and reports false, so the
// caller tries again sooner than the refresh. A nil thresholds means none was ever read.
func (r *NodeModelStoreReconciler) kubeletThresholds(
	ctx context.Context, node string, current *workercore.NodeModelStoreKubelet,
) (thresholds *workercore.NodeModelStoreKubelet, read bool) {
	r.kubeletMu.Lock()
	last, ok := r.kubelet[node]
	r.kubeletMu.Unlock()
	if ok && r.now().Sub(last.readTime) < nodeModelStoreKubeletRefresh {
		return last.thresholds, true
	}

	thresholds, err := r.readKubelet(ctx, node)
	if err != nil {
		ctrllog.FromContext(ctx).Info("the node's effective kubelet configuration could not be read; "+
			"its model cache keeps the last reading or kubelet's defaults", "node", node, "error", err.Error())
		if ok {
			return last.thresholds, false
		}
		return current, false
	}

	r.kubeletMu.Lock()
	defer r.kubeletMu.Unlock()
	if r.kubelet == nil {
		r.kubelet = map[string]kubeletReading{}
	}
	r.kubelet[node] = kubeletReading{thresholds: thresholds, readTime: r.now()}

	return thresholds, true
}

// forgetKubelet drops a node's reading once its object is gone.
func (r *NodeModelStoreReconciler) forgetKubelet(node string) {
	r.kubeletMu.Lock()
	defer r.kubeletMu.Unlock()
	delete(r.kubelet, node)
}

func (r *NodeModelStoreReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// readKubelet reads the node's thresholds within nodeModelStoreKubeletTimeout.
func (r *NodeModelStoreReconciler) readKubelet(ctx context.Context, node string) (*workercore.NodeModelStoreKubelet, error) {
	ctx, cancel := context.WithTimeout(ctx, nodeModelStoreKubeletTimeout)
	defer cancel()
	if r.ReadKubelet != nil {
		return r.ReadKubelet(ctx, node)
	}

	cli := system.LoopbackKubeClient.Get()
	if cli == nil {
		return nil, errors.New("no loopback client")
	}

	return readKubeletConfigz(ctx, cli.CoreV1().RESTClient(), node)
}

// readKubeletConfigz reads the node's effective kubelet configuration through the API server's node
// proxy. configz is what kubelet enforces: its flags, configuration file and drop-ins merged. It is
// served while kubelet's debugging handlers are enabled, their default.
func readKubeletConfigz(ctx context.Context, core rest.Interface, node string) (*workercore.NodeModelStoreKubelet, error) {
	raw, err := core.Get().
		Resource("nodes").Name(node).SubResource("proxy").Suffix("configz").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the kubelet configz of node %s: %w", node, err)
	}

	return decodeKubeletConfigz(raw)
}

// decodeKubeletConfigz takes the thresholds the model cache's cap needs out of a configz answer.
func decodeKubeletConfigz(raw []byte) (*workercore.NodeModelStoreKubelet, error) {
	var z struct {
		KubeletConfig *struct {
			EvictionHard                map[string]string `json:"evictionHard"`
			ImageGCHighThresholdPercent *int32            `json:"imageGCHighThresholdPercent"`
		} `json:"kubeletconfig"`
	}
	if err := json.Unmarshal(raw, &z); err != nil {
		return nil, fmt.Errorf("decode the kubelet configz: %w", err)
	}
	if z.KubeletConfig == nil {
		return nil, errors.New("decode the kubelet configz: no kubeletconfig in the answer")
	}

	return &workercore.NodeModelStoreKubelet{
		NodefsAvailable:             z.KubeletConfig.EvictionHard["nodefs.available"],
		ImagefsAvailable:            z.KubeletConfig.EvictionHard["imagefs.available"],
		ImageGCHighThresholdPercent: z.KubeletConfig.ImageGCHighThresholdPercent,
	}, nil
}
