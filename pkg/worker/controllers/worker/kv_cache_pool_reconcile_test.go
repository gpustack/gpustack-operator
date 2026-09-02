package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// fakeMaster stands in for one Mooncake master's admin surface, and COUNTS what was asked of it.
//
// The count is the assertion, not decoration: the pass is required to write nothing to a settled
// ledger and to read the exposition once however many Bindings a pool has, and neither is visible in
// the resulting status. A fake that only answered would let both regress silently.
type fakeMaster struct {
	mu sync.Mutex

	// ledger is what the master holds, keyed by tenant. Mutated by the handlers, so a second pass in
	// one test sees what the first one wrote.
	ledger map[string]int64
	// explicit records which entries carry a written policy, which is what separates a tenant this
	// operator wrote a quota for from one running under the master's default.
	explicit map[string]bool

	// allocatable is what the exposition reports the master has to divide. Zero is the
	// startup-ordering trap.
	allocatable int64

	// effective, occupancy and overQuota are the per-tenant figures the exposition reports beyond
	// the requested quota. An entry absent from effective reports the requested figure, which is
	// what an unpressured master does; one present reports what it says, which is how a test
	// oversubscribes the pool without arithmetic of its own.
	effective map[string]int64
	occupancy map[string]int64
	overQuota map[string]bool

	// omitTenants are ledger entries the exposition does NOT mention, standing for a tenant the
	// master has not observed yet. It is a set rather than a flag because one Binding of a pool can
	// be in that state while its sibling is not.
	omitTenants map[string]bool

	// newGeneration serializes v0.3.13's series names instead of 0.3.12.post1's: one charged_bytes
	// in place of used_bytes and reserved_bytes. A backend's image decides which, so the pass has to
	// read either.
	newGeneration bool

	// refuse, when set, is written instead of any tenant-quota answer.
	refuseStatus int
	refuseBody   string

	// refuseDelete, when set, is written instead of a tenant-quota REMOVAL only, leaving the listing
	// and the writes answering. It is what a domain that still holds objects looks like, and it has
	// to be narrower than refuse: a pass whose LIST failed never reaches the removal at all.
	refuseDeleteStatus int
	refuseDeleteBody   string

	// refuseScrape, when set, is written instead of the exposition. It is separate from refuse
	// because the ledger and the exposition fail independently: a master can hold a perfectly good
	// ledger and still not answer /metrics.
	refuseScrape int

	puts, deletes, lists, scrapes int

	// onWrite, when set, runs on the FIRST write this master is asked to make, before the ledger is
	// mutated. It is what lets a test observe the cluster at the instant the external state changes
	// — an ordering no resulting status can show, because by the end of a pass both halves are done
	// however they were sequenced.
	onWrite func()
}

func newFakeMaster() *fakeMaster {
	return &fakeMaster{
		ledger:      map[string]int64{},
		explicit:    map[string]bool{},
		effective:   map[string]int64{},
		occupancy:   map[string]int64{},
		overQuota:   map[string]bool{},
		omitTenants: map[string]bool{},
		allocatable: 1 << 40,
	}
}

