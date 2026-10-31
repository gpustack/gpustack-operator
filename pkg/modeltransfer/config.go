// Package modeltransfer is the source side of remote weight loading: the node's
// source broker over the model cache, its worker control service, and the
// model-transfer command that runs them in a process of their own, apart from the
// CSI plugin, so a vendor failure never reaches a mount.
package modeltransfer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	authentication "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	kubernetes "gpustack.ai/gpustack/pkg/kubeclients/kubernetes"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubeconfig"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/modeltransfer/broker"
)

// SourceWorkerComponent is the component label on the shared source DaemonSet.
const SourceWorkerComponent = "model-transfer"

// SourceWorkerLabel selects shared source Pods for reconstruction.
var SourceWorkerLabel = labels.SelectorFromSet(labels.Set{
	"app.kubernetes.io/component": SourceWorkerComponent,
})

// Options are the model-transfer command's flags.
type Options struct {
	// KubeConnTimeout limits each Kubernetes request; zero preserves the supplied timeout.
	KubeConnTimeout time.Duration
	// KubeConnQPS and KubeConnBurst override client rate limits when positive.
	KubeConnQPS   float64
	KubeConnBurst int
	// KubeContentType overrides the Kubernetes request encoding when nonempty.
	KubeContentType string

	// NodeName is the node this broker serves.
	NodeName string
	// CacheRoot is the model cache root whose published trees sources fill
	// from; the model-manager plugin's own root.
	CacheRoot string
	// BrokerRoot is the broker's durable ledger root, its own directory and
	// not inside the cache root's layout.
	BrokerRoot string
	// ControlAddr is where the worker control service listens; the transport
	// workers, on the same node, reach it there.
	ControlAddr string
	// WorkerNamespace is the operator namespace containing the shared source Pod.
	WorkerNamespace string

	// The node-level administrator limits within which admission happens.
	PhysicalBytes   int64
	MaxGenerations  int
	MaxQuarantined  int
	QuarantineBytes int64
	LeaseTTL        time.Duration
	// MinMemoryBytes is the floor under PhysicalBytes: the smallest source
	// buffer the node still serves. A configured value below 1 GiB resolves
	// up to 1 GiB at start.
	MinMemoryBytes int64
	// MaxSessions caps the node's concurrently admitted sessions. A value
	// below one resolves to the default of 64 at start.
	MaxSessions int32

	// SessionAddr is where the engine session listener serves, over TLS.
	SessionAddr string
	// SessionTLSCert and SessionTLSServeKey are the chart-managed serving
	// certificate pair; the session listener refuses plaintext.
	SessionTLSCert string
	SessionTLSKey  string
	// BootstrapFile is the Pod-local file the broker writes its worker
	// bootstrap credential into, inside the emptyDir the two containers
	// share. It is never a cluster-visible path.
	BootstrapFile string
	// WorkerID names this node's transport worker in admissions.
	WorkerID string
	// VendorScope is the broker's vendor identity: it prefixes generation
	// tokens and cache references so two vendor brokers on one node can
	// never collide in either.
	VendorScope string
}

// NewOptions returns the options with their defaults: the standard node's
// cache root beside the model-manager's, conservative node limits an
// administrator raises per node. The operator namespace must be supplied.
func NewOptions() *Options {
	return &Options{
		KubeConnTimeout: 5 * time.Minute,
		KubeConnQPS:     defaultKubeConnQPS,
		KubeConnBurst:   defaultKubeConnBurst,
		NodeName:        "",
		CacheRoot:       "/var/lib/gpustack/models",
		BrokerRoot:      "/var/lib/gpustack/model-transfer",
		ControlAddr:     "127.0.0.1:9250",
		WorkerNamespace: "",

		PhysicalBytes:   0, // zero derives the ceiling from the node's total RAM
		MaxGenerations:  8,
		MaxQuarantined:  2,
		QuarantineBytes: 8 << 30,
		LeaseTTL:        30 * time.Second,
		MinMemoryBytes:  1 << 30,
		MaxSessions:     64,

		SessionAddr:    ":9251",
		SessionTLSCert: "",
		SessionTLSKey:  "",
		BootstrapFile:  "/run/model-transfer/bootstrap",
		WorkerID:       "",
		VendorScope:    "",
	}
}

