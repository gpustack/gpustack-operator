package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/modelstore"
)

func TestDecodeKubeletConfigz(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    *workercore.NodeModelStoreKubelet
		wantErr bool
	}{
		{
			name: "a node setting nodefs only, as a managed node does",
			raw: `{"kubeletconfig":{"evictionHard":{"memory.available":"100Mi","nodefs.available":"10%",` +
				`"nodefs.inodesFree":"5%","pid.available":"10%"},"imageGCHighThresholdPercent":85,"imageGCLowThresholdPercent":80}}`,
			want: &workercore.NodeModelStoreKubelet{NodefsAvailable: "10%", ImageGCHighThresholdPercent: ptr.To[int32](85)},
		},
		{
			name: "both signals, one a quantity",
			raw:  `{"kubeletconfig":{"evictionHard":{"nodefs.available":"20Gi","imagefs.available":"15%"},"imageGCHighThresholdPercent":90}}`,
			want: &workercore.NodeModelStoreKubelet{NodefsAvailable: "20Gi", ImagefsAvailable: "15%", ImageGCHighThresholdPercent: ptr.To[int32](90)},
		},
		{name: "no eviction thresholds", raw: `{"kubeletconfig":{}}`, want: &workercore.NodeModelStoreKubelet{}},
		{name: "not the configz shape", raw: `{"apiVersion":"v1"}`, wantErr: true},
		{name: "not JSON", raw: `<html>`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decodeKubeletConfigz([]byte(c.raw))
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestNodeModelStoreReconcileWritesTheKubeletThresholds(t *testing.T) {
	reading := &workercore.NodeModelStoreKubelet{NodefsAvailable: "10%", ImageGCHighThresholdPercent: ptr.To[int32](85)}
	errUnreachable := errors.New("kubelet unreachable")

	cases := []struct {
		name string
		// reads are the reader's answers in order; each entry is one reconcile, after advance.
		reads   []error
		advance []time.Duration
		want    []*workercore.NodeModelStoreKubelet
		// wantRequeue is each reconcile's requeue: the refresh after a reading, the retry after a
		// failed read.
		wantRequeue []time.Duration
		// wantCalls is how many reads happened in all.
		wantCalls int
	}{
		{
			name: "a reading is written", reads: []error{nil}, advance: []time.Duration{0},
			want: []*workercore.NodeModelStoreKubelet{reading}, wantRequeue: []time.Duration{nodeModelStoreKubeletRefresh}, wantCalls: 1,
		},
		{
			name: "no reading leaves it absent and retries soon", reads: []error{errUnreachable}, advance: []time.Duration{0},
			want: []*workercore.NodeModelStoreKubelet{nil}, wantRequeue: []time.Duration{nodeModelStoreKubeletRetry}, wantCalls: 1,
		},
		{
			name: "a reading is not repeated within its refresh", reads: []error{nil, nil}, advance: []time.Duration{0, time.Minute},
			want:        []*workercore.NodeModelStoreKubelet{reading, reading},
			wantRequeue: []time.Duration{nodeModelStoreKubeletRefresh, nodeModelStoreKubeletRefresh},
			wantCalls:   1,
		},
		{
			name: "a failed refresh keeps the last reading and retries soon", reads: []error{nil, errUnreachable}, advance: []time.Duration{0, 31 * time.Minute},
			want:        []*workercore.NodeModelStoreKubelet{reading, reading},
			wantRequeue: []time.Duration{nodeModelStoreKubeletRefresh, nodeModelStoreKubeletRetry},
			wantCalls:   2,
		},
		{
			name: "a healed read lands at the retry", reads: []error{errUnreachable, nil}, advance: []time.Duration{0, time.Minute},
			want:        []*workercore.NodeModelStoreKubelet{nil, reading},
			wantRequeue: []time.Duration{nodeModelStoreKubeletRetry, nodeModelStoreKubeletRefresh},
			wantCalls:   2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, cli := newTestNodeModelStoreEnv(t,
				testModelCSIDriver(), testModelNode("node-1"), testCSINode("node-1", modelstore.DriverName))
			now := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
			r.Now = func() time.Time { return now }
			calls := 0
			r.ReadKubelet = func(context.Context, string) (*workercore.NodeModelStoreKubelet, error) {
				err := c.reads[calls]
				calls++
				if err != nil {
					return nil, err
				}
				return reading.DeepCopy(), nil
			}
			for i := range c.want {
				now = now.Add(c.advance[i])
				res := reconcileNodeModelStore(t, r, "node-1")
				assert.Equal(t, c.wantRequeue[i], res.RequeueAfter, "reconcile %d", i)
				got := new(workercore.NodeModelStore)
				require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
				assert.Equal(t, c.want[i], got.Spec.Kubelet, "reconcile %d", i)
			}
			assert.Equal(t, c.wantCalls, calls)
		})
	}
}

func TestReadKubeletBoundsAHangingKubelet(t *testing.T) {
	r := &NodeModelStoreReconciler{ReadKubelet: func(ctx context.Context, _ string) (*workercore.NodeModelStoreKubelet, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	start := time.Now()
	_, err := r.readKubelet(context.Background(), "node-1")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), nodeModelStoreKubeletTimeout+2*time.Second)
}

// TestReadKubeletConfigz pins the node-proxy read against an API server that answers, refuses and
// hangs.
func TestReadKubeletConfigz(t *testing.T) {
	answer := `{"kubeletconfig":{"evictionHard":{"nodefs.available":"10%"},"imageGCHighThresholdPercent":85}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/nodes/answers/proxy/configz":
			_, _ = w.Write([]byte(answer))
		case "/api/v1/nodes/hangs/proxy/configz":
			<-r.Context().Done()
		default:
			http.Error(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":403}`, http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	cli, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	require.NoError(t, err)
	core := cli.CoreV1().RESTClient()

	got, err := readKubeletConfigz(context.Background(), core, "answers")
	require.NoError(t, err)
	assert.Equal(t, &workercore.NodeModelStoreKubelet{NodefsAvailable: "10%", ImageGCHighThresholdPercent: ptr.To[int32](85)}, got)

	_, err = readKubeletConfigz(context.Background(), core, "refuses")
	require.Error(t, err)

	r := &NodeModelStoreReconciler{ReadKubelet: func(ctx context.Context, node string) (*workercore.NodeModelStoreKubelet, error) {
		return readKubeletConfigz(ctx, core, node)
	}}
	start := time.Now()
	_, err = r.readKubelet(context.Background(), "hangs")
	require.Error(t, err)
	assert.Less(t, time.Since(start), nodeModelStoreKubeletTimeout+2*time.Second)
}