func (m *fakeMaster) start(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func (m *fakeMaster) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if r.URL.Path == "/metrics" {
		m.scrapes++
		if m.refuseScrape != 0 {
			w.WriteHeader(m.refuseScrape)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "mooncake_tenant_quota_allocatable_capacity_bytes %d\n", m.allocatable)
		for tenant, quota := range m.ledger {
			if m.omitTenants[tenant] {
				continue
			}
			effective, ok := m.effective[tenant]
			if !ok {
				effective = quota
			}
			fmt.Fprintf(w, "mooncake_tenant_quota_requested_bytes{tenant_id=%q} %d\n", tenant, quota)
			fmt.Fprintf(w, "mooncake_tenant_quota_effective_bytes{tenant_id=%q} %d\n", tenant, effective)
			fmt.Fprintf(w, "mooncake_tenant_quota_over_quota{tenant_id=%q} %d\n",
				tenant, boolSample(m.overQuota[tenant]))
			fmt.Fprintf(w, "mooncake_tenant_quota_explicit_policy{tenant_id=%q} %d\n",
				tenant, boolSample(m.explicit[tenant]))
			if m.newGeneration {
				fmt.Fprintf(w, "mooncake_tenant_quota_charged_bytes{tenant_id=%q} %d\n",
					tenant, m.occupancy[tenant])
				fmt.Fprintf(w, "mooncake_tenant_quota_admission_closed{tenant_id=%q} 0\n", tenant)
				continue
			}
			fmt.Fprintf(w, "mooncake_tenant_quota_used_bytes{tenant_id=%q} %d\n",
				tenant, m.occupancy[tenant])
			fmt.Fprintf(w, "mooncake_tenant_quota_reserved_bytes{tenant_id=%q} 0\n", tenant)
		}
		return
	}

	// Checked before the blanket refusal, because it is the narrower one: a master that answers its
	// ledger perfectly and refuses only the removal is the TENANT_NOT_EMPTY state, and a test that
	// refused the LIST as well would never reach the code that reads the removal's outcome.
	if m.refuseDeleteStatus != 0 && r.Method == http.MethodDelete {
		w.WriteHeader(m.refuseDeleteStatus)
		_, _ = w.Write([]byte(m.refuseDeleteBody))
		return
	}

	if m.refuseStatus != 0 && r.Method != http.MethodGet {
		w.WriteHeader(m.refuseStatus)
		_, _ = w.Write([]byte(m.refuseBody))
		return
	}

	tenant := r.URL.Query().Get("tenant_id")
	switch r.Method {
	case http.MethodGet:
		if m.refuseStatus != 0 {
			w.WriteHeader(m.refuseStatus)
			_, _ = w.Write([]byte(m.refuseBody))
			return
		}
		m.lists++
		entries := make([]string, 0, len(m.ledger))
		for name, quota := range m.ledger {
			entries = append(entries, fmt.Sprintf(
				`{"tenant_id":%q,"requested_quota_bytes":%d,"effective_quota_bytes":%d,`+
					`"used_bytes":0,"reserved_bytes":0,"committed_count":0,`+
					`"metadata_object_count":0,"over_quota":false,"has_explicit_policy":%t}`,
				name, quota, quota, m.explicit[name]))
		}
		fmt.Fprintf(w, `{"success":true,"data":[%s]}`, strings.Join(entries, ","))
	case http.MethodPut:
		if m.onWrite != nil {
			m.onWrite()
			m.onWrite = nil
		}
		m.puts++
		body := struct {
			RequestedQuotaBytes int64 `json:"requested_quota_bytes"`
		}{}
		_ = decodeJSON(r, &body)
		m.ledger[tenant] = body.RequestedQuotaBytes
		m.explicit[tenant] = true
		fmt.Fprintf(w, `{"success":true,"data":{"tenant_id":%q,"requested_quota_bytes":%d,`+
			`"effective_quota_bytes":%d,"used_bytes":0,"reserved_bytes":0,"committed_count":0,`+
			`"metadata_object_count":0,"over_quota":false,"has_explicit_policy":true}}`,
			tenant, body.RequestedQuotaBytes, body.RequestedQuotaBytes)
	case http.MethodDelete:
		m.deletes++
		delete(m.ledger, tenant)
		delete(m.explicit, tenant)
		_, _ = w.Write([]byte(`{"success":true,"data":null}`))
	}
}

// quantityValue is the byte count a quantity renders as, which is what the ledger carries.
// boolSample is how a Prometheus exposition spells a flag.
func boolSample(b bool) int {
	if b {
		return 1
	}
	return 0
}

func quantityValue(s string) int64 {
	q := resource.MustParse(s)
	return q.Value()
}

