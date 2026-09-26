package modelmanager

import (
	"net/http"
	"sort"
	"time"

	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

// newDownloadsHandler answers the running downloads, sorted by digest, for the worker's progress
// subresource: a live reading between the thresholds the node's status is written on. It is
// read-only and carries a digest, sizes and a source, nothing that names a tenant.
func newDownloadsHandler(progress func() map[string]materialize.DownloadProgress) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			httpx.Error(w, http.StatusMethodNotAllowed)
			return
		}

		resp := modelstore.Downloads{Downloads: []modelstore.Download{}}
		for hex, p := range progress() {
			resp.Downloads = append(resp.Downloads, modelstore.Download{
				Digest: "sha256:" + hex, DownloadedBytes: p.DownloadedBytes, SizeBytes: p.SizeBytes, Source: string(p.Source),
			})
		}
		sort.Slice(resp.Downloads, func(i, j int) bool { return resp.Downloads[i].Digest < resp.Downloads[j].Digest })
		httpx.PureJSON(w, http.StatusOK, resp)
	})

	return http.TimeoutHandler(h, 5*time.Second, "downloads timed out")
}

// ownObjectChanged passes an update of the node's NodeModelStore only when its spec changed: the
// plugin's own status write comes back as an update too, and reporting on it would make every write
// the cause of the next.
func ownObjectChanged(oldObj, newObj any) bool {
	o, okOld := oldObj.(interface{ GetGeneration() int64 })
	n, okNew := newObj.(interface{ GetGeneration() int64 })
	return !okOld || !okNew || o.GetGeneration() != n.GetGeneration()
}
