package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

const (
	// _ModelArtifactProgressTimeout bounds one progress answer, the live reads included.
	_ModelArtifactProgressTimeout = 5 * time.Second
	// _ModelArtifactProgressNodeTimeout bounds one node's live read; a node slower than this
	// contributes the value it last wrote.
	_ModelArtifactProgressNodeTimeout = 2 * time.Second
	// _ModelArtifactProgressMaxNodes bounds the live reads of one answer, and _ModelArtifactProgressParallel
	// how many run at once.
	_ModelArtifactProgressMaxNodes  = 64
	_ModelArtifactProgressParallel  = 16
	_ModelArtifactProgressMaxBytes  = 1 << 20
	_ModelArtifactProgressNotResolv = "the artifact is not resolved yet, so it has no content to count"
	_ModelArtifactProgressClaim     = "a claim source is mounted from its volume and never downloaded"
)

// ModelArtifactProgressHandler handles the "progress" subresource of v1.ModelArtifact objects: where
// the artifact's content is across the nodes, computed on each request and never stored.
//
// It reads the artifact, the NodeModelStores listing its digest, and, for each node downloading it,
// that node's plugin's running downloads, whose bytes move between the 5% steps the node's status is
// written on. A plugin that does not answer in time leaves its node's stored value. The answer is an
// aggregate that names no node.
type ModelArtifactProgressHandler struct {
	extensionapi.GetOperation

	// APIReader reads the artifact and the plugin Pods; Client lists the NodeModelStores from the
	// worker's cache.
	APIReader ctrlcli.Reader
	Client    ctrlcli.Reader
	// Downloads reads a plugin's running downloads at a URL; nil uses the shared client.
	Downloads func(ctx context.Context, url string) (*modelstore.Downloads, error)
	Now       func() time.Time
}

func newModelArtifactProgressHandler(parent rest.Scoper, opts extensionapi.SetupOptions) *ModelArtifactProgressHandler {
	h := &ModelArtifactProgressHandler{APIReader: opts.Manager.GetAPIReader(), Client: opts.Manager.GetClient()}
	h.GetOperation = extensionapi.WithSubResourceGet(parent, h)

	return h
}

var (
	_ rest.Storage = (*ModelArtifactProgressHandler)(nil)
	_ rest.Getter  = (*ModelArtifactProgressHandler)(nil)
)

func (h *ModelArtifactProgressHandler) New() runtime.Object { return &worker.ModelArtifactProgress{} }

func (h *ModelArtifactProgressHandler) Destroy() {}

func (h *ModelArtifactProgressHandler) OnGet(ctx context.Context, key types.NamespacedName, _ ctrlcli.GetOptions) (runtime.Object, error) {
	ctx, cancel := context.WithTimeout(ctx, _ModelArtifactProgressTimeout)
	defer cancel()

	ma := new(workercore.ModelArtifact)
	if err := h.APIReader.Get(ctx, key, ma, ctrlclix.WithoutQuorum); err != nil {
		return nil, err
	}
	out := &worker.ModelArtifactProgress{
		ObjectMeta: meta.ObjectMeta{Name: ma.Name, Namespace: ma.Namespace},
		Timestamp:  meta.NewTime(h.now()),
	}
	switch r := ma.Status.Resolved; {
	case ma.Spec.Source.PersistentVolumeClaim != nil:
		out.Reason = _ModelArtifactProgressClaim
		return out, nil
	case r == nil || r.ManifestDigest == "":
		out.Reason = _ModelArtifactProgressNotResolv
		return out, nil
	default:
		out.ManifestDigest, out.SizeBytes = r.ManifestDigest, r.SizeBytes
	}

	stores := new(workercore.NodeModelStoreList)
	if err := h.Client.List(ctx, stores); err != nil {
		return nil, fmt.Errorf("list node model stores: %w", err)
	}
	entries := modelstore.NodeEntries(stores.Items, out.ManifestDigest)
	out.Live = h.readLive(ctx, out.ManifestDigest, entries)

	a := modelstore.AggregateEntries(entries)
	out.Ready, out.Downloading, out.Failed = a.Ready, a.Downloading, a.Failed
	out.DownloadingBytes = a.DownloadedBytes
	if p, ok := a.DownloadingPercent(); ok {
		out.DownloadingPercent = &p
	}
	for _, reason := range a.Reasons() {
		out.FailureReasons = append(out.FailureReasons, worker.ModelArtifactProgressReason{Reason: reason, Count: a.FailureReasons[reason]})
	}

	return out, nil
}

