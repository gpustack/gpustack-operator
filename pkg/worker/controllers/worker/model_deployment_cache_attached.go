package worker

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// ModelDeploymentConditionCacheAttached reports whether the cache is observed to be in effect,
// which is a different question from whether it was configured.
//
// A flag being accepted proves nothing: measured on the shipped store, `--enable_kv_events=true` is
// accepted, the startup log echoes `enable_kv_events=1`, and `GET /kv_events/status` still answers
// `{"enabled":false}` with the socket never bound. In the same project another undeclared switch
// fails loudly instead. One switch's failure mode cannot be inferred from another's, so this
// condition is judged downstream of the engine and never on a render or a log line.
const ModelDeploymentConditionCacheAttached kubeapistatus.ConditionType = "CacheAttached"

// modelDeploymentCacheRequeue is how often a deployment bound to a pool is looked at again once it
// has converged, so that CacheAttached follows the traffic it reports. Bounded and named: a scrape
// of every ready store client runs on each of these passes, and an unbounded interval would be the
// multi-hour resync this exists to replace.
const modelDeploymentCacheRequeue = 30 * time.Second

const (
	modelDeploymentReasonCacheActive            = "CacheActive"
	modelDeploymentReasonCacheOperationsFailing = "CacheOperationsFailing"
	modelDeploymentReasonCacheNotApplicable     = "NotApplicable"
	modelDeploymentReasonUnmanaged              = "Unmanaged"
	modelDeploymentReasonCacheNotAttributable   = "CacheNotAttributable"
	modelDeploymentReasonNoReplicaReady         = "NoReplicaReady"
	modelDeploymentReasonNoObservationAvailable = "NoObservationAvailable"
)

// ModelDeploymentCacheReading is one replica's own account of its KV cache client.
//
// THREE VALUES, AND THE THIRD IS NOT A NEGATIVE. What the three supported engines actually expose
// was measured, and none of them publishes anything that says "the connector initialized" before any
// traffic: vLLM's five `vllm:mooncake_store_operation_*` families are all labeled and only reach
// `.labels()` from an observation path that returns early on empty data; vllm-ascend declares no
// Prometheus metric of its own at all; SGLang's storage counters are labeled the same way, and its
// `hicache_host_*` gauges appear at startup but only prove the host tier is on. So "no account" and
// "attached and idle" are indistinguishable, and reading the first as a detachment would be a false
// alarm on the most common steady state there is.
//
// This type therefore CANNOT express a traffic-free "initialized" reading, and that is deliberate: a
// stand-in able to report a signal no engine provides would make the Unknown path untestable, and
// every test over it would be a test of a signal that does not exist.
type ModelDeploymentCacheReading int

const (
	// ModelDeploymentCacheUnreadable is no account at all: the endpoint did not answer, or it
	// answered and carried no store metric family. It is Unknown, never False.
	ModelDeploymentCacheUnreadable ModelDeploymentCacheReading = iota
	// ModelDeploymentCacheFailing is an account of store transfers of which NO key ever succeeded.
	// It is positive evidence rather than an absence: a replica that tried and failed publishes key
	// counters nothing else publishes. It is read off the keys and not off the status label, because
	// the pinned vLLM images record an all-refused transfer as partial_failure.
	ModelDeploymentCacheFailing
	// ModelDeploymentCacheActive is an account of store transfers of which at least one key
	// succeeded, or that completed with no failed key.
	ModelDeploymentCacheActive
)

// ModelDeploymentCacheAccount is one replica's own account of its KV cache client, as one scrape read
// it: the verdict the account supports and the transfer counts it rests on.
type ModelDeploymentCacheAccount struct {
	// Reading is the verdict. Unreadable is the zero value, which is also what a scrape that carried
	// no store family returns.
	Reading ModelDeploymentCacheReading
	// Attempted is how many transfer operations (save_put and load_get) the replica's process has
	// attempted, over every status, since it started.
	Attempted float64
	// Keys is how many keys those operations touched.
	Keys float64
	// Failed is how many of those keys were refused, whatever status the operation recorded.
	Failed float64
}

