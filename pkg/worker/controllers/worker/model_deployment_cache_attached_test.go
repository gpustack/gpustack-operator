package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// fakeModelDeploymentCacheScraper answers per replica name.
//
// It can express exactly the three readings the shipped engines afford and NOT ONE MORE. In
// particular it cannot report "the connector initialized" without traffic, because none of the three
// engines publishes that: a stand-in more capable than the thing it stands in for would make the
// Unknown path unreachable in tests while it is the common case in production.
type fakeModelDeploymentCacheScraper struct {
	readings map[string]ModelDeploymentCacheReading
	// counts gives a replica's attempted transfer operations, the keys they touched and the keys
	// that failed, for the cases about what a message says rather than about which verdict it carries.
	counts map[string][3]float64
	// errs marks replicas whose endpoint did not answer at all, which is the OTHER way to have no
	// account — distinct from answering without the metric family.
	errs  map[string]bool
	calls []string
}

func (f *fakeModelDeploymentCacheScraper) ScrapeCache(
	_ context.Context, pod *core.Pod,
) (ModelDeploymentCacheAccount, error) {
	f.calls = append(f.calls, pod.Name)
	if f.errs[pod.Name] {
		return ModelDeploymentCacheAccount{}, errors.New("dial tcp: connection refused")
	}

	account := ModelDeploymentCacheAccount{Reading: f.readings[pod.Name]}
	counts := f.counts[pod.Name]
	account.Attempted, account.Keys, account.Failed = counts[0], counts[1], counts[2]

	return account, nil
}

// cacheAttachedOf drives one reading through the whole status compute, so the assertions are over
// what an operator would read off the object rather than over an internal return value.
//
// extra objects join the deployment and its AcceleratorType in the client, for the cases about what
// a message can say about the pool.
func cacheAttachedOf(
	t *testing.T, md *workercore.ModelDeployment, pods []core.Pod,
	domain *modelDeploymentDomain, scraper ModelDeploymentCacheScraper, extra ...ctrlcli.Object,
) *workercore.ModelDeployment {
	t.Helper()

	cli := newModelDeploymentClient(append([]ctrlcli.Object{md.DeepCopy(), newRenderAcceleratorType()}, extra...)...)
	r := &ModelDeploymentReconciler{Client: cli, APIReader: cli, CacheScraper: scraper}

	status, err := r.computeModelDeploymentStatusWithQualifications(context.Background(), md, pods, domain, nil, nil, nil)
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

func TestModelDeploymentCacheAttached_WithoutSharedCacheIsNotApplicable(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.KVCache = nil })
	scraper := &fakeModelDeploymentCacheScraper{}

	got := cacheAttachedOf(t, md, readyPods(md, 1), nil, scraper)

	assert.True(t, ModelDeploymentConditionCacheAttached.IsTrue(got))
	assert.Equal(t, modelDeploymentReasonCacheNotApplicable,
		ModelDeploymentConditionCacheAttached.GetReason(got))
	assert.Empty(t, scraper.calls, "there is no shared-store metric to scrape")
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
			//
			// THE REPLICAS CARRY NO DRAIN HOOK, which is what makes them takeover replicas: a role
			// supplying its own command line gets no hook, no synthesized arguments and no client
			// environment. Declaring Command on the role is no longer what opens this gate -- the
			// running Pods are what do -- so a fixture that only edited the spec would now describe
			// replicas the operator did build and would be asking the wrong question.
			name: "a replica running a command line the operator contributed nothing to",
			mutateMD: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"/bin/my-server"}
			},
			pods:       func(md *workercore.ModelDeployment) []core.Pod { return takeoverPods(md, 2) },
			domain:     readyDomain(func(d *modelDeploymentDomain) { d.Blocks = ptr.To[int64](512) }),
			wantStatus: "Unknown",
			wantReason: modelDeploymentReasonUnmanaged,
		},
		{
			// The reverse transition is the case that made this observable: the role has gone back
			// to a managed command line, so the DESIRED spec claims the operator built these
			// arguments -- while the Pods still standing are running a command line it never wrote.
			// Reading the spec here would report a cache for an engine configured with no client.
			name: "a role declared managed while its running replicas were built as a takeover",
			mutateMD: func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"/bin/my-server"}
			},
			pods:       func(md *workercore.ModelDeployment) []core.Pod { return takeoverPods(md, 2) },
			domain:     readyDomain(),
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
			// The one state with no nearer observer: the engine is Ready and serving, and every
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
			// An OBSERVED zero is not evidence of detachment: an attached idle domain holds zero
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
			// A reconciler built without a scraper reads through the production one. It cannot reach a
			// replica that carries no address, so it must read as Unknown rather than as a detachment.
			name:       "no scraper assigned",
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

