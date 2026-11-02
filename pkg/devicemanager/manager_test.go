package devicemanager

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"gpustack.ai/gpustack/pkg/devicemanager/allocator"
	"gpustack.ai/gpustack/pkg/devicemanager/controllers"
	"gpustack.ai/gpustack/pkg/devicemanager/detector"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/manager"
)

// _FakeCtrlManager stands in for the controller manager. Only the three methods below are reached
// before the controller setup fails, so the rest stays the embedded nil interface — the shape
// pkg/manager's own tests use for the same reason.
//
// Its scheme is empty on purpose. The reconciler's first act is to index a field on a Pod, and a
// scheme that does not know Pods makes that fail, which is how a subsystem is made to fail here
// without a cluster or a fake client. The setup reaches GetScheme on that path, so onSetupStart
// marks the moment controller setup began: a test snapshots whatever it wants proven about the
// state of the world at exactly that moment.
type _FakeCtrlManager struct {
	ctrl.Manager

	onSetupStart func()
}

func (f _FakeCtrlManager) GetScheme() *runtime.Scheme {
	if f.onSetupStart != nil {
		f.onSetupStart()
	}
	return runtime.NewScheme()
}

func (_FakeCtrlManager) GetWebhookServer() ctrlwebhook.Server {
	return ctrlwebhook.NewServer(ctrlwebhook.Options{})
}

// _failingWebhookServer is a webhook server whose start fails once the given channel carries the
// detector's first publish, so the failure provably lands while another subsystem is running
// rather than before any of them has started.
type _failingWebhookServer struct {
	waitForDetectorStart <-chan sets.Set[string]
}

func (_failingWebhookServer) NeedLeaderElection() bool          { return false }
func (_failingWebhookServer) Register(_ string, _ http.Handler) {}

func (s _failingWebhookServer) Start(ctx context.Context) error {
	select {
	case <-s.waitForDetectorStart:
	case <-ctx.Done():
		return ctx.Err()
	}
	return errors.New("the controller manager is failing on request")
}

func (_failingWebhookServer) StartedChecker() healthz.Checker {
	return func(_ *http.Request) error { return nil }
}

func (_failingWebhookServer) WebhookMux() *http.ServeMux { return http.NewServeMux() }

// _idleWebhookServer is a webhook server that serves nothing and binds nothing, so the manager
// under test starts no listener.
type _idleWebhookServer struct{}

func (_idleWebhookServer) NeedLeaderElection() bool          { return false }
func (_idleWebhookServer) Register(_ string, _ http.Handler) {}
func (_idleWebhookServer) Start(ctx context.Context) error   { <-ctx.Done(); return ctx.Err() }
func (_idleWebhookServer) StartedChecker() healthz.Checker {
	return func(_ *http.Request) error { return nil }
}
func (_idleWebhookServer) WebhookMux() *http.ServeMux { return http.NewServeMux() }

// _syncedChannel is the closed channel every fake sync check reports.
var _syncedChannel = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// _syncedChecker reports an already-synced DoneChecker for the fake informer.
type _syncedChecker struct{}

func (_syncedChecker) Name() string          { return "synced" }
func (_syncedChecker) Done() <-chan struct{} { return _syncedChannel }

// _syncedRegistration is an event-handler registration the fake informer reports as synced.
type _syncedRegistration struct{}

func (_syncedRegistration) HasSynced() bool                          { return true }
func (_syncedRegistration) HasSyncedChecker() toolscache.DoneChecker { return _syncedChecker{} }

// _syncedInformer is the informer the synced cache hands back: synced from birth.
type _syncedInformer struct{}

func (_syncedInformer) AddEventHandler(_ toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	return _syncedRegistration{}, nil
}

func (_syncedInformer) AddEventHandlerWithResyncPeriod(_ toolscache.ResourceEventHandler, _ time.Duration) (toolscache.ResourceEventHandlerRegistration, error) {
	return _syncedRegistration{}, nil
}

func (_syncedInformer) AddEventHandlerWithOptions(_ toolscache.ResourceEventHandler, _ toolscache.HandlerOptions) (toolscache.ResourceEventHandlerRegistration, error) {
	return _syncedRegistration{}, nil
}

func (_syncedInformer) RemoveEventHandler(_ toolscache.ResourceEventHandlerRegistration) error {
	return nil
}

func (_syncedInformer) AddIndexers(_ toolscache.Indexers) error  { return nil }
func (_syncedInformer) HasSynced() bool                          { return true }
func (_syncedInformer) HasSyncedChecker() toolscache.DoneChecker { return _syncedChecker{} }
func (_syncedInformer) IsStopped() bool                          { return false }

// _syncedCache stands in for the manager cache: every informer it hands back is synced from
// birth, so the controllers under test start without a cluster and without waiting on one.
type _syncedCache struct{}