// ModelDeploymentCacheScraper reads one replica's own account of its KV cache client.
//
// It is an INTERFACE THE RECONCILER TAKES rather than a dial it makes, because every case that
// matters here is a failure and a real dial cannot be made to fail on demand.
//
// The account is per replica at the Pod's own address, and that is the property the whole condition
// rests on: the reuse domain is shared by every deployment on one Binding by design, so a
// domain-level figure cannot attribute — with a healthy deployment and a broken one on one Binding,
// the healthy one's writes would report the broken one as attached.
//
// A NIL SCRAPER IS THE PRODUCTION ONE, which reads the engine's own Prometheus exposition, the way
// a nil groupForwardFetch is the production transport. Wired where it is read rather than in
// SetupController, so a pass never depends on a seam somebody remembered to assign.
type ModelDeploymentCacheScraper interface {
	// ScrapeCache reads one replica's account.
	//
	// A non-nil error means the account could not be read at all, and the reading is then
	// Unreadable. Unreadable with a nil error is the other way to not have an account: the endpoint
	// answered, and carried no store transfer.
	ScrapeCache(ctx context.Context, pod *core.Pod) (ModelDeploymentCacheAccount, error)
}

// observeModelDeploymentCache folds the cache-attachment reading into the status.
//
// Level-based, evaluated every pass, and never True by assumption.
func (r *ModelDeploymentReconciler) observeModelDeploymentCache(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
	domain *modelDeploymentDomain, holder *workercore.ModelDeployment,
) {
	if md.Spec.KVCache == nil {
		ModelDeploymentConditionCacheAttached.True(holder, modelDeploymentReasonCacheNotApplicable,
			"no shared KV cache was requested; direct prefill/decode transfer is reported by the router contract")

		return
	}

	if replica, reason, why := modelDeploymentUnclaimableReplica(md, pods); replica != "" {
		// THE OPERATOR CONTRIBUTED NOTHING THIS REPLICA COULD ANSWER FOR, so it does not claim the
		// observation is about its own doing. Whatever the pool reports, and whatever the shared
		// domain corroborates: a domain-wide fact cannot stand in for a replica whose command line
		// could not be attributed.
		ModelDeploymentConditionCacheAttached.Unknown(holder, reason,
			fmt.Sprintf("%s does not report a cache client it was not shown to have: %s", replica, why))

		return
	}

	ready := modelDeploymentReadyReplicas(md, pods)
	if len(ready) == 0 {
		ModelDeploymentConditionCacheAttached.Unknown(holder, modelDeploymentReasonNoReplicaReady,
			"no replica has become ready yet, so no engine has an account to give")

		return
	}

	// ONLY THE ROLES THAT RENDER A STORE CLIENT ARE ASKED. An encoder stage holds no KV cache, and
	// a disaggregated vLLM decode half receives its blocks from its prefiller; neither carries a
	// client, so asking either would return no account and count it as a replica that stayed
	// silent. Which roles carry one is the renderer's own answer, asked through
	// ModelDeploymentRoleRendersStore rather than restated here per engine.
	ready = modelDeploymentStoreClientReplicas(md, ready)
	if len(ready) == 0 {
		ModelDeploymentConditionCacheAttached.Unknown(holder, modelDeploymentReasonNoObservationAvailable,
			"no ready replica belongs to a role that renders a store client, so no engine has an account to give")

		return
	}

	accounts := r.readModelDeploymentCache(ctx, ready)
	pool := r.resolveModelDeploymentCachePool(ctx, md, domain)
	var active, failing, unreadable int
	for _, account := range accounts {
		switch account.account.Reading {
		case ModelDeploymentCacheActive:
			active++
		case ModelDeploymentCacheFailing:
			failing++
		default:
			unreadable++
		}
	}

	switch {
	case active > 0:
		message := fmt.Sprintf("%s; %d of %d ready replicas report succeeding store transfers (%s)",
			pool.describe(), active, len(ready),
			modelDeploymentCacheAccountsLine(md, accounts, ModelDeploymentCacheActive))
		// A SIBLING'S FAILURE IS NAMED RATHER THAN OUTVOTED. One replica succeeding says the cache is
		// in effect for the deployment, and the replica that fails says something about its own node
		// that the count alone would hide.
		if failing > 0 {
			message += fmt.Sprintf("; %d report transfers of which every key failed (%s)", failing,
				modelDeploymentCacheAccountsLine(md, accounts, ModelDeploymentCacheFailing))
		}
		if unreadable > 0 {
			message += fmt.Sprintf("; %d gave no account", unreadable)
		}
		ModelDeploymentConditionCacheAttached.True(holder, modelDeploymentReasonCacheActive, message)

	case failing > 0:
		// NOT an absence, and this is the one state with no nearer observer. A connector that
		// cannot come up takes its replica with it — vLLM raises without its config path — so that
		// failure is already reported as a replica that never becomes Ready. This one is different:
		// the engine is Ready and serving, and every key of every store transfer it attempts is
		// refused. Nothing else in the status says so, which is why it is False here rather than
		// Unknown.
		message := fmt.Sprintf("%s; %d of %d ready replicas report store transfers of which every key "+
			"failed, so the engine is serving without the cache (%s)",
			pool.describe(), failing, len(ready),
			modelDeploymentCacheAccountsLine(md, accounts, ModelDeploymentCacheFailing))
		if unreadable > 0 {
			message += fmt.Sprintf("; %d gave no account", unreadable)
		}
		// THE ONE CAUSE THE OBJECT ITSELF CAN NAME. A pool whose every transport reaches the host
		// fabric refuses a client that holds no fabric interface, and a role that requests none says
		// so in its own spec. Named here because it is the difference between a pool to fix and a
		// field to set.
		for _, role := range modelDeploymentCacheInterfacelessRoles(md, accounts, pool) {
			message += fmt.Sprintf("; role %q requests resources.interface: %s, so this engine has no RDMA "+
				"interface while every transport of the pool requires one", role.Name, modelDeploymentRoleInterface(role))
		}
		ModelDeploymentConditionCacheAttached.False(holder, modelDeploymentReasonCacheOperationsFailing, message)

	case domain != nil && modelDeploymentDomainHoldsData(domain):
		// The corroborating signal, used only where no replica gave an account. It is the master's
		// own account of bytes that landed, which is stronger evidence of a working cache and weaker
		// attribution — it is per reuse domain, and one domain is shared by design.
		ModelDeploymentConditionCacheAttached.True(holder, modelDeploymentReasonCacheActive,
			fmt.Sprintf("no replica gave an account, and reuse domain %q holds data; this attributes "+
				"to the domain rather than to this deployment, because one domain is shared by "+
				"every deployment on its binding", domain.KVCache.Domain.Name))

	default:
		// Unknown because THE STATE IT WOULD REPORT HAS A NEARER OBSERVER, not merely because the
		// signal is missing. A connector that failed to come up killed its replica, and a replica
		// that is not Ready is already reported by status.roles[].ready. What is left here is a
		// deployment that is attached and simply idle, which is indistinguishable from an unread
		// one — and the two must not be collapsed into a detachment report.
		//
		// Absent is not zero: an absent blocks/usage on the Binding means the scrape did not carry
		// this domain, while an observed zero is a domain holding nothing, and an idle attached
		// domain holds nothing either.
		ModelDeploymentConditionCacheAttached.Unknown(holder,
			modelDeploymentReasonNoObservationAvailable,
			fmt.Sprintf("%s; no store transfer was observed: none of the %d ready replicas gave an account "+
				"of its cache client and the reuse domain reports nothing held; an attached deployment "+
				"that is idle looks exactly like this, so it is not reported as detached",
				pool.describe(), len(ready)))
	}
}

