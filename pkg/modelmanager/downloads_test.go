package modelmanager

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
	"gpustack.ai/gpustack/pkg/modelstore"
)

func TestDownloadsHandler(t *testing.T) {
	progress := func() map[string]materialize.DownloadProgress {
		return map[string]materialize.DownloadProgress{
			"bb": {DownloadedBytes: 10, SizeBytes: 100, Source: workercore.NodeModelStoreModelSourceHub},
			"aa": {SizeBytes: 5},
		}
	}
	h := newDownloadsHandler(progress)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, modelstore.DownloadsPath, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var got modelstore.Downloads
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, []modelstore.Download{
		{Digest: "sha256:aa", SizeBytes: 5},
		{Digest: "sha256:bb", DownloadedBytes: 10, SizeBytes: 100, Source: "Hub"},
	}, got.Downloads)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, modelstore.DownloadsPath, nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "the path is read-only")

	rec = httptest.NewRecorder()
	newDownloadsHandler(func() map[string]materialize.DownloadProgress { return nil }).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, modelstore.DownloadsPath, nil))
	assert.JSONEq(t, `{"downloads":[]}`, rec.Body.String(), "nothing running is an empty list, not null")
}

// TestOwnObjectChanged pins that the plugin's own status write does not trigger the next report,
// while a change of the node's spec does.
func TestOwnObjectChanged(t *testing.T) {
	obj := func(gen, stored int64) *workercore.NodeModelStore {
		return &workercore.NodeModelStore{
			ObjectMeta: meta.ObjectMeta{Name: "node-1", Generation: gen},
			Status:     workercore.NodeModelStoreStatus{Capacity: &workercore.NodeModelStoreCapacity{StoredBytes: stored}},
		}
	}
	assert.False(t, ownObjectChanged(obj(3, 10), obj(3, 20)), "a status write")
	assert.True(t, ownObjectChanged(obj(3, 10), obj(4, 10)), "a spec change")
}
