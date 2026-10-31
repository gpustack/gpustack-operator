package modelstore

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

type coverageCountingReader struct {
	ctrlcli.Reader
	reads map[string]int
	fail  string
}

func (r *coverageCountingReader) Get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
	r.reads[key.Name]++
	if key.Name == r.fail {
		return errors.New("store read unavailable")
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestBindingCoversNodesReadsEachStoreOnce(t *testing.T) {
	nodes := []core.Node{
		{ObjectMeta: meta.ObjectMeta{Name: "node-a", Labels: map[string]string{"pool": "a"}}},
		{ObjectMeta: meta.ObjectMeta{Name: "node-b", Labels: map[string]string{"pool": "b"}}},
	}
	for _, tc := range []struct {
		name    string
		refs    []string
		fail    string
		want    map[string]bool
		reads   map[string]int
		wantErr bool
	}{
		{"both selectors", []string{"a", "b"}, "", map[string]bool{"node-a": true, "node-b": true}, map[string]int{"a": 1, "b": 1}, false},
		{"duplicate and missing refs", []string{"a", "a", "missing", "b"}, "", map[string]bool{"node-a": true, "node-b": true}, map[string]int{"a": 1, "missing": 1, "b": 1}, false},
		{"error discards partial coverage", []string{"a", "b"}, "b", nil, map[string]int{"a": 1, "b": 1}, true},
		{"no grant", nil, "", map[string]bool{}, map[string]int{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := &workercore.ModelStoreBinding{}
			for _, ref := range tc.refs {
				binding.Spec.StoreRefs = append(binding.Spec.StoreRefs, workercore.ModelStoreBindingStoreReference{Name: ref})
			}
			objects := make([]ctrlcli.Object, 0, 2)
			for _, pool := range []string{"a", "b"} {
				objects = append(objects, &workercore.ModelStore{
					ObjectMeta: meta.ObjectMeta{Name: pool},
					Spec:       workercore.ModelStoreSpec{NodeSelector: meta.LabelSelector{MatchLabels: map[string]string{"pool": pool}}},
				})
			}
			reader := &coverageCountingReader{
				Reader: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build(),
				reads:  map[string]int{}, fail: tc.fail,
			}
			covered, err := BindingCoversNodes(context.Background(), reader, binding, nodes)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, covered)
			assert.Equal(t, tc.reads, reader.reads)
		})
	}
}
