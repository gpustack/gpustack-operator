// Package report applies a node's effective configuration to the plugin and writes what the node
// holds into its NodeModelStore's status, only when that changes: a state change at once, download
// progress on thresholds.
package report

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	gpustack "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/download"
	"gpustack.ai/gpustack/pkg/modelmanager/gc"
	"gpustack.ai/gpustack/pkg/modelmanager/materialize"
	"gpustack.ai/gpustack/pkg/modelmanager/metrics"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modelstore"
)

// The status conditions and their reasons.
const (
	ConditionReady       = "Ready"
	ConditionCapacityLow = "CapacityLow"

	ReasonServing              = "Serving"
	ReasonInvalidConfiguration = "InvalidConfiguration"
	ReasonCapacityLow          = "NothingToRemove"
	ReasonCapacityFine         = "WithinWatermarks"

	// MaxModels bounds status.models, the schema's own bound.
	MaxModels = 256
	// caBundleKey is the key of the CA bundle ConfigMap, the ModelArtifact controller's convention.
	caBundleKey = "ca.crt"
)

// DefaultInterval is how often the reporter collects and reports without an event.
const DefaultInterval = 5 * time.Minute

// The progress write rule. The fields that move with every received byte, an entry's
// downloadedBytes and, while a download runs, capacity.storedBytes, are written only once
// progressInterval passed since the last write and some download moved by progressStep of its size,
// or together with a change of any other field. A download then writes at most 1/progressStep times
// for its progress whatever its speed, and at most once per progressInterval.
const (
	progressInterval = 30 * time.Second
	progressStep     = 0.05
	// ProgressTick is how often the reporter looks at running downloads, so progress reaches the
	// object on its threshold rather than on the next unrelated event.
	ProgressTick = 10 * time.Second
)

// conflictRetry is how long a report that lost a write to a newer object waits before it reads the
// object again, so the informer has caught up rather than serving the same stale copy.
const conflictRetry = time.Second

// Reporter keeps the plugin configured from its NodeModelStore's spec, collects the cache and
// reports into its status. It is the driver's Events, and the materializer's Changed.
type Reporter struct {
	// Reader reads the node's NodeModelStore and the CA ConfigMap from the cache; Client writes
	// the status.
	Reader    ctrlcli.Reader
	Client    ctrlcli.Client
	NodeName  string
	Namespace string

	Store       *store.Store
	Collector   *gc.Collector
	Downloading func() map[string]bool
	// Progress is how far each running download is, by digest; nil reports no progress.
	Progress   func() map[string]materialize.DownloadProgress
	Downloader *download.Downloader
	// KubeletCap is the cap on the high watermark from the node's kubelet thresholds when the cache
	// shares kubelet's filesystem, nil when it does not.
	KubeletCap func(k *workercore.NodeModelStoreKubelet, totalBytes uint64) *gc.Cap
	Now        func() time.Time
	Interval   time.Duration

	mu        sync.Mutex
	spec      *workercore.NodeModelStoreSpec
	specErr   error
	client    *http.Client
	ca        []byte
	capTotal  uint64
	high, low int32
	capNote   string
	trigger   chan struct{}
	// lastWrite is when this plugin last wrote the status; Report is its only writer.
	lastWrite time.Time
}

var _ materialize.Environment = (*Reporter)(nil).Environment

func (r *Reporter) init() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.trigger == nil {
		r.trigger = make(chan struct{}, 1)
	}
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Trigger asks for a report soon; triggers coalesce.
func (r *Reporter) Trigger(string) {
	r.init()
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// Mounted records the digest's use and asks for a report.
func (r *Reporter) Mounted(hex string) { r.used(hex) }

// Unmounted records the digest's use and asks for a report.
func (r *Reporter) Unmounted(hex string) { r.used(hex) }

func (r *Reporter) used(hex string) {
	now := r.now()
	if err := r.Store.UpdateDigest(hex, func(rec *store.DigestRecord) { rec.LastUsedTime = now }); err != nil {
		klog.ErrorS(err, "record a digest's use", "digest", hex)
	}
	r.Trigger(hex)
}

// Watermarks are the effective watermarks, for collection, or why there are none in force: no spec
// applied yet, or the node's spec failing its check. A spec that fails after an earlier one applied
// does not leave the earlier watermarks in force, since the node reports that configuration as not
// in force either.
func (r *Reporter) Watermarks() (int32, int32, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.specErr != nil:
		return 0, 0, "the node's configuration fails its check, so nothing is collected under the previous one"
	case r.spec == nil:
		return 0, 0, "the node's configuration has not been applied yet, so there are no watermarks to collect against"
	}

	return r.high, r.low, ""
}

