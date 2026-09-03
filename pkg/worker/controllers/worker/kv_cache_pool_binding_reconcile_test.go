package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// readBinding fetches one Binding's stored state, failing the test rather than returning a zero
// object a later assertion would read as an empty status.
func readBinding(t *testing.T, cli ctrlcli.Client, namespace, name string) *workercore.KVCachePoolBinding {
	t.Helper()

	kvcpb := new(workercore.KVCachePoolBinding)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: namespace, Name: name}, kvcpb))
	return kvcpb
}

// TestKVCachePoolBindingReconcile_EachBindingReadsItsOwnSeries is the rule the whole pass is built
// on: the tenant IS the reuse domain, so a Binding's figures come from the one series bearing its
// own domain name and no path adds two together.
//
// The two Bindings deliberately ask for different amounts and are granted different amounts, so a
// pass that summed, averaged or crossed them would produce a number neither of these assertions
// accepts.
func TestKVCachePoolBindingReconcile_EachBindingReadsItsOwnSeries(t *testing.T) {
	master := newFakeMaster()
	// Oversubscribed on purpose: each domain is granted less than it asked for, and in proportion.
	master.effective["team-a-chat"] = quantityValue("8Ti")
	master.effective["team-b-batch"] = quantityValue("4Ti")
	master.occupancy["team-a-chat"] = quantityValue("2Ti")
	master.occupancy["team-b-batch"] = quantityValue("1Ti")
	master.overQuota["team-b-batch"] = true
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "team-b-batch", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	chat := readBinding(t, cli, "team-a", "chat").Status
	assert.Equal(t, quantityValue("20Ti"), chat.RequestedQuota.Value(),
		"requestedQuota is this tenant's requested_quota_bytes verbatim — no division anywhere")
	assert.Equal(t, quantityValue("8Ti"), chat.EffectiveQuota.Value())
	assert.Equal(t, quantityValue("2Ti"), chat.Usage.Value())
	require.NotNil(t, chat.OverQuota)
	assert.False(t, *chat.OverQuota)

	batch := readBinding(t, cli, "team-b", "batch").Status
	assert.Equal(t, quantityValue("10Ti"), batch.RequestedQuota.Value())
	assert.Equal(t, quantityValue("4Ti"), batch.EffectiveQuota.Value())
	assert.Equal(t, quantityValue("1Ti"), batch.Usage.Value())
	require.NotNil(t, batch.OverQuota)
	assert.True(t, *batch.OverQuota, "the master's own verdict, not one derived here")

	// The master's global sums are in the same document and are 30Ti and 12Ti. Neither Binding may
	// carry either, which is what "no summation on any path" means when written as an assertion.
	for _, status := range []workercore.KVCachePoolBindingStatus{chat, batch} {
		assert.NotEqual(t, quantityValue("30Ti"), status.RequestedQuota.Value())
		assert.NotEqual(t, quantityValue("12Ti"), status.EffectiveQuota.Value())
	}
}

// TestKVCachePoolBindingReconcile_OneScrapeFeedsEveryBinding is the cost assertion, and it is a
// count because nothing in the resulting status shows it.
//
// The exposition is ONE document holding every tenant, so a pass reads it once however many
// namespaces bound to the pool. Scraping per Binding would put the master's request load under the
// control of how many teams use it — invisible in every status, visible only here.
func TestKVCachePoolBindingReconcile_OneScrapeFeedsEveryBinding(t *testing.T) {
	scrapesFor := func(t *testing.T, bindings int) int {
		t.Helper()

		master := newFakeMaster()
		address := master.start(t)

		objs := make([]ctrlcli.Object, 0, 2+bindings)
		objs = append(objs,
			newReconcileBackend("mooncake-dram", address),
			newTestKVCachePool("shared", "mooncake-dram"))
		for i := range bindings {
			objs = append(objs, newBoundBinding(
				fmt.Sprintf("team-%d", i), "workload", "shared",
				fmt.Sprintf("domain-%d", i), resource.MustParse("1Ti")))
		}

		r, _ := newReconciler(objs...)
		reconcilePool(t, r, "shared")

		_, _, _, scrapes := master.counts()
		return scrapes
	}

	// Both the relation AND the absolute value. Comparing the two alone would pass just as happily
	// if every Binding cost a scrape and the two counts happened to be eight apiece.
	assert.Equal(t, 1, scrapesFor(t, 1))
	assert.Equal(t, 1, scrapesFor(t, 8),
		"eight Bindings cost one scrape, the same as one; the count must not grow with them")
}

