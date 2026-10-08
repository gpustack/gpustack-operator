package worker

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/setting"
	workersettings "gpustack.ai/gpustack/pkg/worker/settings"
)

// settingsSecret is the Secret the cluster Settings live in, as the table reads it.
func settingsSecret(data map[string][]byte) *core.Secret {
	return &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Namespace: setting.DelegatedSecretNamespace,
			Name:      setting.DelegatedSecretName,
		},
		Data: data,
	}
}

// countingReader counts the Settings Secret reads a table conversion makes, and answers with the
// given Secret or the given failure, so a case can assert that the conversion reads the cluster
// values once per request rather than once per row.
type countingReader struct {
	client ctrlcli.WithWatch
	reads  int
}

func newCountingReader(t *testing.T, sec *core.Secret, getErr error) *countingReader {
	t.Helper()
	c := &countingReader{}
	builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme)
	// A case that has no Secret to answer with must not seed one, so the read finds nothing.
	if sec != nil {
		builder = builder.WithObjects(sec)
	}
	c.client = builder.
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
				c.reads++
				if getErr != nil {
					return getErr
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	return c
}

// reader is the read side the table is given: the Settings values are read, never written.
func (c *countingReader) reader() ctrlcli.Reader {
	return c.client
}

// modelStore builds a ModelStore with the given watermarks, nil meaning the pool states none and
// inherits the cluster's. It carries a node count so the Nodes column renders a value rather than
// an absent one, keeping each assertion about the watermark columns.
func modelStore(name string, high, low *int32) *worker.ModelStore {
	return &worker.ModelStore{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.ModelStoreSpec{
			Watermarks: watermarks(high, low),
		},
		Status: workercore.ModelStoreStatus{Nodes: 2},
	}
}

func watermarks(high, low *int32) *workercore.NodeModelStoreWatermarks {
	if high == nil && low == nil {
		return nil
	}
	w := &workercore.NodeModelStoreWatermarks{}
	if high != nil {
		w.HighPercent = *high
	}
	if low != nil {
		w.LowPercent = *low
	}
	return w
}

// tableCells converts the object and returns one slice of printed cells per row, so a case reads the
// table the way kubectl prints it.
func tableCells(t *testing.T, table interface {
	ConvertToTable(context.Context, runtime.Object, runtime.Object) (*meta.Table, error)
}, obj, opts runtime.Object,
) [][]string {
	t.Helper()
	got, err := table.ConvertToTable(context.Background(), obj, opts)
	require.NoError(t, err)

	rows := make([][]string, 0, len(got.Rows))
	for _, row := range got.Rows {
		cells := make([]string, 0, len(row.Cells))
		for _, c := range row.Cells {
			cells = append(cells, printCell(c))
		}
		rows = append(rows, cells)
	}
	return rows
}