// takeoverPods are replicas whose command line the operator did not write: the same Pods
// readyPods builds with the serving-port annotation removed, which is exactly what the render emits
// for a role supplying its own command line -- it stamps nothing, because it cannot know what the
// replacement serves.
func takeoverPods(md *workercore.ModelDeployment, n int32) []core.Pod {
	pods := readyPods(md, n)
	for i := range pods {
		delete(pods[i].Annotations, "prometheus.io/port")
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

// TestModelDeploymentCacheAttached_AnInstanceShortAMemberIsNotAsked holds this condition's idea of a
// ready replica equal to the one the role statuses publish.
//
// A REPLICA IS READY WHEN EVERY MEMBER OF IT IS, and asking its leader alone is not the same
// question. The leader of an instance whose sibling is absent or still starting can be Ready on its
// own — nothing makes an engine's readiness probe wait for the collective to form, since the probe
// is the deployment author's — so a leader-only filter puts this condition's account of the cache
// next to a ready count of zero, with nothing to tell a reader which of the two to believe.
//
// THE WHOLE INSTANCE IS THE BASELINE. Without the first subtest the second passes against a filter
// that asks nobody at all, which is the implementation a reader would least expect to be wrong.
func TestModelDeploymentCacheAttached_AnInstanceShortAMemberIsNotAsked(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 2
		md.Spec.Roles[0].ReplicaSize = 2
	})

	member := func(ordinal, index int, ready bool) core.Pod {
		pod := readyReplica(md, int32(ordinal), ready)
		pod.Name = fmt.Sprintf("%s-%s-r%d-m%d", md.Name, md.Spec.Roles[0].Name, ordinal, index)
		pod.UID = types.UID(pod.Name + "-uid")
		pod.Labels[modelDeploymentReplicaOrdinalLabel] = fmt.Sprint(ordinal)
		pod.Labels[modelDeploymentMemberIndexLabel] = fmt.Sprint(index)

		return *pod
	}

	readings := map[string]ModelDeploymentCacheReading{
		"qwen-server-r0-m0": ModelDeploymentCacheActive,
		"qwen-server-r1-m0": ModelDeploymentCacheActive,
	}

	t.Run("both_instances_whole", func(t *testing.T) {
		pods := []core.Pod{
			member(0, 0, true), member(0, 1, true),
			member(1, 0, true), member(1, 1, true),
		}
		scraper := &fakeModelDeploymentCacheScraper{readings: readings}

		got := cacheAttachedOf(t, md, pods, readyDomain(), scraper)

		assert.Equal(t, []string{"qwen-server-r0-m0", "qwen-server-r1-m0"}, scraper.calls,
			"one leader per whole instance, and only the leader: the ranks serve no API to ask")
		assert.True(t, ModelDeploymentConditionCacheAttached.IsTrue(got))
	})

	t.Run("one_instance_short_a_member", func(t *testing.T) {
		// Instance 1 holds a Ready leader and nothing else. The role statuses count it as zero ready
		// replicas, and this condition has to agree.
		pods := []core.Pod{
			member(0, 0, true), member(0, 1, true),
			member(1, 0, true),
		}
		scraper := &fakeModelDeploymentCacheScraper{readings: readings}

		cacheAttachedOf(t, md, pods, readyDomain(), scraper)

		assert.Equal(t, []string{"qwen-server-r0-m0"}, scraper.calls,
			"the leader of an instance short a member is Ready on its own and is still not asked")
	})

	t.Run("one_member_of_an_instance_is_not_ready", func(t *testing.T) {
		pods := []core.Pod{
			member(0, 0, true), member(0, 1, true),
			member(1, 0, true), member(1, 1, false),
		}
		scraper := &fakeModelDeploymentCacheScraper{readings: readings}

		cacheAttachedOf(t, md, pods, readyDomain(), scraper)

		assert.Equal(t, []string{"qwen-server-r0-m0"}, scraper.calls,
			"a rank that is not Ready withholds its whole instance, leader included")
	})
}