// modelDeploymentCacheReplicaAccount is one ready replica's account, with the replica that gave it.
type modelDeploymentCacheReplicaAccount struct {
	pod     *core.Pod
	account ModelDeploymentCacheAccount
}

// readModelDeploymentCache scrapes every ready replica it is given and returns what came back.
//
// THE REPLICAS ARE SCRAPED TOGETHER, under the reconcile's own context and with the per-scrape
// timeout the scraper keeps. One at a time, a pass over N replicas costs up to N timeouts whenever
// endpoints hang, and that pass repeats on every modelDeploymentCacheRequeue while the controller's
// other reconciles queue behind it. The bound on scrapes in flight is the scaler's, for the same act.
//
// A scrape that failed or timed out is an Unreadable account for THAT replica alone, never an error
// for the pass: the condition's whole contract is that an unread signal is Unknown. Nothing cancels
// the other scrapes, so a hung endpoint costs one timeout and no replica more.
//
// EACH GOROUTINE WRITES ITS OWN SLOT, which is what keeps the accounts in replica order however the
// scrapes complete. The message is rendered from that order, so it reads the same from pass to pass
// and a diff of two messages is a diff of the accounts and not of the network.
func (r *ModelDeploymentReconciler) readModelDeploymentCache(
	ctx context.Context, ready []*core.Pod,
) []modelDeploymentCacheReplicaAccount {
	scraper := r.CacheScraper
	if scraper == nil {
		scraper = defaultModelDeploymentCacheScraper
	}

	accounts := make([]modelDeploymentCacheReplicaAccount, len(ready))
	var group errgroup.Group
	group.SetLimit(maxSamplerScrapeConcurrency)
	for i, pod := range ready {
		group.Go(func() error {
			account, err := scraper.ScrapeCache(ctx, pod)
			if err != nil {
				account = ModelDeploymentCacheAccount{}
			}
			accounts[i] = modelDeploymentCacheReplicaAccount{pod: pod, account: account}

			return nil
		})
	}
	// No scrape returns an error, so Wait only waits.
	_ = group.Wait()

	return accounts
}

