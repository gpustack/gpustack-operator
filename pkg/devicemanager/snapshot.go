package devicemanager

import (
	"net/http"
	"time"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

// MonitorSnapshotPath is the webserver path that serves the latest accelerator monitor snapshot.
const MonitorSnapshotPath = "/monitor/snapshot"

// MonitorSnapshot is the JSON envelope served at MonitorSnapshotPath: the latest accelerator
// metrics sample plus the time the device manager stored it.
type MonitorSnapshot = detector.MonitorSnapshot

// newMonitorSnapshotHandler returns a read-only GET handler that serves the latest accelerator
// monitor snapshot as JSON. Before the first monitor tick it answers 200 with an empty group list
// (and a zero timestamp) rather than 204, so consumers always parse the same envelope.
func newMonitorSnapshotHandler(snapshot func() *MonitorSnapshot) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			httpx.Error(w, http.StatusMethodNotAllowed)
			return
		}

		resp := MonitorSnapshot{
			Groups: device.MetricsGroupList{},
		}
		if s := snapshot(); s != nil {
			resp = *s
			if resp.Groups == nil {
				resp.Groups = device.MetricsGroupList{}
			}
		}
		httpx.PureJSON(w, http.StatusOK, resp)
	})
	return http.TimeoutHandler(h, 5*time.Second, "monitor snapshot timed out")
}