// TestModelDeploymentCacheAttached_UnreadableExecutionIsNeverTrue is the control the review named.
//
// THE MEMBERS OF ONE GROUP DISAGREE on whether the operator wrote their command line, so this
// operator cannot say what either container was configured with. The shared domain deliberately
// HOLDS data here, which is the signal that would otherwise corroborate a True: it is a domain-wide
// fact, and letting it stand in for a replica whose client was never shown would attribute a
// domain observation to containers that may carry no client at all.
func TestModelDeploymentCacheAttached_UnreadableExecutionIsNeverTrue(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Replicas = 1
	})
	domain := readyDomain(func(d *modelDeploymentDomain) { d.Blocks = ptr.To[int64](512) })

	// A MANAGED REPLICA DOES report here, which is what shows the case below is refused for the
	// disagreement rather than because the domain is unusable: same domain, same replicas.
	assert.Equal(t, "True",
		ModelDeploymentConditionCacheAttached.GetStatus(
			cacheAttachedOf(t, md, readyPods(md, 1), domain, nil),
		),
		"the corroborating signal is usable, so a refusal below is about the replicas")

	// ONE GROUP OF TWO, hand-built because readyReplica names each Pod for itself and two members
	// of one replica have to name the SAME group. The disagreement is the defect under test, so the
	// fixture states it outright rather than arriving at it by accident.
	group := modelDeploymentReplicaGroupName(md, "server", 0)
	member := func(name, seat string, managed bool) core.Pod {
		pod := *readyReplica(md, 0, true)
		pod.Name, pod.UID = name, types.UID("uid-"+name)
		pod.Annotations = map[string]string{
			kueuepodconst.GroupTotalCountAnnotation: "2",
		}
		pod.Labels = map[string]string{
			modelDeploymentLabelKeyComponent:   "server",
			modelDeploymentReplicaOrdinalLabel: "0",
			modelDeploymentMemberIndexLabel:    seat,
			kueuepodconst.GroupNameLabel:       group,
		}
		if managed {
			pod.Annotations["prometheus.io/port"] = "8000"
		}

		return pod
	}
	half := []core.Pod{member("s-half-0", "0", true), member("s-half-1", "1", false)}

	got := cacheAttachedOf(t, md, half, domain, nil)
	assert.NotEqual(t, "True", ModelDeploymentConditionCacheAttached.GetStatus(got),
		"an unattributable command line may never be corroborated by a shared domain")
	assert.Equal(t, "Unknown", ModelDeploymentConditionCacheAttached.GetStatus(got))
	assert.Equal(t, modelDeploymentReasonCacheNotAttributable,
		ModelDeploymentConditionCacheAttached.GetReason(got),
		"the reason names the refusal rather than the missing observation it would otherwise report")
}

// readyRoleReplica builds a Ready replica of one declared role, on the shape readyReplica builds for
// the first: the role's own labels, name and declared size.
func readyRoleReplica(md *workercore.ModelDeployment, role int, ordinal int32, ready bool) core.Pod {
	pod := readyReplica(md, ordinal, ready)
	declared := &md.Spec.Roles[role]
	pod.Name = fmt.Sprintf("%s-%s-%d", md.Name, declared.Name, ordinal)
	pod.UID = types.UID(pod.Name + "-uid")
	pod.Labels = modelDeploymentPodLabels(md, declared, "fixture-entrance")
	pod.Annotations[kueuepodconst.GroupTotalCountAnnotation] = strconv.Itoa(modelDeploymentRoleSize(declared))

	return *pod
}