// Validate checks the options. PhysicalBytes may be zero: ResolveMemoryLimits
// derives the ceiling from the node's total RAM before the broker opens, and
// only a negative figure is a misconfiguration.
func (o *Options) Validate() error {
	for _, check := range []struct {
		bad bool
		err error
	}{
		{o.KubeConnTimeout != 0 && o.KubeConnTimeout < 10*time.Second, errors.New("kube-conn-timeout must be at least 10s")},
		{
			math.IsNaN(o.KubeConnQPS) || math.IsInf(o.KubeConnQPS, 0) || (o.KubeConnQPS != 0 && o.KubeConnQPS < 10),
			errors.New("kube-conn-qps must be finite and at least 10"),
		},
		{o.KubeConnBurst != 0 && o.KubeConnBurst < 10, errors.New("kube-conn-burst must be at least 10")},
		{o.KubeConnQPS > 0 && o.KubeConnBurst > 0 && float64(o.KubeConnBurst) <= o.KubeConnQPS, errors.New("kube-conn-burst must exceed kube-conn-qps")},
		{o.NodeName == "", errors.New("node-name is required")},
		{o.CacheRoot == "", errors.New("cache-root is required")},
		{o.BrokerRoot == "", errors.New("broker-root is required")},
		{o.ControlAddr == "", errors.New("control-addr is required")},
		{o.WorkerNamespace == "", errors.New("worker-namespace is required")},
		{o.PhysicalBytes < 0, errors.New("physical-bytes must not be negative")},
		{o.MaxGenerations < 1, errors.New("max-generations must be positive")},
		{o.MaxQuarantined < 1, errors.New("max-quarantined must be positive")},
		{o.QuarantineBytes <= 0, errors.New("quarantine-bytes must be positive")},
		{o.LeaseTTL <= 0, errors.New("lease-ttl must be positive")},
		{o.SessionAddr == "", errors.New("session-addr is required")},
		{o.SessionTLSCert == "" || o.SessionTLSKey == "", errors.New("session-tls-cert and session-tls-key are required")},
		{o.BootstrapFile == "", errors.New("bootstrap-file is required")},
		{o.WorkerID == "", errors.New("worker-id is required")},
		{o.VendorScope == "", errors.New("vendor-scope is required")},
	} {
		if check.bad {
			return check.err
		}
	}
	return nil
}

// defaultSessions is the node's session cap when its configured value is
// unusable, matching the chart's own default.
const defaultSessions int32 = 64

// ResolveMemoryLimits settles the node's memory limits before the broker
// opens. A buffer floor below 1 GiB is raised to 1 GiB. A ceiling of zero
// derives one from the node's total RAM — one sixteenth of it — and a
// positive ceiling below the floor is raised to the floor, so the node
// never promises less than the smallest buffer it serves. A session cap
// below one resolves to the default of 64. Every correction is silent:
// an under-configured value means the node's own floor, not an error.
func (o *Options) ResolveMemoryLimits(meminfoPath string) {
	const (
		minFloorBytes int64 = 1 << 30
		ramDivisor    int64 = 16
	)
	if meminfoPath == "" {
		meminfoPath = broker.DefaultMemInfoPath
	}
	o.MinMemoryBytes = broker.AlignDown2M(o.MinMemoryBytes)
	if o.MinMemoryBytes < minFloorBytes {
		o.MinMemoryBytes = minFloorBytes
	}
	if o.PhysicalBytes == 0 {
		derived := broker.AlignDown2M(broker.MemInfoTotalBytes(meminfoPath) / ramDivisor)
		if derived < o.MinMemoryBytes {
			derived = o.MinMemoryBytes
		}
		o.PhysicalBytes = derived
	} else {
		o.PhysicalBytes = broker.AlignDown2M(o.PhysicalBytes)
	}
	if o.PhysicalBytes < o.MinMemoryBytes {
		o.PhysicalBytes = o.MinMemoryBytes
	}
	if o.MaxSessions < 1 {
		o.MaxSessions = defaultSessions
	}
}

const (
	defaultKubeConnQPS   = 200
	defaultKubeConnBurst = 400
)