// TestModelStoreTableShowsTheInheritedWatermarks pins that the table answers the pool's own cache
// policy rather than only what its spec restates: a pool stating no watermarks shows the cluster
// Settings', a pool stating some shows its own, and a missing Secret or key falls back to the
// Setting's default rather than to a zero. These are the pool's policy, not a report of what any
// node has already applied.
func TestModelStoreTableShowsTheInheritedWatermarks(t *testing.T) {
	testCases := []struct {
		name     string
		secret   *core.Secret
		store    *worker.ModelStore
		wantHigh string
		wantLow  string
	}{
		{
			name:     "a pool stating no watermarks shows the cluster's",
			secret:   settingsSecret(map[string][]byte{"model-store-high-watermark": []byte("88"), "model-store-low-watermark": []byte("55")}),
			store:    modelStore("inherit", nil, nil),
			wantHigh: "88",
			wantLow:  "55",
		},
		{
			name:     "a pool stating its watermarks shows its own, not the cluster's",
			secret:   settingsSecret(map[string][]byte{"model-store-high-watermark": []byte("88"), "model-store-low-watermark": []byte("55")}),
			store:    modelStore("own", ptr.To(int32(91)), ptr.To(int32(40))),
			wantHigh: "91",
			wantLow:  "40",
		},
		{
			name:     "a Secret that does not exist shows the Settings' defaults",
			secret:   nil,
			store:    modelStore("inherit", nil, nil),
			wantHigh: workersettings.ModelStoreHighWatermark.DefaultValue(),
			wantLow:  workersettings.ModelStoreLowWatermark.DefaultValue(),
		},
		{
			name:     "a Secret missing the keys shows each Setting's own default",
			secret:   settingsSecret(map[string][]byte{"unrelated-setting": []byte("x")}),
			store:    modelStore("inherit", nil, nil),
			wantHigh: workersettings.ModelStoreHighWatermark.DefaultValue(),
			wantLow:  workersettings.ModelStoreLowWatermark.DefaultValue(),
		},
		{
			name:     "a key the Secret does not carry falls back alone, not both",
			secret:   settingsSecret(map[string][]byte{"model-store-high-watermark": []byte("88")}),
			store:    modelStore("inherit", nil, nil),
			wantHigh: "88",
			wantLow:  workersettings.ModelStoreLowWatermark.DefaultValue(),
		},
		{
			name:     "an unrelated key in the Secret does not move either column",
			secret:   settingsSecret(map[string][]byte{"model-store-high-watermark": []byte("88"), "model-store-low-watermark": []byte("55"), "model-store-download-concurrency": []byte("invalid")}),
			store:    modelStore("inherit", nil, nil),
			wantHigh: "88",
			wantLow:  "55",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			table, err := modelStoreTable(newCountingReader(t, tc.secret, nil).reader())
			require.NoError(t, err)

			rows := tableCells(t, table, tc.store, nil)
			require.Len(t, rows, 1)
			assert.Equal(t, []string{tc.store.Name, "2", tc.wantHigh, tc.wantLow, "<unknown>"}, rows[0],
				"the table answers the pool's effective watermarks, not only what its spec states")
		})
	}
}

// TestModelStoreTableListSharesOneClusterReading pins that a list mixes inherited and stated pools
// correctly while reading the cluster values once: one Secret read serves every row, so the cost
// does not grow with the list.
func TestModelStoreTableListSharesOneClusterReading(t *testing.T) {
	reader := newCountingReader(t, settingsSecret(map[string][]byte{
		"model-store-high-watermark": []byte("88"),
		"model-store-low-watermark":  []byte("55"),
	}), nil)
	table, err := modelStoreTable(reader.reader())
	require.NoError(t, err)

	list := &worker.ModelStoreList{Items: []worker.ModelStore{
		*modelStore("a-inherit", nil, nil),
		*modelStore("b-own", ptr.To(int32(91)), ptr.To(int32(40))),
		*modelStore("c-inherit", nil, nil),
	}}
	rows := tableCells(t, table, list, nil)

	assert.Equal(t, [][]string{
		{"a-inherit", "2", "88", "55", "<unknown>"},
		{"b-own", "2", "91", "40", "<unknown>"},
		{"c-inherit", "2", "88", "55", "<unknown>"},
	}, rows, "each row answers its own pool's watermarks, the inherited ones from the cluster")
	assert.Equal(t, 1, reader.reads, "Secret reads for the whole list")
}

// TestModelStoreTableFollowsTheClusterReading pins that nothing is cached between requests: the
// table shows the values stored when it was asked, so a Settings edit reaches the next kubectl
// without a restart. The view's watch does not push a table update on its own.
func TestModelStoreTableFollowsTheClusterReading(t *testing.T) {
	sec := settingsSecret(map[string][]byte{"model-store-high-watermark": []byte("88"), "model-store-low-watermark": []byte("55")})
	reader := newCountingReader(t, sec, nil)
	table, err := modelStoreTable(reader.reader())
	require.NoError(t, err)

	before := tableCells(t, table, modelStore("pool", nil, nil), nil)
	require.Equal(t, []string{"pool", "2", "88", "55", "<unknown>"}, before[0])

	sec.Data["model-store-high-watermark"] = []byte("77")
	sec.Data["model-store-low-watermark"] = []byte("44")
	require.NoError(t, reader.client.Update(context.Background(), sec))

	after := tableCells(t, table, modelStore("pool", nil, nil), nil)
	assert.Equal(t, []string{"pool", "2", "77", "44", "<unknown>"}, after[0],
		"the next request shows the edited values, so nothing is cached between requests")
}