// routedPairDeployment is a prefill/decode pair behind the engine's own router, bound to a pool,
// with an encoder stage in front when withEncoder is set: the 1P1D and 1E1P1D shapes.
func routedPairDeployment(engine string, withEncoder bool) *workercore.ModelDeployment {
	router := workercore.ModelDeploymentRouterLLMD
	if engine == workercore.ModelDeploymentEngineSGLang {
		router = workercore.ModelDeploymentRouterSGL
	}

	return newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Engine.Name = engine
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: router}
		md.Spec.Roles = []workercore.ModelDeploymentRole{
			{Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill, Replicas: 1, AcceleratorType: "h20-8x"},
			{Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode, Replicas: 1, AcceleratorType: "h20-8x"},
		}
		if withEncoder {
			md.Spec.Roles = append([]workercore.ModelDeploymentRole{{
				Name: "encode", Kind: workercore.ModelDeploymentRoleKindEncode, Replicas: 1, AcceleratorType: "h20-8x",
			}}, md.Spec.Roles...)
		}
	})
}

func TestModelDeploymentCacheAttached_OnlyStoreClientRolesAreAsked(t *testing.T) {
	t.Run("vllm_1e1p1d_asks_the_prefiller_alone", func(t *testing.T) {
		md := routedPairDeployment(workercore.ModelDeploymentEngineVLLM, true)
		pods := []core.Pod{
			readyRoleReplica(md, 0, 0, true), readyRoleReplica(md, 1, 0, true), readyRoleReplica(md, 2, 0, true),
		}
		scraper := &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
			"qwen-prefill-0": ModelDeploymentCacheActive,
		}}

		got := cacheAttachedOf(t, md, pods, readyDomain(), scraper)

		assert.Equal(t, []string{"qwen-prefill-0"}, scraper.calls,
			"the encoder holds no KV cache and the vLLM decode half receives its blocks from the prefiller")
		assert.True(t, ModelDeploymentConditionCacheAttached.IsTrue(got))
		assert.Contains(t, ModelDeploymentConditionCacheAttached.GetMessage(got), "1 of 1 ready replicas")
	})

	t.Run("sglang_1p1d_asks_both_halves", func(t *testing.T) {
		md := routedPairDeployment(workercore.ModelDeploymentEngineSGLang, false)
		pods := []core.Pod{readyRoleReplica(md, 0, 0, true), readyRoleReplica(md, 1, 0, true)}
		scraper := &fakeModelDeploymentCacheScraper{}

		cacheAttachedOf(t, md, pods, readyDomain(), scraper)

		assert.Equal(t, []string{"qwen-prefill-0", "qwen-decode-0"}, scraper.calls,
			"SGLang's store client is role-blind, so its decode half carries one")
	})
}