func decodeJSON(r *http.Request, into any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

// refuse makes every tenant-quota answer the given status and body, taking the same lock the handler
// reads them under.
//
// The handler runs on the server's own goroutine, so a test assigning these fields directly is racing
// it. The HTTP round trip happens to order the two today; nothing in the test says it has to, and a
// race that only appears under load is one this suite would report as a flake somewhere else.
func (m *fakeMaster) refuse(status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.refuseStatus, m.refuseBody = status, body
}

// refuseDeletes refuses only the removals, leaving the listing and the writes answering.
func (m *fakeMaster) refuseDeletes(status int, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.refuseDeleteStatus, m.refuseDeleteBody = status, body
}

// refuseScrapeWith is the same for the exposition, which fails independently of the ledger.
func (m *fakeMaster) refuseScrapeWith(status int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.refuseScrape = status
}

func (m *fakeMaster) counts() (puts, deletes, lists, scrapes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts, m.deletes, m.lists, m.scrapes
}

func (m *fakeMaster) held() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]int64, len(m.ledger))
	for k, v := range m.ledger {
		out[k] = v
	}
	return out
}

// newReconcileBackend builds the backend a pool is reconciled against, already publishing both
// addresses so the pass reaches the ledger.
func newReconcileBackend(name, admin string) *workercore.KVCacheBackend {
	return &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.KVCacheBackendSpec{
			Type: "Mooncake",
			Connection: workercore.KVCacheBackendConnection{
				External: &workercore.KVCacheBackendExternal{
					Endpoints: []workercore.KVCacheBackendEndpoint{
						{Name: workercore.KVCacheBackendEndpointNameClient, Address: "mc.example:50051"},
						{Name: workercore.KVCacheBackendEndpointNameAdmin, Address: admin},
					},
				},
			},
		},
		Status: workercore.KVCacheBackendStatus{
			Endpoints: []workercore.KVCacheBackendEndpoint{
				{Name: workercore.KVCacheBackendEndpointNameClient, Address: "mc.example:50051"},
				{Name: workercore.KVCacheBackendEndpointNameAdmin, Address: admin},
			},
		},
	}
}

func newBoundBinding(namespace, name, pool, domain string, ceiling resource.Quantity) *workercore.KVCachePoolBinding {
	kvcpb := newTestKVCachePoolBinding(namespace, name, pool)
	kvcpb.Spec.Domain.Name = domain
	kvcpb.Spec.QuotaCeiling = ceiling
	return kvcpb
}

func newReconciler(objs ...ctrlcli.Object) (*KVCachePoolReconciler, ctrlcli.Client) {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		// All three, because all three CRDs carry one. The fake answers a status write against a
		// subresource it does not know about with a not-found, so an omission here reads as a missing
		// object rather than as a missing registration — and the backend is written by this
		// reconciler too, when a pool claims it.
		WithStatusSubresource(&workercore.KVCachePool{}, &workercore.KVCachePoolBinding{},
			&workercore.KVCacheBackend{}).
		WithIndex(&workercore.KVCachePoolBinding{},
			IndexingKVCachePoolBindingByPool, indexKVCachePoolBindingByPool).
		WithIndex(&workercore.KVCachePool{},
			IndexingKVCachePoolByBackend, indexKVCachePoolByBackend).
		WithObjects(objs...).
		Build()
	return &KVCachePoolReconciler{Client: cli, AdminHTTP: newAdminHTTPClient()}, cli
}

func reconcilePool(t *testing.T, r *KVCachePoolReconciler, name string) {
	t.Helper()

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: name},
	})
	require.NoError(t, err)
}

func readPool(t *testing.T, cli ctrlcli.Client, name string) *workercore.KVCachePool {
	t.Helper()

	kvcp := new(workercore.KVCachePool)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: name}, kvcp))
	return kvcp
}