// TestModelStoreTableSkipsTheClusterReadWhenNothingInherits pins that the Secret is read only when a
// row needs it: an empty list and a list of pools that all state their own are answered from the
// objects alone, and a cluster that cannot answer does not fail them.
func TestModelStoreTableSkipsTheClusterReadWhenNothingInherits(t *testing.T) {
	testCases := []struct {
		name string
		obj  runtime.Object
		want [][]string
	}{
		{
			name: "an empty list",
			obj:  &worker.ModelStoreList{},
			want: [][]string{},
		},
		{
			name: "a list whose every pool states its watermarks",
			obj: &worker.ModelStoreList{Items: []worker.ModelStore{
				*modelStore("a", ptr.To(int32(91)), ptr.To(int32(40))),
				*modelStore("b", ptr.To(int32(92)), ptr.To(int32(41))),
			}},
			want: [][]string{
				{"a", "2", "91", "40", "<unknown>"},
				{"b", "2", "92", "41", "<unknown>"},
			},
		},
		{
			name: "a single pool that states its watermarks",
			obj:  modelStore("a", ptr.To(int32(91)), ptr.To(int32(40))),
			want: [][]string{
				{"a", "2", "91", "40", "<unknown>"},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// The read is made to fail, so a conversion that needed it would report the failure
			// rather than quietly render a default.
			reader := newCountingReader(t, nil, fmt.Errorf("the settings secret is unreadable"))
			table, err := modelStoreTable(reader.reader())
			require.NoError(t, err)

			assert.Equal(t, tc.want, tableCells(t, table, tc.obj, nil),
				"a pool stating its own watermarks is answered from the object alone")
			assert.Equal(t, 0, reader.reads, "Secret reads for a request that inherits nothing")
		})
	}
}

// TestModelStoreTableRefusesAnUnreadableCluster pins that a failed read is an error rather than a
// silently rendered default: a table that cannot say which cluster values are in force must not
// present the Setting's defaults as if they were.
func TestModelStoreTableRefusesAnUnreadableCluster(t *testing.T) {
	reader := newCountingReader(t, nil, fmt.Errorf("the settings secret is unreadable"))
	table, err := modelStoreTable(reader.reader())
	require.NoError(t, err)

	got, err := table.ConvertToTable(context.Background(), modelStore("pool", nil, nil), nil)
	require.Error(t, err, "a failed read is not a default watermark")
	assert.Nil(t, got, "no table is returned when the cluster values could not be read")
	assert.Equal(t, 1, reader.reads, "a failed read is still one read, not a retry per row")
}

// TestModelStoreTableRefusesAnUnreadableWatermark pins that watermark text which is not a percent
// is an error rather than a zero or a default: the columns are percents an operator reads to
// decide what a pool will do, and a wrong number there is worse than no table.
func TestModelStoreTableRefusesAnUnreadableWatermark(t *testing.T) {
	testCases := []struct {
		name string
		data map[string][]byte
	}{
		{
			name: "a high watermark that is not a number",
			data: map[string][]byte{"model-store-high-watermark": []byte("eighty")},
		},
		{
			name: "a low watermark that is not a number",
			data: map[string][]byte{"model-store-low-watermark": []byte("low")},
		},
		{
			name: "a blank watermark",
			data: map[string][]byte{"model-store-high-watermark": []byte("")},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			table, err := modelStoreTable(newCountingReader(t, settingsSecret(tc.data), nil).reader())
			require.NoError(t, err)

			got, err := table.ConvertToTable(context.Background(), modelStore("pool", nil, nil), nil)
			require.Error(t, err, "watermark text that is not a percent is not rendered as a zero")
			assert.Nil(t, got)
		})
	}
}

