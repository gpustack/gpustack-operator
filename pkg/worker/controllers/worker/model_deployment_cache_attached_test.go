package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// fakeModelDeploymentCacheScraper answers per replica name.
//
// ⛔ It can express exactly the three readings the shipped engines afford and NOT ONE MORE. In
// particular it cannot report "the connector initialized" without traffic, because none of the three
// engines publishes that: a stand-in more capable than the thing it stands in for would make the
// Unknown path unreachable in tests while it is the common case in production.
type fakeModelDeploymentCacheScraper struct {
	readings map[string]ModelDeploymentCacheReading
	// errs marks replicas whose endpoint did not answer at all, which is the OTHER way to have no
	// account — distinct from answering without the metric family.
	errs  map[string]bool
	calls []string
}

func (f *fakeModelDeploymentCacheScraper) ScrapeCache(
	_ context.Context, pod *core.Pod,
) (ModelDeploymentCacheReading, error) {
	f.calls = append(f.calls, pod.Name)
	if f.errs[pod.Name] {
		return ModelDeploymentCacheUnreadable, errors.New("dial tcp: connection refused")
	}

	return f.readings[pod.Name], nil
}

// cacheAttachedOf drives one reading through the whole status compute, so the assertions are over
// what an operator would read off the object rather than over an internal return value.
func cacheAttachedOf(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod,
	domain *modelDeploymentDomain, scraper ModelDeploymentCacheScraper,
) *workercore.ModelDeployment {
	t.Helper()

	cli := newModelDeploymentClient(md.DeepCopy(), newRenderInstanceType())
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, CacheScraper: scraper}

	status, err := r.computeModelDeploymentStatus(context.Background(), md, pods, domain)
	require.NoError(t, err)

	return &workercore.ModelDeployment{Status: *status}
}

// readyDomain is a resolved, usable Binding whose master reports nothing held — the shape every
// primary-signal case wants, because it leaves the corroborating signal silent.
func readyDomain(mutate ...func(*modelDeploymentDomain)) *modelDeploymentDomain {
	d := &modelDeploymentDomain{
		KVCache: &workercore.ModelDeploymentKVCacheStatus{
			Binding: "shared-kv",
			Pool:    "shared",
			Domain: workercore.ModelDeploymentKVCacheDomain{
				Name: "chat", BlockSize: 256, Dtype: "bfloat16",
			},
		},
		Ready:  true,
		Reason: modelDeploymentReasonRegistered,
	}
	for _, m := range mutate {
		m(d)
	}

	return d
}