// TestKVCachePoolReconcile_ConvergesOneMaster is the whole of T8 in one pass: the two addresses, the
// policy document, and the ledger.
func TestKVCachePoolReconcile_ConvergesOneMaster(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	t.Run("only the client address is republished", func(t *testing.T) {
		kvcp := readPool(t, cli, "shared")
		assert.Equal(t, "mc.example:50051", kvcp.Status.ClientEndpoint)
		assert.NotContains(t, kvcp.Status.ClientEndpoint, address,
			"the admin port is the write face of the ledger and is republished nowhere")
	})

	t.Run("every ceiling reaches the ledger verbatim", func(t *testing.T) {
		assert.Equal(t, map[string]int64{
			"team-a-chat":  quantityValue("20Ti"),
			"team-b-batch": quantityValue("10Ti"),
		}, master.held(), "no division anywhere: a ceiling is that tenant's own request")
	})

	t.Run("the policy document holds the same tenants", func(t *testing.T) {
		cm := new(core.ConfigMap)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{
			Name:      "mooncake-dram-tenant-quota-policy",
			Namespace: kuberess.SystemNamespaceName,
		}, cm))

		document := cm.Data[mooncake.QuotaPolicyFileName]
		assert.Contains(t, document, "version: 1",
			"omitting it aborts the master's loader rather than erroring")
		assert.Contains(t, document, "team-a-chat")
		assert.Contains(t, document, "team-b-batch")
	})

	t.Run("the registry names both domains and the bindings that declared them", func(t *testing.T) {
		kvcp := readPool(t, cli, "shared")
		require.Len(t, kvcp.Status.Domains, 2)
		assert.Equal(t, "team-a-chat", kvcp.Status.Domains[0].Name)
		assert.Equal(t, workercore.KVCachePoolBindingReference{Namespace: "team-a", Name: "chat"},
			kvcp.Status.Domains[0].Binding)
		assert.Equal(t, int32(16), kvcp.Status.Domains[0].BlockSize)
		assert.Equal(t, "bfloat16", kvcp.Status.Domains[0].Dtype)
	})

	t.Run("the pool reports ready", func(t *testing.T) {
		assert.Equal(t, KVCachePoolPhaseReady, readPool(t, cli, "shared").Status.Phase)
	})
}

// TestKVCachePoolReconcile_SettledMasterTakesNoWrite is the call-counting assertion the spec asks
// for. Nothing in the resulting status would show a pass that rewrote every entry it already agreed
// with, and the cost is one write per tenant per pass, forever.
func TestKVCachePoolReconcile_SettledMasterTakesNoWrite(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, _ := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	puts, deletes, _, _ := master.counts()
	require.Equal(t, 1, puts, "the first pass writes the entry")
	require.Zero(t, deletes)

	reconcilePool(t, r, "shared")
	reconcilePool(t, r, "shared")

	puts, deletes, _, scrapes := master.counts()
	assert.Equal(t, 1, puts, "a settled ledger takes no further write")
	assert.Zero(t, deletes)
	assert.Equal(t, 3, scrapes, "the exposition is still read every pass: its figures move on their own")
}

// TestKVCachePoolReconcile_SharedBackendCoversBothPools is F7. A pass that saw only its own pool
// would write a policy file holding half the master and delete the other half's ledger entries.
func TestKVCachePoolReconcile_SharedBackendCoversBothPools(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newTestKVCachePool("research", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-c", "eval", "research", "team-c-eval", resource.MustParse("5Ti")),
	)

	// Reconciling ONE pool converges the whole master, because the ledger is the master's.
	reconcilePool(t, r, "shared")

	assert.Equal(t, map[string]int64{
		"team-a-chat": quantityValue("20Ti"),
		"team-c-eval": quantityValue("5Ti"),
	}, master.held(), "the other pool's tenant is written, not erased")

	_, deletes, _, _ := master.counts()
	assert.Zero(t, deletes, "and never deleted: an entry another pool's Binding owns is not ours to remove")

	t.Run("each pool's registry holds only its own domains", func(t *testing.T) {
		shared := readPool(t, cli, "shared")
		require.Len(t, shared.Status.Domains, 1)
		assert.Equal(t, "team-a-chat", shared.Status.Domains[0].Name)
	})

	// And the second pool's own pass agrees rather than undoing it.
	reconcilePool(t, r, "research")
	puts, deletes, _, _ := master.counts()
	assert.Equal(t, 2, puts, "the second pool's pass finds both entries already right")
	assert.Zero(t, deletes)
}

