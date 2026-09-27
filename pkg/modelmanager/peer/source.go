// Package peer is the model-manager plugin's node-to-node sync: a server that serves a node's
// published trees to the plugins of other nodes, and a client that materializes one file from
// several such servers at once, falling back to the hub when no peer works.
//
// A peer serves published trees only, read-only, per file and by byte range; the client verifies
// everything a peer sends the way it verifies the hub's bytes. Plugins authenticate each other as
// plugins, and no tenant credential ever crosses this path.
package peer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

// hexPattern matches a manifest digest's hex part, the only name a tree carries here.
var hexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// peerPortName is the container port's name the chart declares on the plugin's container.
const peerPortName = "peer-sync"

// DefaultStreamsPerSource is how many requests a node opens to one source at once when nothing
// narrower is configured.
const DefaultStreamsPerSource = 4

// Source is one node whose plugin holds a digest published, as the pulling side sees it.
type Source struct {
	// NodeName is the node the serving plugin runs on.
	NodeName string
	// BaseURL is the peer endpoint's base, "https://<address>:<port>".
	BaseURL string
	// Client sends this source's requests, with the authentication its serving side asks for.
	Client *http.Client
	// streams bounds how many of this source's requests run at once; nil is unbounded.
	streams chan struct{}
}

// take admits one request to the source and returns the release to call when it is done.
func (s *Source) take(ctx context.Context) (func(), error) {
	if s.streams == nil {
		return func() {}, nil
	}
	select {
	case s.streams <- struct{}{}:
		return func() { <-s.streams }, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// DiscoverOptions is what discovery reads.
type DiscoverOptions struct {
	// Namespace is where the plugin's Pods run.
	Namespace string
	// SelfNode is this plugin's node, never a source.
	SelfNode string
	// Port is the peer port every plugin serves, this chart's own setting.
	Port int
	// StreamsPerSource bounds one source's concurrent requests.
	StreamsPerSource int
	// Client sends every source's requests.
	Client *http.Client
}

// Discover returns the nodes whose plugin holds digest published and ready, excluding the node
// this plugin runs on, each paired with its plugin's address. NodeModelStores carry what a node
// holds; the plugin's Pods carry where its endpoint answers.
func Discover(ctx context.Context, reader ctrlcli.Reader, opts DiscoverOptions, digest string) ([]*Source, error) {
	if _, ok := HexOf(digest); !ok {
		return nil, fmt.Errorf("digest %q is not a manifest digest", digest)
	}
	stores := new(workercore.NodeModelStoreList)
	if err := reader.List(ctx, stores); err != nil {
		return nil, fmt.Errorf("list node model stores: %w", err)
	}
	ready := map[string]bool{}
	for i := range stores.Items {
		if stores.Items[i].Name == opts.SelfNode {
			continue
		}
		for _, m := range stores.Items[i].Status.Models {
			if m.Digest == digest && m.State == workercore.NodeModelStoreModelStateReady {
				ready[stores.Items[i].Name] = true
			}
		}
	}
	if len(ready) == 0 {
		return nil, nil
	}

	pods := new(core.PodList)
	if err := reader.List(ctx, pods,
		ctrlcli.InNamespace(opts.Namespace),
		ctrlcli.MatchingLabels{deviceplugin.ComponentLabelKey: modelstore.PluginComponent},
	); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list the model-manager plugins: %w", err)
	}
	var sources []*Source
	for i := range pods.Items {
		p := &pods.Items[i]
		switch {
		case p.DeletionTimestamp != nil, p.Status.PodIP == "", p.Spec.NodeName == "", !isPodReady(p):
			continue
		case !ready[p.Spec.NodeName], !hasPeerPort(p):
			// A Pod without the peer port declared is an older plugin without a listener, not
			// a candidate to dial and fail.
			continue
		}
		s := &Source{
			NodeName: p.Spec.NodeName,
			BaseURL:  (&url.URL{Scheme: "https", Host: p.Status.PodIP + ":" + fmt.Sprint(opts.Port)}).String(),
			Client:   opts.Client,
		}
		if opts.StreamsPerSource > 0 {
			s.streams = make(chan struct{}, opts.StreamsPerSource)
		}
		sources = append(sources, s)
	}
	ctrllog.FromContext(ctx).V(1).Info("discovered peer sources", "digest", digest, "sources", len(sources))

	return sources, nil
}

// HexOf validates digest and returns its hex part.
func HexOf(digest string) (string, bool) {
	h, ok := strings.CutPrefix(digest, "sha256:")

	return h, ok && hexPattern.MatchString(h)
}

// isPodReady reports whether a plugin Pod answers.
func isPodReady(p *core.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == core.PodReady && c.Status == core.ConditionTrue {
			return true
		}
	}

	return false
}

// hasPeerPort reports whether a plugin Pod declares the peer port: an older plugin without it
// has nothing listening there.
func hasPeerPort(p *core.Pod) bool {
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			if port.Name == peerPortName {
				return true
			}
		}
	}

	return false
}

// requestTimeout bounds one peer request: a peer that stalls longer is a failed source, and the
// file goes to the hub or the next peer rather than wait.
const requestTimeout = 2 * time.Minute

// NewClient builds the client every source's requests go through, with the pulling side's
// authentication in it: no proxy, as Pod-to-Pod traffic must not be carried by one, and a
// server certificate that is self-signed, as the plugin's secure port serves one.
func NewClient(auth AuthOptions) (*http.Client, error) {
	tlsCfg, err := auth.ClientTLSConfig()
	if err != nil {
		return nil, err
	}
	transport := httpx.Transport(httpx.TransportOptions().WithoutProxy().WithTLSClientConfig(tlsCfg))

	return &http.Client{Transport: auth.RoundTripper(transport), Timeout: requestTimeout}, nil
}

// Sync is the peer sync's configuration, as one node runs it: what it serves to the other
// nodes' plugins, and how it pulls from them.
type Sync struct {
	// Port is the port the peer endpoints answer on; 0 is off.
	Port int
	// Auth is how peers authenticate each other.
	Auth AuthOptions
	// MaxServingStreams is how many file answers this node serves at once.
	MaxServingStreams int
	// StreamsPerSource is how many requests this node opens to one source at once.
	StreamsPerSource int
}