// TestKVCachePoolBindingReconcile_AbsentIsNotZero covers the two states that would both serialize as
// zero if the figures were not pointers, and which mean opposite things to whoever reads them.
func TestKVCachePoolBindingReconcile_AbsentIsNotZero(t *testing.T) {
	t.Run("a tenant the master has never heard of reports nothing, and says so", func(t *testing.T) {
		master := newFakeMaster()
		master.omitTenants["team-a-chat"] = true
		address := master.start(t)

		r, cli := newReconciler(
			newReconcileBackend("mooncake-dram", address),
			newTestKVCachePool("shared", "mooncake-dram"),
			newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		)

		reconcilePool(t, r, "shared")

		kvcpb := readBinding(t, cli, "team-a", "chat")
		assert.Nil(t, kvcpb.Status.RequestedQuota, "absent, not a quota of zero")
		assert.Nil(t, kvcpb.Status.EffectiveQuota)
		assert.Nil(t, kvcpb.Status.Usage)
		assert.Nil(t, kvcpb.Status.OverQuota,
			"an unobserved verdict must not read as 'you are within quota'")

		assert.True(t, KVCachePoolBindingConditionQuotaObserved.IsTrue(kvcpb),
			"the master answered; it simply carries no entry — that is an observation, not a failure")
		assert.Contains(t, KVCachePoolBindingConditionQuotaObserved.GetMessage(kvcpb),
			"no quota has been observed at all")
	})

	t.Run("blocks and hitRate stay absent, having no source at all", func(t *testing.T) {
		master := newFakeMaster()
		master.occupancy["team-a-chat"] = quantityValue("2Ti")
		address := master.start(t)

		r, cli := newReconciler(
			newReconcileBackend("mooncake-dram", address),
			newTestKVCachePool("shared", "mooncake-dram"),
			newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		)

		reconcilePool(t, r, "shared")

		kvcpb := readBinding(t, cli, "team-a", "chat")
		require.NotNil(t, kvcpb.Status.Usage, "the figures that DO have a source arrived")
		assert.Nil(t, kvcpb.Status.Blocks,
			"0.3.13 exports no per-tenant object count at all, and publishing one only on the older "+
				"generation would make the field vanish on upgrade — worse than never having it")
		assert.Empty(t, kvcpb.Status.HitRate,
			"hit rate is per-master, never per-tenant; a fabricated 0 on a warm cache is worse than none")
	})
}

// TestKVCachePoolBindingReconcile_UsageFollowsTheMastersGeneration is the version split reaching
// status.
//
// 0.3.12.post1 reports committed bytes apart from reservations; 0.3.13 reports one figure charged
// from PutStart. Both land in usage — a Binding on the newer master must not report nothing — but
// only the second is allowed to say the number includes writes that have not committed.
func TestKVCachePoolBindingReconcile_UsageFollowsTheMastersGeneration(t *testing.T) {
	observe := func(t *testing.T, newGeneration bool) *workercore.KVCachePoolBinding {
		t.Helper()

		master := newFakeMaster()
		master.newGeneration = newGeneration
		master.occupancy["team-a-chat"] = quantityValue("2Ti")
		address := master.start(t)

		r, cli := newReconciler(
			newReconcileBackend("mooncake-dram", address),
			newTestKVCachePool("shared", "mooncake-dram"),
			newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
		)
		reconcilePool(t, r, "shared")
		return readBinding(t, cli, "team-a", "chat")
	}

	t.Run("0.3.12.post1 reports usage excluding reservations, and does not caveat it", func(t *testing.T) {
		kvcpb := observe(t, false)
		require.NotNil(t, kvcpb.Status.Usage)
		assert.Equal(t, quantityValue("2Ti"), kvcpb.Status.Usage.Value())
		assert.NotContains(t, KVCachePoolBindingConditionQuotaObserved.GetMessage(kvcpb),
			"have not committed")
	})

	t.Run("0.3.13 reports the one figure it has, and says what is inside it", func(t *testing.T) {
		kvcpb := observe(t, true)
		require.NotNil(t, kvcpb.Status.Usage,
			"the newer master must not silently report no usage at all")
		assert.Equal(t, quantityValue("2Ti"), kvcpb.Status.Usage.Value())
		assert.Contains(t, KVCachePoolBindingConditionQuotaObserved.GetMessage(kvcpb),
			"have not committed",
			"a figure that includes in-flight writes may not be published as if it did not")
	})
}