// TestKVCachePoolReconcile_EveryBindingIsLockedBeforeTheMasterIsWritten pins the ordering that keeps
// a ledger entry deletable.
//
// An entry can only be removed by name, and convergeTenantLedger removes only what master.registered
// proves this operator wrote — a set built from the pools' published domains and from the live
// Bindings. A Binding that goes away before either records its domain therefore takes the last record
// of its entry with it, and the entry survives every later pass. Locking before the write is what
// makes that unreachable; locking after it left a window a crash could land in.
//
// The assertion is taken from INSIDE the write, because by the end of a pass both halves are done
// however they were ordered — a check on the resulting objects passes either way and would pin
// nothing.
//
// The SIBLING pool's Binding is the sharp case: reconciling one pool converges the whole master, so
// this pass writes a ledger entry for a Binding it does not own, and guarding only its own Bindings
// would leave exactly that one unprotected.
func TestKVCachePoolReconcile_EveryBindingIsLockedBeforeTheMasterIsWritten(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newTestKVCachePool("research", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-c", "eval", "research", "team-c-eval", resource.MustParse("5Ti")),
	)

	// Guarded, and read back under the same lock: the hook runs on the server's goroutine.
	var (
		mu       sync.Mutex
		atWrite  map[string]bool
		observed error
	)
	master.onWrite = func() {
		mu.Lock()
		defer mu.Unlock()

		atWrite = map[string]bool{}
		for _, ref := range []ctrlcli.ObjectKey{
			{Namespace: "team-a", Name: "chat"},
			{Namespace: "team-c", Name: "eval"},
		} {
			kvcpb := new(workercore.KVCachePoolBinding)
			if err := cli.Get(context.Background(), ref, kvcpb); err != nil {
				observed = err
				return
			}
			atWrite[ref.String()] = systemmeta.IsLocked(kvcpb)
		}
	}

	reconcilePool(t, r, "shared")

	mu.Lock()
	defer mu.Unlock()

	require.NoError(t, observed)
	require.Len(t, atWrite, 2, "the master was never written, so the ordering was never observed")
	assert.True(t, atWrite["team-a/chat"],
		"this pool's own Binding must already hold its finalizer when the master is written")
	assert.True(t, atWrite["team-c/eval"],
		"and the sibling pool's: this pass writes its ledger entry, so this pass owes it a finalizer")
}

// TestKVCachePoolReconcile_DeletesOnlyWhatItRegistered is the guard on the delete path.
//
// An external master may well be serving tenants nobody in this cluster created. The ledger carries
// no label saying whose an entry is, so "not asked for by any Binding" is not enough to delete on —
// what this operator itself published in status.domains is.
func TestKVCachePoolReconcile_DeletesOnlyWhatItRegistered(t *testing.T) {
	master := newFakeMaster()
	master.ledger["someone-elses"] = 1 << 30
	master.explicit["someone-elses"] = true
	master.ledger["team-a-chat"] = 1 << 30
	master.explicit["team-a-chat"] = true
	address := master.start(t)

	pool := newTestKVCachePool("shared", "mooncake-dram")
	// What the previous pass published: this operator registered team-a-chat and nothing else.
	pool.Status.Domains = []workercore.KVCachePoolDomain{{
		Name:      "team-a-chat",
		Binding:   workercore.KVCachePoolBindingReference{Namespace: "team-a", Name: "chat"},
		BlockSize: 16,
		Dtype:     "bfloat16",
	}}

	// The Binding that asked for it is gone, so its entry is no longer wanted.
	r, _ := newReconciler(newReconcileBackend("mooncake-dram", address), pool)

	reconcilePool(t, r, "shared")

	assert.Equal(t, map[string]int64{"someone-elses": 1 << 30}, master.held(),
		"the entry this operator registered is removed; the one it never did is left alone")
}