// modelDeploymentStoreClientReplicas keeps the ready replicas of the roles that render a store client.
//
// A replica of a role the spec no longer declares was already dropped by the caller, so a nil role
// here is not a shape this filter has to decide on.
func modelDeploymentStoreClientReplicas(md *workercore.ModelDeployment, ready []*core.Pod) []*core.Pod {
	declared := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		declared[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	kept := make([]*core.Pod, 0, len(ready))
	for _, pod := range ready {
		role := declared[modelDeploymentPodRole(pod)]
		if role != nil && ModelDeploymentRoleRendersStore(md, role) {
			kept = append(kept, pod)
		}
	}

	return kept
}

// modelDeploymentBoundRoleAddress stands in for the pool's address where the question is only WHICH
// roles render a store client. The renderer's predicate reads the address for presence alone, and
// presence is not in question for a deployment that declares a pool: whether the pool currently
// publishes its endpoint decides what a replica was rendered with, which the scrape itself reports.
const modelDeploymentBoundRoleAddress = "bound"

// ModelDeploymentRoleRendersStore reports whether this operator renders the shared store client onto
// one role of a pool-bound deployment.
//
// IT IS THE RENDERER'S OWN ANSWER, asked through inject.RendersStore with the role's kind, the
// engine family and the point-to-point leg, and never a second table: a decoder keeps off the store
// under vLLM and keeps it under SGLang, and which is which is that package's to know.
//
// THE SCRAPE SET RELIES ON inject.RendersStore ANSWERING THE SAME FOR BOTH vLLM ENGINES: the pool's
// manufacturer is not resolved here, so the plain engine is asked on behalf of vLLM-Ascend too, and
// TestModelDeploymentCacheScraper_RendersStoreIsInvariantAcrossTheVLLMEngines holds that equality so
// a divergence fails a test instead of skewing the set.
//
// THREE ROLES NEVER CARRY A CLIENT AND SO NEVER ANSWER. A take-over role runs a command line this
// operator adds nothing to. An encoder stage holds no KV cache. A disaggregated vLLM decode half
// receives its blocks from its prefiller. Admission asks this before warning about a missing fabric
// interface, and adding one to a decode role would move the direct transfer onto the fabric.
//
// A deployment that declares no pool renders no client on any role.
func ModelDeploymentRoleRendersStore(md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole) bool {
	if md.Spec.KVCache == nil || len(role.Command) > 0 {
		return false
	}
	injectRole, ok := modelDeploymentInjectRole(ModelDeploymentEffectiveRoleKind(role))
	if !ok {
		return false
	}
	engine, err := ModelDeploymentInjectEngine(md.Spec.Engine.Name, "")
	if err != nil {
		return false
	}

	return inject.RendersStore(inject.Input{
		Engine:     engine,
		Role:       injectRole,
		Connection: inject.Connection{MasterAddress: modelDeploymentBoundRoleAddress},
		KVTransfer: modelDeploymentUsesKVTransfer(md, role),
	})
}

// modelDeploymentCachePoolFact is what the cache condition can say about the pool a deployment
// writes to. Every field is for the message; nothing in the verdict reads it.
type modelDeploymentCachePoolFact struct {
	// kind and name name the object the fact was read down to: "pool" once the pool resolved, and
	// "binding" while only the deployment's own reference is known.
	kind string
	name string
	// transports is what the pool's backend offers an engine, in group order. nil when the backend
	// could not be read.
	transports []string
	// needsInterface says whether every transport reaches the host's fabric.
	needsInterface bool
}

// describe names the pool and its transports for a condition message.
func (f modelDeploymentCachePoolFact) describe() string {
	transports := "unresolved"
	if len(f.transports) > 0 {
		transports = fmt.Sprintf("%v", f.transports)
	}

	return fmt.Sprintf("%s %q (transports: %s)", f.kind, f.name, transports)
}

// resolveModelDeploymentCachePool reads the pool and the backend the deployment's Binding points at,
// through the same chain the connection resolves through.
//
// IT IS LENIENT AT EVERY STEP, because it feeds a message and not a verdict: an unreadable object
// leaves the fact at whatever it resolved down to, and the condition's Unknown is the verdict's own
// answer to a Binding that does not resolve.
func (r *ModelDeploymentReconciler) resolveModelDeploymentCachePool(
	ctx context.Context, md *workercore.ModelDeployment, domain *modelDeploymentDomain,
) modelDeploymentCachePoolFact {
	fact := modelDeploymentCachePoolFact{kind: "binding", name: md.Spec.KVCache.PoolRef.Name}
	if domain == nil || domain.KVCache == nil {
		return fact
	}
	fact.kind, fact.name = "pool", domain.KVCache.Pool

	pool := new(workercore.KVCachePool)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: fact.name}, pool); err != nil || len(pool.Spec.Backends) == 0 {
		return fact
	}
	backend := new(workercore.KVCacheBackend)
	if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: pool.Spec.Backends[0]}, backend); err != nil {
		return fact
	}
	fact.transports = mooncake.MemberProtocols(backend)
	fact.needsInterface = ModelDeploymentPoolTransportsNeedInterface(backend)

	return fact
}