func (_syncedCache) Get(_ context.Context, _ ctrlcli.ObjectKey, _ ctrlcli.Object, _ ...ctrlcli.GetOption) error {
	return errors.New("the synced cache serves no reads")
}

func (_syncedCache) List(_ context.Context, _ ctrlcli.ObjectList, _ ...ctrlcli.ListOption) error {
	return errors.New("the synced cache serves no reads")
}

func (_syncedCache) GetInformer(_ context.Context, _ ctrlcli.Object, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	return _syncedInformer{}, nil
}

func (_syncedCache) GetInformerForKind(_ context.Context, _ schema.GroupVersionKind, _ ...ctrlcache.InformerGetOption) (ctrlcache.Informer, error) {
	return _syncedInformer{}, nil
}

func (_syncedCache) RemoveInformer(_ context.Context, _ ctrlcli.Object) error { return nil }

func (_syncedCache) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (_syncedCache) WaitForCacheSync(_ context.Context) bool { return true }

func (_syncedCache) IndexField(_ context.Context, _ ctrlcli.Object, _ string, _ ctrlcli.IndexerFunc) error {
	return nil
}

// _controllerLogSink is a logr sink that records every message logged through the manager under
// test, so a setup control can wait on controller-reported evidence instead of elapsed time.
type _controllerLogSink struct {
	mu       sync.Mutex
	messages []string
}

func (s *_controllerLogSink) Init(logr.RuntimeInfo) {}

func (s *_controllerLogSink) Enabled(int) bool { return true }

func (s *_controllerLogSink) Info(_ int, msg string, _ ...any) { s.append(msg) }

func (s *_controllerLogSink) Error(_ error, msg string, _ ...any) { s.append(msg) }

func (s *_controllerLogSink) WithName(_ string) logr.LogSink { return s }

func (s *_controllerLogSink) WithValues(_ ...any) logr.LogSink { return s }

func (s *_controllerLogSink) append(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
}

func (s *_controllerLogSink) saw(msg string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i] == msg {
			return true
		}
	}
	return false
}