// TestKVCachePoolReconcile_UsageSumsThisPoolsOwnTenants pins the reading a shared master makes easy
// to get wrong: a pool's usage is ITS tenants, never the whole ledger it happens to sit on.
func TestKVCachePoolReconcile_UsageSumsThisPoolsOwnTenants(t *testing.T) {
	master := newFakeMaster()
	master.occupancy["team-a-chat"] = quantityValue("2Ti")
	master.occupancy["lab-sweep"] = quantityValue("5Ti")
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newTestKVCachePool("research", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("lab", "sweep", "research", "lab-sweep", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	kvcp := readPool(t, cli, "shared")
	require.NotNil(t, kvcp.Status.Usage, "the scrape answered, so there is a total to publish")
	require.NotNil(t, kvcp.Status.Usage.Total)
	assert.Equal(t, quantityValue("2Ti"), kvcp.Status.Usage.Total.Value(),
		"the sibling pool's 5Ti is on the same master and in the same scrape; a pool that added it "+
			"would report growth caused by another namespace's writes")
}

// TestKVCachePoolReconcile_TheTwoPreconditionsFailLoudly is criterion 11.
func TestKVCachePoolReconcile_TheTwoPreconditionsFailLoudly(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantReason string
		wantSays   string
	}{
		{
			name:       "a master running without multi-tenancy",
			status:     http.StatusConflict,
			body:       `{"success":false,"error_code":-1011,"error_message":"UNAVAILABLE_IN_CURRENT_MODE"}`,
			wantReason: KVCachePoolReasonMultiTenancyDisabled,
			wantSays:   "runs without multi-tenancy",
		},
		{
			name:       "a policy source the master cannot rewrite",
			status:     http.StatusInternalServerError,
			body:       `{"success":false,"error_code":-1503,"error_message":"PERSISTENT_FAIL"}`,
			wantReason: KVCachePoolReasonQuotaPolicyNotWritable,
			wantSays:   "is not writable",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			master := newFakeMaster()
			master.refuse(c.status, c.body)
			address := master.start(t)

			r, cli := newReconciler(
				newReconcileBackend("mooncake-dram", address),
				newTestKVCachePool("shared", "mooncake-dram"),
				newBoundBinding("team-a", "chat", "shared", "team-a-chat",
					resource.MustParse("20Ti")),
			)

			reconcilePool(t, r, "shared")

			kvcp := readPool(t, cli, "shared")
			assert.NotEqual(t, KVCachePoolPhaseReady, kvcp.Status.Phase,
				"neither precondition may be assumed: a pool without it does not report Ready")
			assert.Contains(t, kvcp.Status.PhaseMessage, c.wantSays)

			reasons := make([]string, 0, len(kvcp.Status.Conditions))
			for _, cond := range kvcp.Status.Conditions {
				reasons = append(reasons, cond.Reason)
			}
			assert.Contains(t, reasons, c.wantReason)
		})
	}
}

// TestKVCachePoolReconcile_ZeroAllocatableCapacityHoldsItBack covers the startup-ordering trap: every
// object looks correct, every quota was accepted, and nothing can be written.
func TestKVCachePoolReconcile_ZeroAllocatableCapacityHoldsItBack(t *testing.T) {
	master := newFakeMaster()
	master.allocatable = 0
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")

	kvcp := readPool(t, cli, "shared")
	assert.NotEqual(t, KVCachePoolPhaseReady, kvcp.Status.Phase)
	assert.Contains(t, kvcp.Status.PhaseMessage, "not finished remounting them after the master restarted")
	assert.NotEmpty(t, master.held(), "and the quota was still written: it is the capacity that is missing")
}

// TestKVCachePoolReconcile_AnUnobservedBackendPublishesNoAddress covers the fallback that must not
// exist. A derived Service name that happens to resolve is how a pool would drive the wrong master.
func TestKVCachePoolReconcile_AnUnobservedBackendPublishesNoAddress(t *testing.T) {
	kvcb := newReconcileBackend("mooncake-dram", "unused")
	kvcb.Status.Endpoints = nil

	r, cli := newReconciler(kvcb, newTestKVCachePool("shared", "mooncake-dram"))

	reconcilePool(t, r, "shared")

	kvcp := readPool(t, cli, "shared")
	assert.Empty(t, kvcp.Status.ClientEndpoint)
	assert.NotEqual(t, KVCachePoolPhaseReady, kvcp.Status.Phase)
	assert.Contains(t, kvcp.Status.PhaseMessage, "publishes no")
}

// TestKVCachePoolReconcile_AMissingBackendIsReportedNotRetriedForever covers the backend deleted out
// from under an admitted pool.
func TestKVCachePoolReconcile_AMissingBackendIsReportedNotRetriedForever(t *testing.T) {
	r, cli := newReconciler(newTestKVCachePool("shared", "mooncake-dram"))

	reconcilePool(t, r, "shared")

	kvcp := readPool(t, cli, "shared")
	assert.NotEqual(t, KVCachePoolPhaseReady, kvcp.Status.Phase)
	assert.Contains(t, kvcp.Status.PhaseMessage, `backend "mooncake-dram" does not exist`)
}