// ModelDeploymentPoolTransportsNeedInterface reports whether every member group of a backend reaches
// the host's fabric, which is what makes an engine with no fabric interface of its own unable to
// transfer to the pool at all.
//
// rdma and efa reach it unconditionally. ascend reaches it only when the group opts into Ascend A5
// Host RoCE with a positive fabricInterfaceCount; the same protocol with the count left unset is
// the shape that stays off the host fabric, and never needs the interface. tcp never does. An
// external backend that declares no member group is judged on its one backend-wide transport.
//
// It is EXPORTED because admission asks the same question before warning about a role that writes
// to such a pool without requesting an interface, and a second copy of the rule would disagree with
// this one on the first protocol added.
func ModelDeploymentPoolTransportsNeedInterface(kvcb *workercore.KVCacheBackend) bool {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil || len(managed.Members) == 0 {
		return mooncake.MemberGroupIsHostFabric(kvcb, workercore.KVCacheBackendMember{})
	}
	for i := range managed.Members {
		if !mooncake.MemberGroupIsHostFabric(kvcb, managed.Members[i]) &&
			!mooncake.MemberGroupUsesCANNHostRoCE(kvcb, managed.Members[i]) {
			return false
		}
	}

	return true
}

// modelDeploymentRoleInterface spells a role's resources.interface for a message: the value it
// requests, or that it requests none.
func modelDeploymentRoleInterface(role *workercore.ModelDeploymentRole) string {
	if role == nil || role.Resources == nil || role.Resources.Interface == nil {
		return "unset"
	}

	return role.Resources.Interface.String()
}

// modelDeploymentCacheMessageReplicas is how many replicas of each role one line of the CacheAttached
// message renders.
//
// IT IS A BOUND ON THE MESSAGE, not on the observation. The schema caps a condition message at 32768
// characters and a role's replicas at 1024, and one segment per replica runs past the first limit
// long before the second. Every replica is still scraped and still counted in the head; the ones
// beyond the cap are counted again in one tail that names their verdict, so a reader is told what
// they are not being shown rather than left to read a short list as the whole of it.
const modelDeploymentCacheMessageReplicas = 3