// TestModelDeploymentRoleRendersStore asks the renderer's predicate through the deployment, for
// every role shape that could be mistaken for a store client.
func TestModelDeploymentRoleRendersStore(t *testing.T) {
	vllm := workercore.ModelDeploymentEngineVLLM
	sglang := workercore.ModelDeploymentEngineSGLang
	role := func(name string, kind workercore.ModelDeploymentRoleKind) workercore.ModelDeploymentRole {
		return workercore.ModelDeploymentRole{Name: name, Kind: kind, Replicas: 1, AcceleratorType: "h20-8x"}
	}
	withRole := func(name string, kind workercore.ModelDeploymentRoleKind, mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
		md := newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Roles = []workercore.ModelDeploymentRole{role(name, kind)}
		})
		for _, m := range mutate {
			m(md)
		}

		return md
	}
	// paired puts the role under test behind the engine's own router with the other half declared,
	// which is what makes a prefill or decode role a disaggregated one.
	paired := func(engine string) func(*workercore.ModelDeployment) {
		router := workercore.ModelDeploymentRouterLLMD
		if engine == sglang {
			router = workercore.ModelDeploymentRouterSGL
		}

		return func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Name = engine
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: router}
			other := workercore.ModelDeploymentRoleKindDecode
			if md.Spec.Roles[0].Kind == workercore.ModelDeploymentRoleKindDecode {
				other = workercore.ModelDeploymentRoleKindPrefill
			}
			md.Spec.Roles = append(md.Spec.Roles, role("other", other))
		}
	}

	testCases := []struct {
		name string
		md   *workercore.ModelDeployment
		want bool
	}{
		{name: "a vLLM server", md: withRole("server", workercore.ModelDeploymentRoleKindServer), want: true},
		{name: "a vLLM prefiller", md: withRole("prefill", workercore.ModelDeploymentRoleKindPrefill), want: true},
		{
			name: "a vLLM prefiller behind a router",
			md:   withRole("prefill", workercore.ModelDeploymentRoleKindPrefill, paired(vllm)), want: true,
		},
		{
			// The decode half receives its blocks from its prefiller, which reads and writes the store.
			name: "a disaggregated vLLM decoder",
			md:   withRole("decode", workercore.ModelDeploymentRoleKindDecode, paired(vllm)), want: false,
		},
		{
			// A lone decode half has no pair to receive from and runs undivided, on the store.
			name: "a lone vLLM decoder",
			md:   withRole("decode", workercore.ModelDeploymentRoleKindDecode), want: true,
		},
		{name: "a vLLM encoder", md: withRole("encode", workercore.ModelDeploymentRoleKindEncode), want: false},
		{
			name: "a take-over role",
			md: withRole("server", workercore.ModelDeploymentRoleKindServer, func(md *workercore.ModelDeployment) {
				md.Spec.Roles[0].Command = []string{"/bin/my-server"}
			}),
			want: false,
		},
		{
			name: "a disaggregated SGLang decoder",
			md:   withRole("decode", workercore.ModelDeploymentRoleKindDecode, paired(sglang)), want: true,
		},
		{
			name: "a role of a deployment with no pool",
			md: withRole("server", workercore.ModelDeploymentRoleKindServer, func(md *workercore.ModelDeployment) {
				md.Spec.KVCache = nil
			}),
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ModelDeploymentRoleRendersStore(tc.md, &tc.md.Spec.Roles[0]))
		})
	}
}