// TestKVCachePoolReconcile_ADomainClaimedTwiceIsManagedForNeither is F9's backstop at the ledger.
//
// Admission refuses the second claim, so reaching this means two creates raced one cache. Picking a
// winner here would hand one namespace a ceiling the other one set.
func TestKVCachePoolReconcile_ADomainClaimedTwiceIsManagedForNeither(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "contested", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "contested", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	puts, deletes, _, _ := master.counts()
	assert.Zero(t, puts, "neither ceiling is written")
	assert.Zero(t, deletes, "and the entry, if any, is left exactly as it is")
	assert.Empty(t, readPool(t, cli, "shared").Status.Domains,
		"a contested domain is in no pool's registry")
}

// TestKVCachePoolReconcile_ALedgerFaultDoesNotOutliveItself pins the level-based half of criterion
// 11: the two ledger conditions describe what THIS pass observed, not the union of every pass.
//
// The holder starts from the last written status, so an axis a failure branch does not touch keeps
// its previous conclusion. The two faults exclude one another — a master with no tenant ledger never
// gets as far as refusing to persist a policy — so leaving one standing under the other makes
// summarizeKVCachePool report the fixed fault instead of the live one. It scans the axes in order,
// and QuotaLedgerAvailable comes first, so the stale one is the one an operator is told to act on.
func TestKVCachePoolReconcile_ALedgerFaultDoesNotOutliveItself(t *testing.T) {
	const (
		multiTenancyOff  = `{"success":false,"error_code":-1011,"error_message":"UNAVAILABLE_IN_CURRENT_MODE"}`
		policyUnwritable = `{"success":false,"error_code":-1503,"error_message":"PERSISTENT_FAIL"}`
	)

	// Both directions, because the defect is symmetric and only one half of it shows up in the phase
	// message: summarizeKVCachePool scans QuotaLedgerAvailable first, so a stale POLICY fault under a
	// live ledger fault is invisible there and has to be asserted on the condition itself.
	cases := []struct {
		name         string
		firstStatus  int
		firstBody    string
		secondStatus int
		secondBody   string
		// assertSecondPass reads the pool after the second pass, where the first pass's fault has
		// been fixed and must not still be on the object.
		assertSecondPass func(t *testing.T, kvcp *workercore.KVCachePool)
	}{
		{
			name:         "multi-tenancy is turned on and the policy source is what fails now",
			firstStatus:  http.StatusConflict,
			firstBody:    multiTenancyOff,
			secondStatus: http.StatusInternalServerError,
			secondBody:   policyUnwritable,
			assertSecondPass: func(t *testing.T, kvcp *workercore.KVCachePool) {
				assert.Equal(t, KVCachePoolReasonQuotaPolicyNotWritable,
					conditionReason(t, kvcp, KVCachePoolConditionQuotaPolicyWritable),
					"this pass's own fault is reported")
				assert.True(t, KVCachePoolConditionQuotaLedgerAvailable.IsTrue(kvcp),
					"and the axis the previous pass faulted is re-decided from THIS response rather "+
						"than kept: a master that refuses to persist a policy is one that has a ledger")
				assert.Contains(t, kvcp.Status.PhaseMessage, "is not writable",
					"so the phase names the policy source, which is what an operator has to go and fix")
				assert.NotContains(t, kvcp.Status.PhaseMessage, "multi-tenancy",
					"and not the backend setting that was already turned on")
			},
		},
		{
			name:         "the policy source is fixed and multi-tenancy is what is off now",
			firstStatus:  http.StatusInternalServerError,
			firstBody:    policyUnwritable,
			secondStatus: http.StatusConflict,
			secondBody:   multiTenancyOff,
			assertSecondPass: func(t *testing.T, kvcp *workercore.KVCachePool) {
				assert.Equal(t, KVCachePoolReasonMultiTenancyDisabled,
					conditionReason(t, kvcp, KVCachePoolConditionQuotaLedgerAvailable),
					"this pass's own fault is reported")
				assert.False(t, KVCachePoolConditionQuotaPolicyWritable.IsFalse(kvcp),
					"and the policy axis stops asserting a source this pass never reached: with no "+
						"ledger there is nothing to persist, so 'not writable' is a conclusion the "+
						"previous pass drew and this one cannot stand behind")
				assert.Equal(t, KVCachePoolReasonMultiTenancyDisabled,
					conditionReason(t, kvcp, KVCachePoolConditionQuotaPolicyWritable),
					"it says which observation left it unknown, rather than going silent")
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			master := newFakeMaster()
			master.refuse(c.firstStatus, c.firstBody)
			address := master.start(t)

			r, cli := newReconciler(
				newReconcileBackend("mooncake-dram", address),
				newTestKVCachePool("shared", "mooncake-dram"),
				newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
			)

			reconcilePool(t, r, "shared")
			require.NotEqual(t, KVCachePoolPhaseReady, readPool(t, cli, "shared").Status.Phase,
				"the first pass has to leave a fault for the second one to outlive")

			master.refuse(c.secondStatus, c.secondBody)
			reconcilePool(t, r, "shared")

			c.assertSecondPass(t, readPool(t, cli, "shared"))
		})
	}
}