// Config is the completed model-transfer setup on one node.
type Config struct {
	NodeName string
	Store    *store.Store
	Broker   *broker.Broker
	// RestConfig is the rate-limited Kubernetes configuration backing Client and Reader.
	RestConfig *rest.Config
	// Client reads the live source Pod and Kubernetes resources.
	Client kubernetes.Interface
	// WorkerNamespace and WorkerLabel select the source-worker Pods.
	WorkerNamespace string
	WorkerLabel     labels.Selector
	// ControlAddr is where the control service listens.
	ControlAddr string
	// TickEvery is how often leases are swept.
	TickEvery time.Duration
	// SessionAddr and the TLS pair configure the engine session listener.
	// Both certificate paths are required; sessions are never served in plaintext.
	SessionAddr    string
	SessionTLSCert string
	SessionTLSKey  string
	// BootstrapFile is where the broker writes the worker's Pod-local
	// bootstrap credential.
	BootstrapFile string
	// WorkerID names this node's transport worker in admissions.
	WorkerID string
	// VendorScope is the broker's vendor identity in generation tokens
	// and cache references.
	VendorScope string
	// Authorizer admits engine sessions using the live cluster client.
	Authorizer broker.SessionAuthorizer
	// Intents resolves live Kubernetes source preparations for this node.
	Intents          broker.IntentSource
	bootstrapToken   string
	runtimeURL       string
	metadataOnce     sync.Once
	metadataSlots    chan struct{}
	manifestPageRoot string
	liveIntentEpoch  uint64
	liveIntentMu     sync.Mutex
	liveIntentCache  map[broker.LiveIntent]time.Time
	// reconcileFailures counts consecutive failed source-intent passes on
	// the single reconciliation goroutine; it resets on the first pass
	// that lists cleanly. Draining serving generations waits for a
	// sustained failure rather than a single API hiccup.
	reconcileFailures int
	// sessionPodMu guards the short-lived target-Pod liveness cache.
	sessionPodMu    sync.Mutex
	sessionPodCache map[string]sessionPodSnapshot
}

// reconcileFailureLimit is how many consecutive failed source passes it
// takes before serving generations are drained; a single transient API
// error never invalidates serving readiness.
const reconcileFailureLimit = 3

// sessionPodSnapshot is one namespace's Pod liveness at a point in time.
type sessionPodSnapshot struct {
	live    map[string]bool
	expires time.Time
}

// sessionPodCacheTTL bounds how long a namespace's Pod liveness snapshot
// is reused before the namespace is listed again.
const sessionPodCacheTTL = 5 * time.Second

// Complete opens the store and the broker and connects the cluster client.
func (o *Options) Complete() (*Config, error) {
	_, restCfg, _, err := kubeconfig.LoadRestConfigNonInteractive()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	return o.complete(restCfg)
}