// TestModelDeploymentCacheAttached_Messages reads what an operator would read off the object. A
// condition an operator acts on has to name the pool, what the pool's transport asks of the engine,
// what the role asked for, and what each replica reported.
func TestModelDeploymentCacheAttached_Messages(t *testing.T) {
	rdmaBackend := newRenderBackend()
	rdmaBackend.Spec.Transport.Protocol = "RDMA"
	failing := func() *fakeModelDeploymentCacheScraper {
		return &fakeModelDeploymentCacheScraper{
			readings: map[string]ModelDeploymentCacheReading{
				"qwen-server-0": ModelDeploymentCacheFailing,
				"qwen-server-1": ModelDeploymentCacheFailing,
			},
			counts: map[string][3]float64{"qwen-server-0": {3, 3, 3}, "qwen-server-1": {2, 2, 2}},
		}
	}
	withInterface := func(value string) func(*workercore.ModelDeployment) {
		return func(md *workercore.ModelDeployment) {
			md.Spec.Roles[0].Replicas = 2
			if value != "" {
				md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
					Interface: ptr.To(resource.MustParse(value)),
				}
			}
		}
	}
	message := func(t *testing.T, md *workercore.ModelDeployment, scraper *fakeModelDeploymentCacheScraper,
		domain *modelDeploymentDomain, extra ...ctrlcli.Object,
	) string {
		t.Helper()

		return ModelDeploymentConditionCacheAttached.GetMessage(
			cacheAttachedOf(t, md, readyPods(md, 2), domain, scraper, extra...),
		)
	}

	t.Run("failing_without_an_interface_on_a_fabric_pool", func(t *testing.T) {
		got := message(t, newRenderDeployment(withInterface("0")), failing(), readyDomain(),
			newRenderPool(), rdmaBackend)

		assert.Contains(t, got, `pool "shared" (transports: [rdma])`)
		assert.Contains(t, got, "2 of 2 ready replicas report store transfers of which every key failed")
		assert.Contains(t, got, `qwen-server-0 (role "server", resources.interface: 0): 3 transfer operations attempted, 3 of 3 keys failed`)
		assert.Contains(t, got, `qwen-server-1 (role "server", resources.interface: 0): 2 transfer operations attempted, 2 of 2 keys failed`)
		assert.Contains(t, got, "this engine has no RDMA interface while every transport of the pool requires one")
	})

	t.Run("failing_with_the_interface_unset_on_a_fabric_pool", func(t *testing.T) {
		got := message(t, newRenderDeployment(withInterface("")), failing(), readyDomain(),
			newRenderPool(), rdmaBackend)

		assert.Contains(t, got, `resources.interface: unset`)
		assert.Contains(t, got, "this engine has no RDMA interface while every transport of the pool requires one")
	})

	t.Run("failing_with_an_interface_on_a_fabric_pool", func(t *testing.T) {
		got := message(t, newRenderDeployment(withInterface("1")), failing(), readyDomain(),
			newRenderPool(), rdmaBackend)

		assert.Contains(t, got, `resources.interface: 1`)
		assert.NotContains(t, got, "no RDMA interface", "the role asked for an interface, so its absence is not the cause to name")
	})

	t.Run("failing_without_an_interface_on_a_tcp_pool", func(t *testing.T) {
		got := message(t, newRenderDeployment(withInterface("0")), failing(), readyDomain(),
			newRenderPool(), newRenderBackend())

		assert.Contains(t, got, `pool "shared" (transports: [tcp])`)
		assert.NotContains(t, got, "no RDMA interface", "a tcp pool asks no fabric interface of the engine")
	})

	t.Run("active_names_the_failing_sibling", func(t *testing.T) {
		scraper := &fakeModelDeploymentCacheScraper{
			readings: map[string]ModelDeploymentCacheReading{
				"qwen-server-0": ModelDeploymentCacheActive,
				"qwen-server-1": ModelDeploymentCacheFailing,
			},
			counts: map[string][3]float64{"qwen-server-0": {5, 9, 1}, "qwen-server-1": {2, 2, 2}},
		}

		got := message(t, newRenderDeployment(withInterface("1")), scraper, readyDomain(),
			newRenderPool(), rdmaBackend)

		assert.Contains(t, got, "1 of 2 ready replicas report succeeding store transfers")
		assert.Contains(t, got, `qwen-server-0 (role "server", resources.interface: 1): 5 transfer operations attempted, 1 of 9 keys failed`)
		assert.Contains(t, got, "1 report transfers of which every key failed")
		assert.Contains(t, got, `qwen-server-1 (role "server", resources.interface: 1): 2 transfer operations attempted, 2 of 2 keys failed`)
	})

	t.Run("unreadable_says_no_transfer_was_observed", func(t *testing.T) {
		scraper := &fakeModelDeploymentCacheScraper{errs: map[string]bool{"qwen-server-0": true, "qwen-server-1": true}}

		got := message(t, newRenderDeployment(withInterface("1")), scraper, readyDomain(),
			newRenderPool(), rdmaBackend)

		assert.Contains(t, got, `pool "shared" (transports: [rdma]); no store transfer was observed: none of the 2 ready replicas`)
		assert.NotContains(t, got, ". ", "the Unknown says so in one sentence")
	})

	t.Run("an_unresolved_binding_names_the_binding", func(t *testing.T) {
		scraper := &fakeModelDeploymentCacheScraper{readings: map[string]ModelDeploymentCacheReading{
			"qwen-server-0": ModelDeploymentCacheFailing,
			"qwen-server-1": ModelDeploymentCacheFailing,
		}}

		got := message(t, newRenderDeployment(withInterface("0")), scraper, nil, newRenderPool(), rdmaBackend)

		assert.Contains(t, got, `binding "shared-kv" (transports: unresolved)`)
	})
}