// Environment returns the hub and the downloader built from the node's current configuration, or an
// InvalidRequest error while that configuration is missing or fails its check.
func (r *Reporter) Environment(context.Context) (materialize.Hub, *download.Downloader, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	// The node's configuration names no tenant, so the whole message is the detail.
	case r.specErr != nil:
		msg := r.specErr.Error()
		return nil, nil, &download.Error{Reason: download.ReasonInvalidRequest, Message: msg, Detail: msg}
	case r.spec == nil:
		const msg = "the node's configuration has not arrived yet"
		return nil, nil, &download.Error{Reason: download.ReasonInvalidRequest, Message: msg, Detail: msg}
	}

	return &modelartifact.HuggingFace{Endpoint: r.spec.Hub.HuggingFaceEndpoint, Client: listingClient(r.client)}, r.Downloader, nil
}

func httpClient(hub workercore.NodeModelStoreHub, caBundle []byte) (*http.Client, error) {
	client, err := modelartifact.NewHTTPClient(modelartifact.HTTPClientOptions{
		HTTPSProxy: hub.HTTPSProxy, NoProxy: hub.NoProxy, CABundle: caBundle,
	})
	if err != nil {
		return nil, err
	}
	// The downloader bounds a stalled range itself; an overall timeout would cut a large range short.
	client.Timeout = 0

	return client, nil
}

// listingClient is the download client with a timeout per request again, for the listing's small
// answers.
func listingClient(c *http.Client) *http.Client {
	l := *c
	l.Timeout = modelartifact.DefaultRequestTimeout
	return &l
}

// Run reports on every trigger and every interval until ctx is done.
func (r *Reporter) Run(ctx context.Context) error {
	r.init()
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	pt := time.NewTicker(ProgressTick)
	defer pt.Stop()
	for {
		if err := r.Report(ctx); err != nil {
			klog.ErrorS(err, "report the node's model cache")
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
				break wait
			case <-r.trigger:
				break wait
			case <-pt.C:
				// Only a running download has progress to report.
				if len(r.Downloading()) > 0 {
					break wait
				}
			}
		}
	}
}

// Report applies the spec, collects, and writes the status if it changed.
func (r *Reporter) Report(ctx context.Context) error {
	nms := new(workercore.NodeModelStore)
	if err := r.Reader.Get(ctx, ctrlcli.ObjectKey{Name: r.NodeName}, nms); err != nil {
		if kerrors.IsNotFound(err) {
			// The worker creates it once kubelet lists the driver for this node.
			return nil
		}
		return err
	}
	r.apply(ctx, nms)

	res, err := r.Collector.Collect(ctx)
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}
	status, err := r.status(nms, res)
	if err != nil {
		return err
	}
	// Semantically: a time read back from the API is in the local zone, and equal all the same.
	if equality.Semantic.DeepEqual(status, nms.Status) {
		return nil
	}
	if onlyProgressMoved(status, nms.Status) && !r.progressDue(status, nms.Status) {
		return nil
	}
	nms.Status = status
	if err := r.Client.Status().Update(ctx, nms); err != nil {
		if kerrors.IsConflict(err) {
			time.AfterFunc(conflictRetry, func() { r.Trigger("") })
			return nil
		}
		return fmt.Errorf("update status: %w", err)
	}
	r.lastWrite = r.now()

	return nil
}

// onlyProgressMoved says next differs from old in progress fields alone: an entry's
// downloadedBytes, and capacity.storedBytes while some entry downloads. Without a running download
// storedBytes moves only when content is removed, which is a state change.
func onlyProgressMoved(next, old workercore.NodeModelStoreStatus) bool {
	written := make(map[string]int64, len(old.Models))
	for _, m := range old.Models {
		written[m.Digest] = m.DownloadedBytes
	}
	held := *next.DeepCopy()
	downloading := false
	for i := range held.Models {
		m := &held.Models[i]
		if m.State == workercore.NodeModelStoreModelStateDownloading {
			downloading = true
		}
		if b, ok := written[m.Digest]; ok {
			m.DownloadedBytes = b
		}
	}
	if downloading && held.Capacity != nil && old.Capacity != nil {
		held.Capacity.StoredBytes = old.Capacity.StoredBytes
	}

	return equality.Semantic.DeepEqual(held, old)
}

// progressDue says a progress-only write may go: progressInterval passed since the last write, and
// some download moved by progressStep of its size since the value written.
func (r *Reporter) progressDue(next, old workercore.NodeModelStoreStatus) bool {
	if r.now().Sub(r.lastWrite) < progressInterval {
		return false
	}
	written := make(map[string]int64, len(old.Models))
	for _, m := range old.Models {
		written[m.Digest] = m.DownloadedBytes
	}
	for _, m := range next.Models {
		if m.SizeBytes > 0 && float64(m.DownloadedBytes-written[m.Digest]) >= progressStep*float64(m.SizeBytes) {
			return true
		}
	}

	return false
}

