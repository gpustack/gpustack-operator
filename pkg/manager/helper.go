package manager

import (
	"context"
	"errors"
	"net/http"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlapiutil "sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	ctrlctrl "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"

	"gpustack.ai/gpustack/pkg/webserver"
)

type (
	// CtrlManager is a wrapper around ctrl.Manager.
	CtrlManager struct {
		ctrl.Manager
		aggressiveEventFiltering bool
		options                  ctrl.Options
		httpClient               *http.Client
		disableController        bool
		indexedFields            sets.Set[string]
	}

	// RepeatableCtrlFieldIndexer is a wrapper around ctrlcli.FieldIndexer.
	RepeatableCtrlFieldIndexer struct {
		ctrl.Manager
		indexedFields sets.Set[string]
	}
)

// GetHTTPClient implements the controller manager interface to returns the singleton HTTP client of the system.
func (m CtrlManager) GetHTTPClient() *http.Client {
	if m.httpClient != nil {
		return m.httpClient
	}
	return m.Manager.GetHTTPClient()
}

// Start implements the controller manager interface to avoid function ambiguity.
func (m CtrlManager) Start(ctx context.Context) error {
	return m.Manager.Start(ctx)
}

// Add implements the controller manager interface to add a controller to the manager.
//
// Add skips controllers who need leader election, when specifies with disableController.
func (m CtrlManager) Add(r ctrlmgr.Runnable) error {
	if m.disableController {
		// If cache is disabled, skip controllers who need leader election.
		l, ok := r.(ctrlmgr.LeaderElectionRunnable)
		if ok && l.NeedLeaderElection() {
			if _, ok := r.(ctrlctrl.Controller); ok {
				return nil
			}
		}
	}
	return m.Manager.Add(r)
}

// GetFieldIndexer implements the controller manager interface to returns the repeatable field indexer.
//
// GetFieldIndexer warns up if the same field is indexed multiple times.
func (m CtrlManager) GetFieldIndexer() ctrlcli.FieldIndexer {
	return RepeatableCtrlFieldIndexer{
		Manager:       m.Manager,
		indexedFields: m.indexedFields,
	}
}

func (i RepeatableCtrlFieldIndexer) IndexField(ctx context.Context, obj ctrlcli.Object, field string, extractValue ctrlcli.IndexerFunc) error {
	logger := ctrllog.FromContext(ctx)
	gvk, err := ctrlapiutil.GVKForObject(obj, i.GetScheme())
	if err != nil {
		return err
	}
	key := gvk.String() + "/" + field
	if i.indexedFields.Has(key) {
		// If the field is already indexed, skip.
		logger.InfoDepth(1, "field is already indexed, skipping", "field", field, "gvk", gvk)
		return nil
	}
	i.indexedFields.Insert(key)
	return i.Manager.GetFieldIndexer().IndexField(ctx, obj, field, extractValue)
}

// HostPort returns the host and port of the manager's server listener.
func (m CtrlManager) HostPort() (string, int, error) {
	ms := m.GetWebhookServer()
	if ms == nil {
		return "", 0, errors.New("manager does not have a webhook server")
	}
	s, ok := ms.(webserver.Server)
	if !ok {
		return "", 0, errors.New("invalid webhook server type")
	}
	return s.HostPort()
}

// GetLeaderElectionNamespacedName returns the namespaced name for leader election.
func (m CtrlManager) GetLeaderElectionNamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: m.options.LeaderElectionNamespace,
		Name:      m.options.LeaderElectionID,
	}
}

// AllowAggressiveEventFiltering returns whether aggressive event filtering is allowed.
func (m CtrlManager) AllowAggressiveEventFiltering() bool {
	return m.aggressiveEventFiltering
}

// _CtrlManagerSentinel is a ctrlmgr.Runnable implementation for observing
// whether the ctrl.Manager is started.
type _CtrlManagerSentinel struct {
	done chan struct{}
}

func (s _CtrlManagerSentinel) Start(ctx context.Context) error {
	close(s.done)
	<-ctx.Done()
	return ctx.Err()
}

func (s _CtrlManagerSentinel) NeedLeaderElection() bool {
	return false
}

func (s _CtrlManagerSentinel) Done() <-chan struct{} {
	return s.done
}