// TestKVCachePoolReconcile_APoolNamingTwoBackendsIsServedByNeither keeps this pass and
// resolveKVCachePoolBackend agreeing on what a servable pool is.
//
// The backend index carries EVERY entry of spec.backends, so a pool naming two of them is listed
// under both and is a sibling of both. resolveKVCachePoolBackend refuses it outright — reason
// BackendNotSingular, and it claims neither backend — but this pass used to gather its Bindings
// anyway: the ceiling was written into two masters' ledgers by two reconciles, for a pool holding an
// operator-owned record on neither.
func TestKVCachePoolReconcile_APoolNamingTwoBackendsIsServedByNeither(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newReconcileBackend("mooncake-ssd", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		// Admission takes exactly one backend; this is an object written before that rule or around
		// the webhook, which resolveKVCachePoolBackend reconciles rather than panics over.
		newTestKVCachePool("straddling", "mooncake-dram", "mooncake-ssd"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-x", "wide", "straddling", "straddling-domain", resource.MustParse("9Ti")),
	)

	reconcilePool(t, r, "shared")

	assert.NotContains(t, master.held(), "straddling-domain",
		"a pool no backend serves must not have its ceilings written by a sibling's pass")
	assert.Contains(t, master.held(), "team-a-chat",
		"while the pool that IS served by this backend still converges")
	assert.NotContains(t, readQuotaPolicyDocument(t, cli, "mooncake-dram"), "straddling-domain",
		"and the seed says the same thing the ledger does, or a restart would re-create it")
}

// TestKVCachePoolReconcile_ARefusedPolicyReachesTheMasterNotAtAll pins the ordering F6 rests on:
// nothing reaches the master until the whole set has cleared the validator.
//
// The document is rendered from every tenant on the master at once, so ONE tenant the validator
// refuses means the whole document is refused. The ledger, by contrast, is written one entry at a
// time and revalidates nothing — so a pass that logged the refusal and carried on would PUT the
// refused tenant to the master anyway, and leave the ledger holding entries the seed document does
// not. Both halves of that are what this asserts: the pass fails, and the master is untouched.
//
// A name starting with "_" is what the validator refuses; admission refuses it too, so reaching this
// state means an object written around the webhook.
func TestKVCachePoolReconcile_ARefusedPolicyReachesTheMasterNotAtAll(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, _ := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "_reserved", resource.MustParse("20Ti")),
	)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: ctrlcli.ObjectKey{Name: "shared"},
	})
	require.Error(t, err,
		"a refused document is not something to converge past: the ledger write below revalidates "+
			"nothing, so carrying on is what sends the refused tenant to the master")
	assert.Contains(t, err.Error(), "render the tenant quota policy")

	puts, deletes, _, _ := master.counts()
	assert.Zero(t, puts, "the master must not be written for a set the validator refused")
	assert.Zero(t, deletes, "and nothing may be removed from it on that pass either")
	assert.Empty(t, master.held(), "so its ledger is exactly as it was")
}