// modelDeploymentCacheAccountsLine renders the replicas whose account reads the given reading, each
// with its role, the interface that role requests and its transfer counts, at most
// modelDeploymentCacheMessageReplicas of each role in replica order. The replicas left out follow in
// one segment with their count and verdict. A replica that gave no account has no counts to print, so
// it is not rendered here at all; the message counts it instead.
func modelDeploymentCacheAccountsLine(
	md *workercore.ModelDeployment, accounts []modelDeploymentCacheReplicaAccount,
	reading ModelDeploymentCacheReading,
) string {
	roles := modelDeploymentRolesByName(md)
	rendered := make(map[string]int, len(roles))
	parts := make([]string, 0, len(accounts))
	var omitted int
	for _, entry := range accounts {
		if entry.account.Reading != reading {
			continue
		}
		role := modelDeploymentPodRole(entry.pod)
		if rendered[role] >= modelDeploymentCacheMessageReplicas {
			omitted++

			continue
		}
		rendered[role]++
		parts = append(parts, fmt.Sprintf("%s (role %q, resources.interface: %s): %s transfer operations "+
			"attempted, %s of %s keys failed", entry.pod.Name, role,
			modelDeploymentRoleInterface(roles[role]),
			modelDeploymentCacheCount(entry.account.Attempted), modelDeploymentCacheCount(entry.account.Failed),
			modelDeploymentCacheCount(entry.account.Keys)))
	}
	if omitted > 0 {
		parts = append(parts, fmt.Sprintf("and %d more %s", omitted, modelDeploymentCacheReadingPhrase(reading)))
	}

	return strings.Join(parts, "; ")
}

// modelDeploymentCacheReadingPhrase is what a replica's account says it reports, in the words the
// message's head already uses for the same reading.
func modelDeploymentCacheReadingPhrase(reading ModelDeploymentCacheReading) string {
	if reading == ModelDeploymentCacheActive {
		return "report succeeding store transfers"
	}

	return "report transfers of which every key failed"
}

// modelDeploymentRolesByName indexes the declared roles by name.
func modelDeploymentRolesByName(md *workercore.ModelDeployment) map[string]*workercore.ModelDeploymentRole {
	roles := make(map[string]*workercore.ModelDeploymentRole, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		roles[md.Spec.Roles[i].Name] = &md.Spec.Roles[i]
	}

	return roles
}

// modelDeploymentCacheInterfacelessRoles names the roles whose failing replicas request no fabric
// interface while the pool needs one. A role is named once however many of its replicas fail.
func modelDeploymentCacheInterfacelessRoles(
	md *workercore.ModelDeployment, accounts []modelDeploymentCacheReplicaAccount, pool modelDeploymentCachePoolFact,
) []*workercore.ModelDeploymentRole {
	if !pool.needsInterface {
		return nil
	}

	roles := modelDeploymentRolesByName(md)
	var named []*workercore.ModelDeploymentRole
	for _, entry := range accounts {
		if entry.account.Reading != ModelDeploymentCacheFailing {
			continue
		}
		role := roles[modelDeploymentPodRole(entry.pod)]
		if role == nil || modelDeploymentRoleRequestsInterfaces(role) || slices.Contains(named, role) {
			continue
		}
		named = append(named, role)
	}

	return named
}