// There is no case here for a Binding that asked for nothing. quotaCeiling is REQUIRED, because the
// state it would describe does not work: the storage layer holds no default policy, and refuses a
// tenant it has no policy for with the same error a domain that was never declared gets. The two
// tests that used to pin that state were removed with it rather than rewritten — a test for a
// configuration admission no longer accepts asserts against a path nothing can reach.

// TestKVCachePoolBindingReconcile_AFailedScrapeKeepsThePreviousFigures is the difference between
// "we could not look" and "you are using nothing".
//
// The first pass observes real figures; the second finds the master refusing /metrics. Zeroing them
// there would send somebody hunting for a cache that had evaporated, when the cache is fine and the
// scrape is not.
func TestKVCachePoolBindingReconcile_AFailedScrapeKeepsThePreviousFigures(t *testing.T) {
	master := newFakeMaster()
	master.occupancy["team-a-chat"] = quantityValue("2Ti")
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	before := readBinding(t, cli, "team-a", "chat").Status
	require.NotNil(t, before.Usage, "the first pass has to have observed something to keep")

	master.refuseScrapeWith(503)

	reconcilePool(t, r, "shared")

	kvcpb := readBinding(t, cli, "team-a", "chat")
	assert.Equal(t, before.Usage.Value(), kvcpb.Status.Usage.Value(),
		"the previous figures stay; a failed scrape is not an observation of zero")
	assert.Equal(t, before.EffectiveQuota.Value(), kvcpb.Status.EffectiveQuota.Value())

	assert.False(t, KVCachePoolBindingConditionQuotaObserved.IsTrue(kvcpb),
		"what moves is the Condition, so the figures are readable as stale rather than as current")
	assert.Contains(t, KVCachePoolBindingConditionQuotaObserved.GetMessage(kvcpb),
		"from the last pass that could")
}

// TestKVCachePoolBindingReconcile_ATenantThatLeavesTheLedgerClearsItsFigures is the OPPOSITE rule to
// the one above, and the pair only works if both hold.
//
// A failed scrape keeps the previous figures, because nothing was learned. A successful scrape that
// no longer mentions the tenant CLEARS them, because something was: the entry is gone. Keeping them
// here would leave a quota on display that the master has stopped enforcing — the exact reading the
// "absent, never zero" rule exists to prevent, arrived at from the other direction.
func TestKVCachePoolBindingReconcile_ATenantThatLeavesTheLedgerClearsItsFigures(t *testing.T) {
	master := newFakeMaster()
	master.occupancy["team-a-chat"] = quantityValue("2Ti")
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	require.NotNil(t, readBinding(t, cli, "team-a", "chat").Status.EffectiveQuota,
		"the first pass has to have observed something for the second to clear")

	// The master answers, and its answer no longer carries this tenant.
	master.mu.Lock()
	master.omitTenants["team-a-chat"] = true
	master.mu.Unlock()

	reconcilePool(t, r, "shared")

	kvcpb := readBinding(t, cli, "team-a", "chat")
	assert.Nil(t, kvcpb.Status.EffectiveQuota,
		"a quota the master no longer holds may not stay on display")
	assert.Nil(t, kvcpb.Status.RequestedQuota)
	assert.Nil(t, kvcpb.Status.Usage)
	assert.Nil(t, kvcpb.Status.OverQuota)
	assert.True(t, KVCachePoolBindingConditionQuotaObserved.IsTrue(kvcpb),
		"and the axis stays True: the master was read, it simply carries no entry")
}

// TestKVCachePoolBindingReconcile_AContestedDomainNamesTheOtherClaimant is the F9 race backstop, on
// the Binding side.
//
// Admission refuses a second claim on a domain, so reaching this means two creates raced one cache.
// Neither Binding is managed — picking a winner would hand one namespace a ceiling the other set —
// and each names the other, because they are in different namespaces and neither can list the other.
func TestKVCachePoolBindingReconcile_AContestedDomainNamesTheOtherClaimant(t *testing.T) {
	master := newFakeMaster()
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "contested", resource.MustParse("20Ti")),
		newBoundBinding("team-b", "batch", "shared", "contested", resource.MustParse("10Ti")),
	)

	reconcilePool(t, r, "shared")

	chat := readBinding(t, cli, "team-a", "chat")
	batch := readBinding(t, cli, "team-b", "batch")

	for _, kvcpb := range []*workercore.KVCachePoolBinding{chat, batch} {
		assert.False(t, KVCachePoolBindingConditionDomainExclusive.IsTrue(kvcpb))
		assert.Equal(t, KVCachePoolPhaseError, kvcpb.Status.Phase,
			"a domain managed for nobody is not a Ready binding")
		assert.Nil(t, kvcpb.Status.EffectiveQuota,
			"and no figures are published for a domain this binding may not be the owner of")
	}

	assert.Contains(t, KVCachePoolBindingConditionDomainExclusive.GetMessage(chat), "team-b/batch",
		"each side names the other; without the name neither can find out who holds it")
	assert.Contains(t, KVCachePoolBindingConditionDomainExclusive.GetMessage(batch), "team-a/chat")
	assert.NotContains(t, KVCachePoolBindingConditionDomainExclusive.GetMessage(chat), "team-a/chat",
		"and never itself, which would read as a binding contending with its own claim")
}