// readLive replaces each downloading node's stored bytes with its plugin's live reading where the
// plugin answers in time, and returns how many did. A reading never moves a node back.
func (h *ModelArtifactProgressHandler) readLive(
	ctx context.Context, digest string, entries map[string]workercore.NodeModelStoreModel,
) int32 {
	var nodes []string
	for node, m := range entries {
		if m.State == workercore.NodeModelStoreModelStateDownloading && len(nodes) < _ModelArtifactProgressMaxNodes {
			nodes = append(nodes, node)
		}
	}
	if len(nodes) == 0 {
		return 0
	}
	pods, err := h.pluginPods(ctx)
	if err != nil {
		ctrllog.FromContext(ctx).Error(err, "list the model-manager plugins; progress keeps the stored values")
		return 0
	}

	var (
		mu   sync.Mutex
		live int32
		wg   sync.WaitGroup
		sem  = make(chan struct{}, _ModelArtifactProgressParallel)
	)
	for _, node := range nodes {
		pod := pods[node]
		if pod == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nctx, cancel := context.WithTimeout(ctx, _ModelArtifactProgressNodeTimeout)
			defer cancel()
			url := "https://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(pluginSecurePortOf(pod))) + modelstore.DownloadsPath
			ds, err := h.downloads(nctx, url)
			if err != nil {
				ctrllog.FromContext(ctx).V(2).Info("read a plugin's downloads", "node", node, "error", err.Error())
				return
			}
			for _, d := range ds.Downloads {
				if d.Digest != digest {
					continue
				}
				mu.Lock()
				m := entries[node]
				m.DownloadedBytes = max(m.DownloadedBytes, d.DownloadedBytes)
				if d.SizeBytes > 0 {
					m.SizeBytes = d.SizeBytes
				}
				entries[node] = m
				live++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return live
}

// pluginPods returns the ready model-manager plugin Pod of each node, by node name.
func (h *ModelArtifactProgressHandler) pluginPods(ctx context.Context) (map[string]*core.Pod, error) {
	list := new(core.PodList)
	if err := h.APIReader.List(ctx, list,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName),
		ctrlcli.MatchingLabels{deviceplugin.ComponentLabelKey: modelstore.PluginComponent},
		ctrlclix.WithoutQuorum,
	); err != nil {
		return nil, err
	}
	pods := map[string]*core.Pod{}
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp == nil && p.Status.PodIP != "" && p.Spec.NodeName != "" && deviceplugin.IsPodReady(p) {
			pods[p.Spec.NodeName] = p
		}
	}

	return pods, nil
}

// pluginSecurePortOf returns the plugin container's named secure port, or the chart's default.
func pluginSecurePortOf(pod *core.Pod) int {
	for i := range pod.Spec.Containers {
		for j := range pod.Spec.Containers[i].Ports {
			if pod.Spec.Containers[i].Ports[j].Name == modelstore.PluginSecurePortName {
				return int(pod.Spec.Containers[i].Ports[j].ContainerPort)
			}
		}
	}
	return modelstore.DefaultPluginSecurePort
}

func (h *ModelArtifactProgressHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *ModelArtifactProgressHandler) downloads(ctx context.Context, url string) (*modelstore.Downloads, error) {
	if h.Downloads != nil {
		return h.Downloads(ctx, url)
	}
	return fetchPluginDownloads(ctx, url)
}

// _pluginDownloadsClient reads the plugins' downloads: one keep-alive transport per process.
var _pluginDownloadsClient = &http.Client{
	Transport: httpx.Transport(httpx.TransportOptions().WithoutProxy().WithTLSClientConfig(
		&tls.Config{InsecureSkipVerify: true})), // nolint: gosec
}

// fetchPluginDownloads GETs a plugin's running downloads. The plugin serves a self-signed
// certificate, as the device manager does, inside the same trust domain; its answer carries no
// secret, and the body is bounded because the peer is not verified. Proxying is disabled: a
// proxy must never carry Pod-to-Pod traffic.
func fetchPluginDownloads(ctx context.Context, url string) (*modelstore.Downloads, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := _pluginDownloadsClient.Do(req)
	if err != nil {
		return nil, err
	}
	body := io.LimitReader(resp.Body, _ModelArtifactProgressMaxBytes)
	defer func() {
		_, _ = io.Copy(io.Discard, body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	ds := new(modelstore.Downloads)
	if err := json.NewDecoder(body).Decode(ds); err != nil {
		return nil, fmt.Errorf("decode the downloads: %w", err)
	}

	return ds, nil
}