// TestModelDeploymentReconciler_APoolBoundDeploymentIsLookedAtAgain is what keeps the cache
// condition honest once a deployment has converged.
//
// A converged pass returns an empty result, and nothing that moves the condition is an event: the
// transfers it reports are traffic between an engine and a store, on objects this controller is not
// watching for it. Without a pass of its own the condition a reader sees is the one the first
// traffic-free pass wrote. A deployment with no pool has no such condition and keeps its result.
func TestModelDeploymentReconciler_APoolBoundDeploymentIsLookedAtAgain(t *testing.T) {
	convergedResult := func(t *testing.T, md *workercore.ModelDeployment) ctrl.Result {
		t.Helper()

		cli := newModelDeploymentClient(md, newRenderAcceleratorType())
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		res, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		return res
	}

	t.Run("bound_to_a_pool", func(t *testing.T) {
		res := convergedResult(t, newRenderDeployment())

		assert.Positive(t, res.RequeueAfter)
		assert.LessOrEqual(t, res.RequeueAfter, modelDeploymentCacheRequeue)
		assert.Equal(t, modelDeploymentCacheRequeue, res.RequeueAfter)
	})

	t.Run("no_pool", func(t *testing.T) {
		res := convergedResult(t, newRenderDeployment(func(md *workercore.ModelDeployment) { md.Spec.KVCache = nil }))

		assert.Equal(t, ctrl.Result{}, res)
	})

	t.Run("a_rollout_waiting_for_admission", func(t *testing.T) {
		// The same hold TestModelDeploymentWorkloadWatch_ReleasesAHeldRollout walks: a pass that left
		// replicas outdated is not converged, and the admission event is what brings it back.
		cli := newModelDeploymentClient(newRenderDeployment(), newRenderAcceleratorType())
		_, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)
		standInForKueue(t, cli, false)
		changed := getModelDeployment(t, cli)
		changed.Spec.Roles[0].Image = "vllm/vllm-openai:v0.26.0"
		require.NoError(t, cli.Update(context.Background(), changed))

		res, err := reconcileModelDeployment(t, cli)
		require.NoError(t, err)

		assert.Equal(t, ctrl.Result{}, res)
	})
}

// TestModelDeploymentPoolTransportsNeedInterface reads the rule off the backend shapes admission and
// the cache condition both judge: every member group has to reach the host's fabric for an engine
// with no fabric interface to be unable to transfer at all.
func TestModelDeploymentPoolTransportsNeedInterface(t *testing.T) {
	one := int32(1)
	// group is one member group: its own transport when it declares one, and its fabric interface
	// count.
	type group struct {
		protocol string
		count    *int32
	}
	backend := func(protocol string, groups ...group) *workercore.KVCacheBackend {
		kvcb := newRenderBackend()
		kvcb.Spec.Transport.Protocol = protocol
		if len(groups) == 0 {
			return kvcb
		}
		managed := &workercore.KVCacheBackendManaged{}
		for _, group := range groups {
			member := workercore.KVCacheBackendMember{FabricInterfaceCount: group.count}
			if group.protocol != "" {
				member.Transport = &workercore.KVCacheBackendMemberTransport{Protocol: group.protocol}
			}
			managed.Members = append(managed.Members, member)
		}
		kvcb.Spec.Connection.Managed = managed

		return kvcb
	}

	testCases := []struct {
		name string
		kvcb *workercore.KVCacheBackend
		want bool
	}{
		{name: "an external rdma backend", kvcb: backend("RDMA"), want: true},
		{name: "an external efa backend", kvcb: backend("EFA"), want: true},
		{name: "an external tcp backend", kvcb: backend("TCP"), want: false},
		{
			name: "an external ascend backend, which has no group to opt into host roce",
			kvcb: backend("CANN"), want: false,
		},
		{name: "one rdma group", kvcb: backend("RDMA", group{count: &one}), want: true},
		{name: "rdma groups with and without a count", kvcb: backend("RDMA", group{count: &one}, group{}), want: true},
		{name: "an ascend group on host roce", kvcb: backend("CANN", group{count: &one}), want: true},
		{name: "an ascend group with no count", kvcb: backend("CANN", group{}), want: false},
		{
			name: "ascend groups with and without host roce",
			kvcb: backend("CANN", group{count: &one}, group{}), want: false,
		},
		{name: "rdma beside tcp", kvcb: backend("RDMA", group{protocol: "TCP"}, group{}), want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ModelDeploymentPoolTransportsNeedInterface(tc.kvcb))
		})
	}
}