// TestModelStoreTableLeavesTheObjectAlone pins that the conversion is a read: the object it was
// handed keeps its spec, so a pool that inherits still has no watermarks written into it, and the
// table's own copy of the object is the same one.
func TestModelStoreTableLeavesTheObjectAlone(t *testing.T) {
	table, err := modelStoreTable(newCountingReader(t, settingsSecret(map[string][]byte{
		"model-store-high-watermark": []byte("88"),
		"model-store-low-watermark":  []byte("55"),
	}), nil).reader())
	require.NoError(t, err)

	store := modelStore("pool", nil, nil)
	store.ResourceVersion = "42"
	before := store.DeepCopy()

	got, err := table.ConvertToTable(context.Background(), store, nil)
	require.NoError(t, err)

	assert.Equal(t, before, store, "the converted object is not rewritten with the inherited values")
	require.Len(t, got.Rows, 1)
	embedded, ok := got.Rows[0].Object.Object.(*worker.ModelStore)
	require.True(t, ok, "the row embeds the object it was given")
	assert.Nil(t, embedded.Spec.Watermarks, "the table does not write the inherited values into the object")
	assert.Equal(t, "42", embedded.ResourceVersion, "the resourceVersion is carried through")
}

// TestModelStoreTableKeepsTheTableContract pins what kubectl relies on besides the two watermark
// columns: the column names and their order, and the list metadata a paged list carries.
func TestModelStoreTableKeepsTheTableContract(t *testing.T) {
	table, err := modelStoreTable(newCountingReader(t, settingsSecret(map[string][]byte{
		"model-store-high-watermark": []byte("88"),
		"model-store-low-watermark":  []byte("55"),
	}), nil).reader())
	require.NoError(t, err)

	list := &worker.ModelStoreList{
		ListMeta: meta.ListMeta{
			ResourceVersion:    "100",
			Continue:           "next-page",
			RemainingItemCount: ptr.To(int64(4)),
		},
		Items: []worker.ModelStore{*modelStore("pool", nil, nil)},
	}
	got, err := table.ConvertToTable(context.Background(), list, nil)
	require.NoError(t, err)

	headers := make([]string, 0, len(got.ColumnDefinitions))
	for _, c := range got.ColumnDefinitions {
		headers = append(headers, c.Name)
	}
	assert.Equal(t, []string{"Name", "Nodes", "High", "Low", "Age"}, headers, "the columns kubectl prints")
	assert.Equal(t, "100", got.ResourceVersion, "the list's resourceVersion")
	assert.Equal(t, "next-page", got.Continue, "the list's continue token")
	require.NotNil(t, got.RemainingItemCount)
	assert.Equal(t, int64(4), *got.RemainingItemCount, "the list's remaining item count")
}

// TestModelStoreTableKeepsNoHeaders pins that the header-less rendering, which kubectl asks for
// when it prints a table by itself, still carries the values.
func TestModelStoreTableKeepsNoHeaders(t *testing.T) {
	table, err := modelStoreTable(newCountingReader(t, settingsSecret(map[string][]byte{
		"model-store-high-watermark": []byte("88"),
		"model-store-low-watermark":  []byte("55"),
	}), nil).reader())
	require.NoError(t, err)

	got, err := table.ConvertToTable(context.Background(), modelStore("pool", nil, nil), &meta.TableOptions{NoHeaders: true})
	require.NoError(t, err)

	assert.Empty(t, got.ColumnDefinitions, "no headers were asked for")
	require.Len(t, got.Rows, 1)
	cells := make([]string, 0, len(got.Rows[0].Cells))
	for _, c := range got.Rows[0].Cells {
		cells = append(cells, printCell(c))
	}
	assert.Equal(t, []string{"pool", "2", "88", "55", "<unknown>"}, cells, "the values are still there")
}

// TestModelStoreTableRendersTheNodeCount pins that the count the worker observes is still read from
// the status, beside the watermarks now read from the cluster.
func TestModelStoreTableRendersTheNodeCount(t *testing.T) {
	table, err := modelStoreTable(newCountingReader(t, settingsSecret(nil), nil).reader())
	require.NoError(t, err)

	store := modelStore("pool", ptr.To(int32(91)), ptr.To(int32(40)))
	store.Status.Nodes = 3
	rows := tableCells(t, table, store, nil)

	require.Len(t, rows, 1)
	assert.Equal(t, strconv.Itoa(3), rows[0][1], "the node count still comes from the status")
}