// modelDeploymentCacheCount prints a counter the way an operator reads a count.
func modelDeploymentCacheCount(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// modelDeploymentUnclaimableReplica names a replica of a declared role whose running command line
// this operator cannot speak for, with the machine-readable reason and a message. It refuses both a
// replica running a command line the operator did not write and one whose command line cannot be
// attributed, because a shared domain may not corroborate either.
func modelDeploymentUnclaimableReplica(
	md *workercore.ModelDeployment, pods []core.Pod,
) (string, string, string) {
	declared := make(map[string]bool, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		declared[md.Spec.Roles[i].Name] = true
	}

	for _, replica := range modelDeploymentGroupPodsByReplica(pods) {
		// Auxiliary heads some engines render are built by this operator and carry none of its
		// annotations, so only a role the spec declares is asked.
		if !declared[replica.Role] {
			continue
		}

		execution, why := modelDeploymentDeployedReplicaExecution(replica.Members)
		switch execution {
		case modelDeploymentExecutionTakeover:
			return modelDeploymentPodDescription(replica.Members[0]),
				modelDeploymentReasonUnmanaged,
				"it runs a command line this operator contributed no arguments to, so it was " +
					"configured with no cache client and is not reported on one"
		case modelDeploymentExecutionUnreadable:
			return modelDeploymentPodDescription(replica.Members[0]),
				modelDeploymentReasonCacheNotAttributable, why
		}
	}

	return "", "", ""
}

// modelDeploymentReadyReplicas selects the replicas an engine can be asked about: whole, ready, not
// on their way out, and represented by the one member of each that answers. A terminating replica
// may still answer, and what it says is about a process that is leaving.
//
// ONLY THE LEADER IS ASKED, because only the leader serves the API this question is put through. A
// replica of several members runs one engine spread across them and exposes it at member zero; the
// others hold ranks and answer nothing. Asking them would not return a different account of the
// cache -- it would return no account at all, and those non-answers land in the unreadable or
// failing tallies, which turns a healthy multi-member deployment's cache condition False for a
// reason that has nothing to do with its cache.
//
// READY MEANS WHAT THE PUBLISHED REPLICA COUNTS MEAN, which is why the whole replica is weighed
// rather than its leader alone. A replica is ready when every member of it is, and the role statuses
// beside this condition count them that way; a leader that is Ready while a sibling is absent or
// still starting would otherwise put this condition's account of the cache next to a ready count of
// zero, with nothing to tell a reader which of the two to believe. Nothing enforces that an engine's
// leader stays unready until its collective forms -- the readiness probe is the deployment author's
// -- so the agreement has to be made here rather than assumed.
//
// A REPLICA OF A ROLE THE SPEC NO LONGER DECLARES IS NOT ASKED. Its size is unknown, so there is no
// count to hold its members against, and a Pod the deployment has stopped declaring is one on its
// way out rather than one whose cache the deployment is reporting on.
//
// A POD CARRYING NO MEMBER LABEL IS THE LEADER, which is what makes this filter free for the shape
// that came before: a single-member replica's Pod has no such label and reads as member zero, and a
// replica of one member is whole as soon as that Pod is there, so exactly the same Pods are asked as
// were asked before.
func modelDeploymentReadyReplicas(md *workercore.ModelDeployment, pods []core.Pod) []*core.Pod {
	declared := make(map[string]bool, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		declared[md.Spec.Roles[i].Name] = true
	}

	// The grouping drops the members already on their way out, so a replica losing one is short of
	// its declared total here and falls out with the incomplete ones.
	//
	// COMPLETENESS IS READ FROM THE REPLICA'S OWN GROUP TOTAL. The role's declared size describes
	// the replica about to be built, and a role whose size has been edited would otherwise drop
	// every healthy old replica of it out of the set of engines this operator is willing to ask
	// -- turning a configuration change into a silent cache observation gap.
	ready := make([]*core.Pod, 0, len(pods))
	for _, replica := range modelDeploymentGroupPodsByReplica(pods) {
		if !declared[replica.Role] {
			continue
		}
		if !modelDeploymentDeployedReplicaShape(replica).modelDeploymentReplicaIsWhole() {
			continue
		}
		// Only a replica proven managed is scraped: the gate above already refused the rest.
		if execution, _ := modelDeploymentDeployedReplicaExecution(replica.Members); execution !=
			modelDeploymentExecutionManaged {
			continue
		}

		var leader *core.Pod
		whole := true
		for _, member := range replica.Members {
			if !podIsReady(member) {
				whole = false

				break
			}
			if modelDeploymentPodMemberIndex(member) == modelDeploymentLeaderMemberIndex {
				leader = member
			}
		}
		if whole && leader != nil {
			ready = append(ready, leader)
		}
	}

	return ready
}

// modelDeploymentDomainHoldsData reports whether the master's account of the reuse domain shows
// anything held.
//
// nil is not zero on either figure. Both are pointers with omitempty on the Binding, so absent
// means the scrape did not carry this domain; treating that as zero would turn every unscraped pool
// into a positive detachment report.
func modelDeploymentDomainHoldsData(domain *modelDeploymentDomain) bool {
	if domain.KVCache == nil {
		return false
	}
	if domain.Blocks != nil && *domain.Blocks > 0 {
		return true
	}

	return domain.Usage != nil && !domain.Usage.IsZero()
}