// apply takes the spec as the plugin's configuration, after checking it again: a hand edit of the
// object bypasses the worker's checks.
func (r *Reporter) apply(ctx context.Context, nms *workercore.NodeModelStore) {
	spec := nms.Spec
	err := modelstore.Validate(spec)
	var ca []byte
	if err == nil && spec.Hub.CABundleConfigMap != "" {
		cm := new(core.ConfigMap)
		if gerr := r.Reader.Get(ctx, ctrlcli.ObjectKey{Namespace: r.Namespace, Name: spec.Hub.CABundleConfigMap}, cm); gerr != nil {
			err = fmt.Errorf("read the CA bundle ConfigMap %q: %w", spec.Hub.CABundleConfigMap, gerr)
		} else if ca = []byte(cm.Data[caBundleKey]); len(ca) == 0 {
			err = fmt.Errorf("the CA bundle ConfigMap %q has no %s", spec.Hub.CABundleConfigMap, caBundleKey)
		}
	}

	var (
		total    uint64
		usageErr error
	)
	if err == nil && r.KubeletCap != nil {
		var u gc.Usage
		u, usageErr = r.Collector.Usage()
		total = u.Total
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if usageErr != nil {
		// The cap follows the filesystem's size, which a failed read does not change: the last reading
		// stands, so the watermarks stay capped and the client is not rebuilt. Without a reading there
		// is no cap, and the spec is not applied, so nothing collects under uncapped watermarks.
		total = r.capTotal
		if total == 0 {
			err = fmt.Errorf("read the cache's filesystem for the kubelet cap: %w", usageErr)
		}
	}
	if err == nil && r.spec != nil && equality.Semantic.DeepEqual(*r.spec, spec) && bytes.Equal(r.ca, ca) &&
		r.capTotal == total {
		// Nothing changed: the client, its connections and the running downloads' limits stay.
		r.specErr = nil
		return
	}
	var client *http.Client
	if err == nil {
		client, err = httpClient(spec.Hub, ca)
	}
	r.specErr = err
	if err != nil {
		return
	}
	high, low, note := spec.Watermarks.HighPercent, spec.Watermarks.LowPercent, ""
	if total > 0 {
		c := r.KubeletCap(spec.Kubelet, total)
		high, low, note = gc.Effective(high, low, c)
		// Both are said whether or not the cap lowers the watermark: the defaults, or the threshold the
		// cap set aside, may not be what kubelet enforces.
		if spec.Kubelet == nil {
			note = joinNotes("the node's effective kubelet configuration could not be read, so the cap on the "+
				"filesystem the cache shares with kubelet assumes kubelet's defaults", note)
		}
		if c != nil && c.Ignored != "" && note == "" {
			note = "the cap on the filesystem the cache shares with kubelet set a threshold aside: " + c.Ignored
		}
	}
	r.spec, r.client, r.ca, r.capTotal, r.high, r.low, r.capNote = &spec, client, ca, total, high, low, note
	r.Downloader.SetClient(client)
	r.Downloader.SetLimits(int(spec.Download.Concurrency), spec.Download.BytesPerSecond)
}

func (r *Reporter) status(nms *workercore.NodeModelStore, res gc.Result) (workercore.NodeModelStoreStatus, error) {
	models, omitted, stored, err := r.models()
	if err != nil {
		return workercore.NodeModelStoreStatus{}, err
	}
	metrics.StoredBytes.Set(float64(stored))
	metrics.CapacityBytes.Set(float64(res.Usage.Total))

	r.mu.Lock()
	specErr, capNote := r.specErr, r.capNote
	r.mu.Unlock()

	ready := condition(ConditionReady, meta.ConditionTrue, ReasonServing,
		joinNotes(capNote, omittedNote(omitted), skippedNote(res.Skipped)))
	if specErr != nil {
		ready = condition(ConditionReady, meta.ConditionFalse, ReasonInvalidConfiguration,
			joinNotes("no download starts until the configuration is fixed: "+specErr.Error(), skippedNote(res.Skipped)))
	}
	low := condition(ConditionCapacityLow, meta.ConditionFalse, ReasonCapacityFine, "")
	if res.CapacityLow {
		low = condition(ConditionCapacityLow, meta.ConditionTrue, ReasonCapacityLow,
			"the cache's filesystem is above the high watermark and every model on it is in use")
	}

	return workercore.NodeModelStoreStatus{
		ObservedGeneration: nms.Generation,
		Capacity: &workercore.NodeModelStoreCapacity{
			TotalBytes:  int64(min(res.Usage.Total, math.MaxInt64)), // nolint: gosec
			StoredBytes: stored,
			UsedPercent: int32(math.Floor(res.Usage.Percent()/5) * 5),
		},
		Models:     models,
		Conditions: carryTransitions(nms.Status.Conditions, []gpustack.Condition{ready, low}, r.now()),
	}, nil
}

// models lists what the node holds, keeping the referenced and then the most recently used when more
// is held than status may list, and what the published trees and partials occupy.
func (r *Reporter) models() ([]workercore.NodeModelStoreModel, int, int64, error) {
	markers, err := r.Store.Published()
	if err != nil {
		return nil, 0, 0, err
	}
	refs, err := r.Store.Refs()
	if err != nil {
		return nil, 0, 0, err
	}
	records, err := r.Store.Digests()
	if err != nil {
		return nil, 0, 0, err
	}
	partials, err := r.Store.Partials()
	if err != nil {
		return nil, 0, 0, err
	}
	referenced := map[string]bool{}
	for _, ref := range refs {
		referenced[ref.Hex] = true
	}
	downloading := r.Downloading()
	var progress map[string]materialize.DownloadProgress
	if r.Progress != nil {
		progress = r.Progress()
	}

	var stored int64
	seen := map[string]bool{}
	var models []workercore.NodeModelStoreModel
	for _, m := range markers {
		hex := store.HexOf(m.Digest)
		seen[hex] = true
		stored += m.SizeBytes
		models = append(models, workercore.NodeModelStoreModel{
			Digest: m.Digest, State: workercore.NodeModelStoreModelStateReady, SizeBytes: m.SizeBytes,
			Source:     workercore.NodeModelStoreModelSource(m.Source),
			Referenced: referenced[hex], LastUsedTime: hourOf(records[hex].LastUsedTime),
		})
	}
	for _, p := range partials {
		stored += p.SizeBytes
	}
	for hex := range downloading {
		if seen[hex] {
			continue
		}
		seen[hex] = true
		p := progress[hex]
		models = append(models, workercore.NodeModelStoreModel{
			Digest: "sha256:" + hex, State: workercore.NodeModelStoreModelStateDownloading,
			SizeBytes: p.SizeBytes, DownloadedBytes: max(p.DownloadedBytes, 0), Source: p.Source,
			LastUsedTime: hourOf(records[hex].LastUsedTime),
		})
	}
	for hex, rec := range records {
		if seen[hex] || rec.Reason == "" || rec.Reason == download.ReasonCanceled {
			continue
		}
		// The ledger keeps nanoseconds but a stored metav1.Time is read back in seconds; without
		// the truncation the two never compare equal and every report writes.
		retry := meta.NewTime(rec.RetryTime.UTC().Truncate(time.Second))
		models = append(models, workercore.NodeModelStoreModel{
			Digest: "sha256:" + hex, State: workercore.NodeModelStoreModelStateFailed,
			LastUsedTime: hourOf(rec.LastUsedTime), Reason: rec.Reason, Message: rec.Message, RetryTime: &retry,
		})
	}

	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if a.Referenced != b.Referenced {
			return a.Referenced
		}
		at, bt := timeOf(a.LastUsedTime), timeOf(b.LastUsedTime)
		if !at.Equal(bt) {
			return at.After(bt)
		}
		return a.Digest < b.Digest
	})
	omitted := 0
	if len(models) > MaxModels {
		omitted = len(models) - MaxModels
		models = models[:MaxModels]
	}

	return models, omitted, stored, nil
}