// TestKVCachePoolBindingReconcile_ASettledBindingIsNotRewritten is the same guard the pool has, one
// level down: the reconciler wakes on its own status writes, so a pass that rewrote an unchanged
// Binding would feed itself forever.
func TestKVCachePoolBindingReconcile_ASettledBindingIsNotRewritten(t *testing.T) {
	master := newFakeMaster()
	master.occupancy["team-a-chat"] = quantityValue("2Ti")
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")
	first := readBinding(t, cli, "team-a", "chat").ResourceVersion

	reconcilePool(t, r, "shared")

	assert.Equal(t, first, readBinding(t, cli, "team-a", "chat").ResourceVersion,
		"a second pass over an unchanged master writes nothing at all")
}

// TestKVCachePoolBindingReconcile_AZeroGrantIsNotReady is the reading a Binding exists to prevent,
// asserted on the object a workload actually names.
//
// Measured on a real master: a leader that has restarted answers its admin API within a couple of
// seconds and passes its readiness probe there — the probe reads the segment list, not the ledger —
// and then reports effective_quota_bytes 0 until its segments have remounted. The tenant IS in the
// ledger throughout, with its requested figure intact, so every observation succeeded.
//
// Observation succeeding is exactly why Ready was wrong: the phase was summarized from
// DomainExclusive and QuotaObserved alone, and a grant of zero is a successful observation. A
// workload sent to that Binding would have every byte it wrote refused, with the status saying Ready.
func TestKVCachePoolBindingReconcile_AZeroGrantIsNotReady(t *testing.T) {
	master := newFakeMaster()
	master.effective["team-a-chat"] = 0
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")

	kvcpb := readBinding(t, cli, "team-a", "chat")
	require.NotNil(t, kvcpb.Status.EffectiveQuota,
		"the figure is still published: the master was asked and said zero, which is a measurement")
	assert.Equal(t, int64(0), kvcpb.Status.EffectiveQuota.Value())
	assert.Equal(t, quantityValue("20Ti"), kvcpb.Status.RequestedQuota.Value(),
		"and the request is intact, which is what makes the observation a success rather than a gap")

	assert.NotEqual(t, KVCachePoolPhaseReady, kvcpb.Status.Phase,
		"a Binding granted nothing cannot serve a write, whatever the observation succeeded at")
	assert.Contains(t, kvcpb.Status.PhaseMessage, "zero bytes",
		"and the phase message says what was read rather than naming a cause it did not observe")
}

// TestKVCachePoolBindingReconcile_ANoLedgerEntryIsNotReady is the same defect one branch over, and
// the branch is worse: the master carries no entry at all, so every quota figure is cleared, and the
// Binding still reported Ready on two axes that were both True.
//
// This is the state a Binding is in between being admitted and the pass that writes its ceiling
// reaching the master, and the state a Binding whose ceiling never arrived stays in.
func TestKVCachePoolBindingReconcile_ANoLedgerEntryIsNotReady(t *testing.T) {
	master := newFakeMaster()
	master.omitTenants["team-a-chat"] = true
	address := master.start(t)

	r, cli := newReconciler(
		newReconcileBackend("mooncake-dram", address),
		newTestKVCachePool("shared", "mooncake-dram"),
		newBoundBinding("team-a", "chat", "shared", "team-a-chat", resource.MustParse("20Ti")),
	)

	reconcilePool(t, r, "shared")

	kvcpb := readBinding(t, cli, "team-a", "chat")
	assert.Nil(t, kvcpb.Status.EffectiveQuota,
		"cleared rather than zeroed: no quota was observed at all, which is not a quota of zero")
	assert.NotEqual(t, KVCachePoolPhaseReady, kvcpb.Status.Phase,
		"and a Binding with no ledger entry has nothing to write into")
}