// TestModelDeploymentCacheAttached_Table walks every row of F8's table.
func TestModelDeploymentCacheAttached_Table(t *testing.T) {
	testCases := []struct {
		name       string
		mutateMD   func(*workercore.ModelDeployment)
		pods       func(*workercore.ModelDeployment) []core.Pod
		domain     *modelDeploymentDomain
		scraper    *fakeModelDeploymentCacheScraper
		wantStatus string
		wantReason string
	}{
		{
			// The operator rendered no cache client for it, so it does not report on one it did not
			// render — whatever the pool says. The domain here deliberately HOLDS data, which would
			// otherwise corroborate a True.
			name: "a role that took over the command line",
			mutateMD: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Template.Command = []string{"/bin/my-server"}
			},
			pods:       func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain:     readyDomain(func(d *modelDeploymentDomain) { d.Blocks = ptr.To[int64](512) }),
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonUnmanaged,
		},
		{
			name:       "no replica is ready yet",
			pods:       func(md *workercore.ModelDeployment) []core.Pod { return nil },
			domain:     readyDomain(),
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonNoReplicaReady,
		},
		{
			// The happy path, with NO traffic having been sent to the deployment: what is observed
			// is the engine's own account of succeeding store operations.
			name:   "a replica reports succeeding store operations",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(),
			scraper: &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
				"qwen-server-0": ModelDeploymentCacheActive,
				"qwen-server-1": ModelDeploymentCacheUnreadable,
			}},
			wantStatus: "True",
			wantReason: modelDeploymentReasonCacheActive,
		},
		{
			// ⛔ The one state with no nearer observer: the engine is Ready and serving, and every
			// store operation it attempts fails. Nothing else in the status says so.
			name:   "every store operation on every replica failed",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(),
			scraper: &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
				"qwen-server-0": ModelDeploymentCacheFailing,
				"qwen-server-1": ModelDeploymentCacheFailing,
			}},
			wantStatus: "False",
			wantReason: modelDeploymentReasonCacheOperationsFailing,
		},
		{
			// One replica succeeding outranks another failing: the cache IS in effect for this
			// deployment, and the failing replica is a per-replica fault the events report.
			name:   "one replica succeeds and another fails",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(),
			scraper: &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
				"qwen-server-0": ModelDeploymentCacheActive,
				"qwen-server-1": ModelDeploymentCacheFailing,
			}},
			wantStatus: "True",
			wantReason: modelDeploymentReasonCacheActive,
		},
		{
			// No account anywhere, and the domain holds nothing. An attached deployment that is idle
			// looks exactly like this, so it must not be reported as detached.
			name:   "nothing gave an account and the domain holds nothing",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(),
			scraper: &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
				"qwen-server-0": ModelDeploymentCacheUnreadable,
				"qwen-server-1": ModelDeploymentCacheUnreadable,
			}},
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonNoObservationAvailable,
		},
		{
			name:   "the endpoints did not answer at all",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(),
			scraper: &fakeModelDeploymentCacheScraper{
				errs: map[string]bool{"qwen-server-0": true, "qwen-server-1": true},
			},
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonNoObservationAvailable,
		},
		{
			// The corroborating signal, used only where the primary could not be read. It is the
			// master's own account of bytes that landed.
			name:   "no account, but the reuse domain holds blocks",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(func(d *modelDeploymentDomain) { d.Blocks = ptr.To[int64](512) }),
			scraper: &fakeModelDeploymentCacheScraper{
				errs: map[string]bool{"qwen-server-0": true, "qwen-server-1": true},
			},
			wantStatus: "True",
			wantReason: modelDeploymentReasonCacheActive,
		},
		{
			// ⛔ An OBSERVED zero is not evidence of detachment: an attached idle domain holds zero
			// too. This is the case F8 rejected `blocks: 0` for, asserted rather than described.
			name:   "no account, and the domain reports an observed zero",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(func(d *modelDeploymentDomain) { d.Blocks = ptr.To[int64](0) }),
			scraper: &fakeModelDeploymentCacheScraper{
				errs: map[string]bool{"qwen-server-0": true, "qwen-server-1": true},
			},
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonNoObservationAvailable,
		},
		{
			// And an ABSENT figure is a third thing again: the scrape did not carry this domain.
			// Treating absent as zero would be the same false report, reached differently.
			name:   "no account, and the domain figure is absent",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(),
			scraper: &fakeModelDeploymentCacheScraper{
				errs: map[string]bool{"qwen-server-0": true, "qwen-server-1": true},
			},
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonNoObservationAvailable,
		},
		{
			name:   "no account, but the domain reports bytes held",
			pods:   func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain: readyDomain(func(d *modelDeploymentDomain) { d.Usage = ptr.To(resource.MustParse("4Gi")) }),
			scraper: &fakeModelDeploymentCacheScraper{
				errs: map[string]bool{"qwen-server-0": true, "qwen-server-1": true},
			},
			wantStatus: "True",
			wantReason: modelDeploymentReasonCacheActive,
		},
		{
			// A reconciler built without a scraper is every replica unreadable. It is the shipped
			// state today, and it must read as Unknown rather than as a detachment.
			name:       "no scraper at all",
			pods:       func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain:     readyDomain(),
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonNoObservationAvailable,
		},
		{
			// The Binding could not be resolved either. Still Unknown, never False: nothing was
			// observed about the cache, and the domain's own condition is what reports the Binding.
			name:       "no reading of the binding at all",
			pods:       func(md *workercore.ModelDeployment) []core.Pod { return readyPods(md, 2) },
			domain:     nil,
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonNoObservationAvailable,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := newRenderDeployment(func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Replicas = 2
				if tc.mutateMD != nil {
					tc.mutateMD(md)
				}
			})

			got := cacheAttachedOf(t, md, tc.pods(md), tc.domain, scraperOrNil(tc.scraper))

			assert.Equal(t, tc.wantStatus,
				ModelDeploymentConditionCacheAttached.GetStatus(got))
			assert.Equal(t, tc.wantReason,
				ModelDeploymentConditionCacheAttached.GetReason(got))
			assert.NotEmpty(t, ModelDeploymentConditionCacheAttached.GetMessage(got),
				"a condition an operator acts on must carry a message")
		})
	}
}