// hourOf truncates a use to the hour, so use does not rewrite the object.
func hourOf(t time.Time) *meta.Time {
	if t.IsZero() {
		return nil
	}
	h := meta.NewTime(t.UTC().Truncate(time.Hour))
	return &h
}

func timeOf(t *meta.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func condition(typ string, status meta.ConditionStatus, reason, message string) gpustack.Condition {
	return gpustack.Condition{Type: typ, Status: status, Reason: reason, Message: message}
}

// carryTransitions keeps a condition's transition time while its status holds, so an unchanged
// report is equal to the stored one and writes nothing.
func carryTransitions(old, next []gpustack.Condition, now time.Time) []gpustack.Condition {
	for i := range next {
		next[i].LastTransitionTime = meta.NewTime(now.UTC().Truncate(time.Second))
		for _, o := range old {
			if o.Type == next[i].Type && o.Status == next[i].Status {
				next[i].LastTransitionTime = o.LastTransitionTime
			}
		}
	}

	return next
}

func omittedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d more models are on the node than status lists", n)
}

func skippedNote(reason string) string {
	if reason == "" {
		return ""
	}
	return "collection skipped: " + reason
}

func joinNotes(notes ...string) string {
	var out string
	for _, n := range notes {
		switch {
		case n == "":
		case out == "":
			out = n
		default:
			out += "; " + n
		}
	}
	return out
}