func (o *Options) complete(restCfg *rest.Config) (*Config, error) {
	if restCfg == nil {
		return nil, errors.New("kubernetes configuration is required")
	}
	o.ResolveMemoryLimits(broker.DefaultMemInfoPath)
	cfgCopy := rest.CopyConfig(restCfg)
	if o.KubeConnTimeout > 0 {
		cfgCopy.Timeout = o.KubeConnTimeout
	}
	if o.KubeContentType != "" {
		cfgCopy.ContentType = o.KubeContentType
	}
	if o.KubeConnQPS > 0 {
		cfgCopy.QPS = float32(o.KubeConnQPS)
	} else if cfgCopy.QPS <= 0 {
		cfgCopy.QPS = defaultKubeConnQPS
	}
	if o.KubeConnBurst > 0 {
		cfgCopy.Burst = o.KubeConnBurst
	} else if cfgCopy.Burst <= 0 {
		cfgCopy.Burst = defaultKubeConnBurst
	}
	client, err := kubernetes.NewForConfig(cfgCopy)
	if err != nil {
		return nil, fmt.Errorf("construct Kubernetes client: %w", err)
	}
	reader, err := ctrlcli.New(cfgCopy, ctrlcli.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("construct Kubernetes reader: %w", err)
	}
	st, err := store.Open(o.CacheRoot)
	if err != nil {
		return nil, fmt.Errorf("open the cache root %s: %w", o.CacheRoot, err)
	}
	b, err := broker.Open(o.BrokerRoot, o.VendorScope, st, broker.Limits{
		PhysicalBytes: o.PhysicalBytes, MaxGenerations: o.MaxGenerations,
		MaxQuarantined: o.MaxQuarantined, QuarantineBytes: o.QuarantineBytes,
		LeaseTTL: o.LeaseTTL, MinMemoryBytes: o.MinMemoryBytes, MaxSessions: o.MaxSessions,
	}, time.Now)
	if err != nil {
		return nil, fmt.Errorf("open the broker ledger %s: %w", o.BrokerRoot, err)
	}
	manifestRoot := filepath.Join(o.BrokerRoot, "manifest-staging")
	if err := os.RemoveAll(manifestRoot); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("clean manifest staging: %w", err)
	}
	if err := b.EnableOperationJournal(); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("initialize operation journal: %w", err)
	}
	if err := cleanupMetadataRequests(st, o.WorkerID, o.VendorScope); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("recover metadata requests: %w", err)
	}
	if err := b.RequireAuthorityInventory(o.NodeName); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("initialize authority inventory: %w", err)
	}
	cfg := &Config{
		manifestPageRoot: manifestRoot,
		NodeName:         o.NodeName, Store: st, Broker: b, RestConfig: cfgCopy, Client: client,
		WorkerNamespace: o.WorkerNamespace, WorkerLabel: SourceWorkerLabel,
		ControlAddr: o.ControlAddr, TickEvery: o.LeaseTTL / 2,
		SessionAddr: o.SessionAddr, SessionTLSCert: o.SessionTLSCert,
		SessionTLSKey: o.SessionTLSKey, BootstrapFile: o.BootstrapFile,
		WorkerID: o.WorkerID, VendorScope: o.VendorScope,
		Authorizer: &authorization.Authorizer{Reader: reader, Reviewer: tokenReviewer{client: client}, Store: st, Resolver: b, CacheTTL: 10 * time.Second},
	}
	b.SetAssignmentsFrozen(true)
	cfg.runtimeURL = "http://127.0.0.1:9252/v1/runtime"
	cfg.Intents = &kubernetesIntentSource{config: cfg, client: reader}
	b.SetIntentValidator(cfg.validateLiveIntent)
	b.SetSessionPodChecker(cfg.isSessionPodLive)
	return cfg, nil
}

type tokenReviewer struct{ client kubernetes.Interface }

func (r tokenReviewer) Create(ctx context.Context, review *authentication.TokenReview, opts meta.CreateOptions) (*authentication.TokenReview, error) {
	return r.client.AuthenticationV1().TokenReviews().Create(ctx, review, opts)
}

// Close releases the broker's ledger lock and the store's resources.
func (c *Config) Close() error {
	if c.Broker != nil {
		if err := c.Broker.Close(); err != nil {
			return err
		}
	}
	return nil
}

// writeBootstrapCredential mints the Pod-local worker bootstrap token and
// writes it into the shared emptyDir file at 0600, creating the parent
// directory at 0700. The token authenticates the node's worker container
// on the loopback control wire; it never appears in logs or status.
func (c *Config) writeBootstrapCredential() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint the bootstrap credential: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(c.BootstrapFile), 0o700); err != nil {
		return "", fmt.Errorf("prepare the bootstrap directory: %w", err)
	}
	if err := os.WriteFile(c.BootstrapFile, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("write the bootstrap credential: %w", err)
	}
	return token, nil
}

// LiveWorkers answers which admitted source-worker Pods exist right now. An
// answer that cannot be read is an error, never an empty set in its place:
// even a readable empty set retains native ownership and only invalidates readiness.
func (c *Config) LiveWorkers(ctx context.Context) (map[string]bool, error) {
	if c.Client == nil {
		return nil, errors.New("no cluster client is connected")
	}
	pods, err := c.Client.CoreV1().Pods(c.WorkerNamespace).List(ctx,
		meta.ListOptions{LabelSelector: c.WorkerLabel.String()})
	if err != nil {
		return nil, fmt.Errorf("list the source workers: %w", err)
	}
	live := make(map[string]bool, len(pods.Items))
	for _, pod := range pods.Items {
		if c.NodeName != "" && pod.Spec.NodeName != c.NodeName {
			continue
		}
		live[string(pod.UID)] = pod.UID != "" && pod.DeletionTimestamp == nil &&
			pod.Status.Phase != core.PodSucceeded &&
			pod.Status.Phase != core.PodFailed
	}
	return live, nil
}