// scraperOrNil keeps a nil typed pointer from becoming a non-nil interface, which would make the
// "no scraper at all" case silently scrape a zero-valued fake instead of taking the nil path.
func scraperOrNil(f *fakeModelDeploymentCacheScraper) ModelDeploymentCacheScraper {
	if f == nil {
		return nil
	}

	return f
}

// readyPods builds n Ready replicas of the deployment's first role.
func readyPods(md *workercore.ModelDeployment, n int32) []core.Pod {
	pods := make([]core.Pod, 0, n)
	for ordinal := range n {
		pods = append(pods, *readyReplica(md, ordinal, true))
	}

	return pods
}

// TestModelDeploymentCacheAttached_IsNeverTrueFromARender is G5 and F8's whole premise, asserted
// where it can actually be caught.
//
// A flag being accepted proves nothing: measured on the shipped store, `--enable_kv_events=true` is
// accepted, the startup log echoes it back, and the feature is off with the socket never bound. So a
// deployment whose connector was rendered perfectly, whose Binding is registered, and whose replicas
// are all Ready must NOT report the cache attached until something downstream says so.
func TestModelDeploymentCacheAttached_IsNeverTrueFromARender(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })

	got := cacheAttachedOf(t, md, readyPods(md, 2), readyDomain(), nil)

	assert.False(t, ModelDeploymentConditionCacheAttached.IsTrue(got),
		"everything was configured and nothing was observed, so nothing is claimed")
	assert.True(t, ModelDeploymentConditionCacheAttached.IsUnknown(got))
}

// TestModelDeploymentCacheAttached_ASiblingCannotAnswerForABrokenDeployment is the case the
// per-replica predicate exists for, and the regression it would catch.
//
// Two deployments on the same Binding share one reuse domain by design, so the domain's own
// blocks/usage cannot attribute: a healthy sibling filling the domain would report the broken
// deployment as attached. The primary signal is per replica, so the broken one answers for itself.
func TestModelDeploymentCacheAttached_ASiblingCannotAnswerForABrokenDeployment(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 2 })

	// The sibling is busy filling the shared domain, which is what the master's figures show.
	shared := readyDomain(func(d *modelDeploymentDomain) {
		d.Blocks = ptr.To[int64](8192)
		d.Usage = ptr.To(resource.MustParse("64Gi"))
	})

	broken := &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
		"qwen-server-0": ModelDeploymentCacheFailing,
		"qwen-server-1": ModelDeploymentCacheFailing,
	}}

	got := cacheAttachedOf(t, md, readyPods(md, 2), shared, broken)

	assert.True(t, ModelDeploymentConditionCacheAttached.IsFalse(got),
		"the shared domain is full because of the SIBLING; this deployment's own replicas all fail")
	assert.Equal(t, modelDeploymentReasonCacheOperationsFailing,
		ModelDeploymentConditionCacheAttached.GetReason(got))
}

// TestModelDeploymentCacheAttached_OnlyReadyReplicasAreAsked keeps the question answerable. An
// engine that has not finished coming up has no account to give, and one on its way out would
// answer about a process that is leaving.
func TestModelDeploymentCacheAttached_OnlyReadyReplicasAreAsked(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.Roles[0].Replicas = 3 })

	pods := []core.Pod{
		*readyReplica(md, 0, true),
		*readyReplica(md, 1, false),
		*readyReplica(md, 2, true),
	}
	// The third is Ready and terminating.
	now := pods[2].CreationTimestamp
	pods[2].DeletionTimestamp = &now

	scraper := &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
		"qwen-server-0": ModelDeploymentCacheActive,
	}}

	got := cacheAttachedOf(t, md, pods, readyDomain(), scraper)

	assert.Equal(t, []string{"qwen-server-0"}, scraper.calls,
		"a replica that is not Ready, and one that is leaving, are not asked")
	assert.True(t, ModelDeploymentConditionCacheAttached.IsTrue(got))
}