// _testManager is the throwaway controller-runtime manager the tests that need a setup which
// SUCCEEDS build: it binds no listener, serves no reads from its cache, and skips controller
// name validation, because the controller names land in a process-global registry on the first
// setup and a second registration anywhere in the package would fail validation against it.
// It returns the wrapped form the component code takes, so the repeatable field indexer works.
func _testManager(t *testing.T, cache ctrlcache.Cache, logger logr.Logger, webhook ctrlwebhook.Server) manager.CtrlManager {
	t.Helper()
	skipNameValidation := true
	rawMgr, err := ctrlmgr.New(&rest.Config{Host: "localhost:1"}, ctrlmgr.Options{
		Scheme:  scheme.Scheme,
		Logger:  logger,
		Metrics: ctrlmetricsserver.Options{BindAddress: "0"},
		NewCache: func(_ *rest.Config, _ ctrlcache.Options) (ctrlcache.Cache, error) {
			return cache, nil
		},
		WebhookServer: webhook,
		Controller:    ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	require.NoError(t, err, "the manager must come up without a cluster")
	return manager.NewCtrlManager(rawMgr)
}

// _noManufacturersDetector builds the detector every test here runs: no manufacturers means no
// detectors, so detection itself is a no-op on any platform, and the tick is an hour away so the
// first pass happens once.
func _noManufacturersDetector(t *testing.T) (*detector.Detector, chan sets.Set[string]) {
	t.Helper()
	published := make(chan sets.Set[string], 1)
	det, err := detector.New(&detector.Config{
		Manufacturers:           sets.New[string](),
		MonitorPeriod:           time.Hour,
		DetectedManufacturersCh: published,
	})
	require.NoError(t, err)
	return det, published
}

// TestManager_StartEndsWhenASubsystemFails pins that the device manager's Start is over as soon
// as one of its parts is: it reports a subsystem that failed rather than waiting on it, and it
// cancels the subsystems that are still running when the failure lands after the group started.
func TestManager_StartEndsWhenASubsystemFails(t *testing.T) {
	// The observer's setup reads the node name off the environment; an empty value keeps the
	// fixtures free of any cluster or client assumption they cannot satisfy.
	t.Setenv("KUBERNETES_NODE_NAME", "")

	t.Run("a controller setup that fails reports before any subsystem starts", func(t *testing.T) {
		// No manufacturers and no manufacturers channel: this fixture never reaches a first
		// pass, because the empty scheme fails the controller setup before the task group that
		// runs the detector is even created.
		det, err := detector.New(&detector.Config{
			Manufacturers: sets.New[string](),
			MonitorPeriod: time.Hour,
		})
		require.NoError(t, err)
		alc, err := allocator.New(&allocator.Config{})
		require.NoError(t, err)

		m := &Manager{
			Manager: &manager.Manager{
				CtrlManager: manager.CtrlManager{Manager: _FakeCtrlManager{}},
			},
			Detector:  det,
			Allocator: alc,
		}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		errCh := make(chan error, 1)
		go func() {
			errCh <- m.Start(ctx)
		}()

		select {
		case err := <-errCh:
			// Reported rather than swallowed: the device manager's caller is what decides the
			// process exits, and it can only decide that if the failure reaches it.
			require.Error(t, err, "a subsystem that failed must be reported, not waited on")
			assert.Contains(t, err.Error(), "setup controllers")
		case <-time.After(30 * time.Second):
			t.Fatal("Start did not return after one of its subsystems failed")
		}
	})

	t.Run("a subsystem that fails while the detector runs cancels it and returns", func(t *testing.T) {
		// The detector's first pass publishes into this channel, and the webhook start waits
		// for exactly that before failing — so by the time the failure lands, the detector
		// provably sits inside its first Start pass and the controller manager inside its own.
		// The task group over both is what has to end this.
		det, published := _noManufacturersDetector(t)
		alc, err := allocator.New(&allocator.Config{})
		require.NoError(t, err)
		rawMgr := _testManager(t, &_syncedCache{}, logr.Discard(),
			_failingWebhookServer{waitForDetectorStart: published})

		m := &Manager{
			Manager: &manager.Manager{
				CtrlManager: rawMgr,
			},
			Detector:  det,
			Allocator: alc,
		}

		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		errCh := make(chan error, 1)
		go func() {
			errCh <- m.Start(ctx)
		}()

		select {
		case err := <-errCh:
			require.Error(t, err, "a subsystem that failed must be reported, not waited on")
			assert.Contains(t, err.Error(), "the controller manager is failing on request")
		case <-time.After(30 * time.Second):
			t.Fatal("Start did not return after a running subsystem failed")
		}
	})
}

// TestManager_StartWiresRDMASelectionInventory pins that Start attaches an inventory source on
// the RDMA selection observer, links the observer to the devices reconciler, and hands the
// detector's change callback to the observer — all inside controller setup, before any subsystem
// runs: the snapshot below is taken from inside the setup itself, not after Start returns.
//
// The observer reads the detector's observation through no other path, and an unwired source
// publishes its inventory as unknown forever — nothing fails, so the regression stays silent
// unless pinned here. The callback is the same kind of silence: a callback wired after the
// detector's first pass races that pass and drops the change it fires, because the observer's
// event channel only exists once the controllers are set up.
func TestManager_StartWiresRDMASelectionInventory(t *testing.T) {
	// The inventory source lives on a process-global singleton, so a wiring left behind by an
	// earlier test would satisfy this one on its own: start from nothing, and give back whatever
	// was here once done.
	prior := controllers.Get[*deviceplugin.RDMASelectionObserver]().RDMAInterfaceInventory
	priorRec := controllers.Get[*deviceplugin.RDMASelectionObserver]().Reconciler
	priorRDMAInv := controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory
	t.Cleanup(func() {
		controllers.Get[*deviceplugin.RDMASelectionObserver]().RDMAInterfaceInventory = prior
		controllers.Get[*deviceplugin.RDMASelectionObserver]().Reconciler = priorRec
		controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory = priorRDMAInv
	})
	controllers.Get[*deviceplugin.RDMASelectionObserver]().RDMAInterfaceInventory = nil
	controllers.Get[*deviceplugin.RDMASelectionObserver]().Reconciler = nil
	controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory = nil

	// The same fixture as the synchronous subtest of TestManager_StartEndsWhenASubsystemFails:
	// controller setup fails on the empty scheme, and the assignment this test pins sits before
	// that setup, so a returned Start proves the run reached the wiring point.
	t.Setenv("KUBERNETES_NODE_NAME", "")

	det, detectorStartedCh := _noManufacturersDetector(t)
	alc, err := allocator.New(&allocator.Config{})
	require.NoError(t, err)

	var m *Manager
	wiredAtSetup := false
	var detectorStartedAtSetup bool
	fake := _FakeCtrlManager{onSetupStart: func() {
		obs := controllers.Get[*deviceplugin.RDMASelectionObserver]()
		wiredAtSetup = obs.RDMAInterfaceInventory != nil &&
			obs.Reconciler != nil &&
			controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory != nil &&
			m.Detector.OnRDMAInterfacesChanged != nil
		select {
		case <-detectorStartedCh:
			detectorStartedAtSetup = true
		default:
			detectorStartedAtSetup = false
		}
	}}

	m = &Manager{
		Manager: &manager.Manager{
			CtrlManager: manager.CtrlManager{Manager: fake},
		},
		Detector:  det,
		Allocator: alc,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- m.Start(ctx)
	}()

	select {
	case err := <-errCh:
		require.Error(t, err, "a controller setup that failed must be reported, not waited on")
		assert.Contains(t, err.Error(), "setup controllers")
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after its controller setup failed")
	}

	// The snapshot from inside the setup: the four wirings were already in place when the setup
	// began, and the detector had not started — wiring and setup run to completion in Start's
	// own goroutine, and the group that starts the detector is created only after the setup has
	// succeeded.
	require.True(t, wiredAtSetup,
		"Start must wire the inventory sources, the reconciler link, and the detector's change callback before controller setup runs")
	require.False(t, detectorStartedAtSetup,
		"the detector must not have started when controller setup ran")

	// After Start returns with the setup error, the wirings stand, and the detector still never
	// ran: nothing was launched behind the dead setup.
	wired := controllers.Get[*deviceplugin.RDMASelectionObserver]().RDMAInterfaceInventory
	require.NotNil(t, wired,
		"Start must wire the RDMA selection observer's inventory source before controller setup")
	require.Same(t, controllers.Get[*deviceplugin.DevicesReconciler](),
		controllers.Get[*deviceplugin.RDMASelectionObserver]().Reconciler,
		"Start must wire the RDMA selection observer's reconciler before controller setup")
	require.NotNil(t, controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory,
		"Start must wire the devices reconciler's RDMA inventory source before controller setup")
	require.NotNil(t, m.Detector.OnRDMAInterfacesChanged,
		"Start must hand the detector's change callback to the RDMA selection observer before controller setup")
	select {
	case <-detectorStartedCh:
		t.Fatal("the detector must not start when its manager's controller setup has failed")
	default:
	}
}

// TestManager_StartArmsRDMASelectionEventsBeforeDetectorRuns pins the activation link end to
// end: with a controller setup that SUCCEEDS, the detector's change callback reaches the armed
// observer while the detector is running — the callback only lands once the setup has armed the
// observer's event channel, and the detector only starts once the same setup has returned, so
// the two orderings below are the same fact seen from both ends.
func TestManager_StartArmsRDMASelectionEventsBeforeDetectorRuns(t *testing.T) {
	// The inventory source lives on a process-global singleton: start from nothing, and give
	// back whatever was here once done, exactly as the wiring test above does.
	prior := controllers.Get[*deviceplugin.RDMASelectionObserver]().RDMAInterfaceInventory
	priorRec := controllers.Get[*deviceplugin.RDMASelectionObserver]().Reconciler
	priorRDMAInv := controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory
	t.Cleanup(func() {
		controllers.Get[*deviceplugin.RDMASelectionObserver]().RDMAInterfaceInventory = prior
		controllers.Get[*deviceplugin.RDMASelectionObserver]().Reconciler = priorRec
		controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory = priorRDMAInv
	})
	controllers.Get[*deviceplugin.RDMASelectionObserver]().RDMAInterfaceInventory = nil
	controllers.Get[*deviceplugin.RDMASelectionObserver]().Reconciler = nil
	controllers.Get[*deviceplugin.DevicesReconciler]().RDMAInterfaceInventory = nil

	t.Setenv("KUBERNETES_NODE_NAME", "")

	det, detectorStartedCh := _noManufacturersDetector(t)
	alc, err := allocator.New(&allocator.Config{})
	require.NoError(t, err)

	sink := new(_controllerLogSink)
	rawMgr := _testManager(t, &_syncedCache{}, logr.New(sink), _idleWebhookServer{})

	m := &Manager{
		Manager: &manager.Manager{
			CtrlManager: rawMgr,
		},
		Detector:  det,
		Allocator: alc,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- m.Start(ctx)
	}()

	// The detector's first pass reports here; the group that runs it is created only after the
	// setup — which is what arms the observer — has returned, so this signal arriving pins that
	// the arming preceded Detector.Start.
	select {
	case <-detectorStartedCh:
	case <-time.After(30 * time.Second):
		t.Fatal("the detector did not start")
	}

	// The internal controller logs "Starting workers" only after every registered source has
	// started and synced, so by the time the line lands the observer's controller consumes the
	// channel the setup armed.
	require.Eventually(t, func() bool {
		return sink.saw("Starting workers")
	}, 30*time.Second, 10*time.Millisecond,
		"the started controller must report that its sources have started")

	// The wired callback reaches the armed observer: firing it enqueues a reconcile pass for
	// this node's Devices, which the recording cache answers with a read error the pass logs.
	// An un-armed channel would swallow the notification and the line would never land.
	m.Detector.OnRDMAInterfacesChanged()
	require.Eventually(t, func() bool {
		return sink.saw("fetch devices")
	}, 30*time.Second, 10*time.Millisecond,
		"the detector's change callback must reach the armed RDMA selection observer")

	cancel()
	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}
