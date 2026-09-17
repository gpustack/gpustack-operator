// This file holds the measured transport constraints that must be checked before rendering. An
// engine handed a transport its backend does not accept raises before it serves a request, so
// admitting the pair buys nothing and costs a container that cannot start.
package inject

// transportFacts is what has been measured about one engine version's transport requirement.
//
// The version and source line sit beside the answer so the discriminating check below can be
// repeated without first finding the upstream file again.
type transportFacts struct {
	// Version is the engine release the answer was read from.
	Version string

	// Source is the file and line the answer was read at, in that release.
	//
	Source string

	// Required is the ONE transport this engine's store backend accepts, in the artifact's own
	// spelling -- what mooncake.MemberProtocol renders, not what the API enum publishes.
	//
	// Empty means the engine refuses no transport, and that is a MEASURED answer rather than a
	// missing one: every engine has an entry and its Source names where the answer was read. The
	// distinction decides opposite outcomes -- an unmeasured engine has to be let through, an engine
	// measured as requiring "ascend" must not be.
	Required string

	// RequiredAPIValue is that same transport in the API's spelling: the value an operator writes
	// into KVCacheBackend.spec.transport.protocol.
	//
	// BOTH SPELLINGS ARE HERE because a refusal needs both, in different sentences. The pair it
	// reports has to be in the artifact's spelling, since that is the value the container was
	// actually handed; the remediation has to be a value the SCHEMA accepts, and the enum is
	// case-sensitive. Naming the artifact spelling in the remediation sends an operator to a field
	// that will reject it.
	//
	// IT IS RECORDED AND NOT DERIVED, and that is forced rather than preferred: mooncake's mapping is
	// not injective, so it has no inverse. Both auto and tcp render "tcp", so "which API value
	// produces this artifact value" has two answers there and one here. A helper computing it would
	// be correct for ascend and quietly wrong for the transport that has two -- the same shape of
	// defect this table exists to remove.
	//
	// Empty exactly when Required is empty. A row carrying one without the other would print a blank
	// where the remediation goes, which is why the pin below requires them together.
	RequiredAPIValue string
}

// engineTransportConstraint is the measured answer per engine: which transport that engine's store
// backend accepts.
//
// WHY IT IS A TABLE AND NOT AN `if`. The constraint is a fact about somebody else's release, on the
// external fact changes when that project ships a new build, and the only way to know is to read that
// build's source again. A conditional would put the fact in the control flow of whoever asked, where
// the next engine gets added in one of the two places.
//
// WHAT IT PREVENTS. An engine that requires a transport RAISES at startup on every other one, so the
// container never serves a request -- a functional failure rather than a degradation. And the
// condition is not that somebody chose a wrong transport: KVCacheBackend.spec.transport.protocol
// defaults to auto, which mooncake.MemberProtocol renders as "tcp", so a pool nobody configured hands
// vLLM-Ascend exactly the value it refuses. Everything left at its default is the case this exists for.
//
// HOW TO RE-CHECK AN ENTRY, because this table goes stale silently:
//
//  0. FIRST, find which store backend THIS PROJECT renders for that engine -- vllmConnectorFor and
//     renderSGLang decide it -- and re-check that one. A constraint on a backend we never select is
//     not this row's answer.
//  1. Find where that backend reads `protocol` off its config, and follow every use of the value.
//  2. A COMPARISON IS NOT A CONSTRAINT. Read what happens when it does not match: SGLang compares
//     protocol to "rdma" and its else branch takes the ordinary path, while vLLM-Ascend's else branch
//     raises. Only the second is a requirement. A re-check that counted comparisons would mark SGLang
//     as constrained, which is why the entries below record what the else branch DOES rather than
//     that a branch exists.
//
// HOW TO EXERCISE A ROW, which is not the same question. This table is keyed by Engines(), and that
// list is WIDER than SelectableEngines(): vLLM-Ascend is renderable but not nameable. NAMING it in
// the Pod annotation gets a refusal from ParseEngine, on the engine name and not on the transport -
// a reader who reads that as "the table is wrong" has been answered by a different rule.
//
// REACHING it from an annotated Pod is a different matter, and that refusal says how: name the plain
// engine and declare the runtime alongside it, which selects this row the same way the
// ModelDeployment path does from the role's accelerator. The spelling lives in the refusal rather
// than here, so there is one copy of it. The row is therefore not ModelDeployment-only, and a reader
// who stops at the refusal concludes a route does not exist when it does.
//
// A wrong entry fails in both directions, and neither direction is quiet: too strict refuses a pool
// that would have worked, too loose admits a container that raises before it serves anything.
var engineTransportConstraint = map[Engine]transportFacts{
	EngineVLLM: {
		Version: "v0.25.1",
		// protocol appears three times and is never compared: declared on the config class at :108,
		// read out of the file at :132, handed to store.setup() at :1045. No branch reads its value.
		Source:           "vllm/distributed/kv_transfer/kv_connector/v1/mooncake/store/worker.py:108,132,1045",
		Required:         "",
		RequiredAPIValue: "",
	},
	EngineVLLMAscend: {
		// REQUIRES ascend, and this is the entry the table exists for. MooncakeBackend's constructor
		// raises NotImplementedError for every protocol other than "ascend" at :193-194, so tcp,
		// rdma and hip each abort the container at startup.
		//
		// The file reader would have hidden this from a reader who stopped at the config class:
		// from_file defaults an ABSENT protocol key to "ascend" (:544). That default never applies
		// here, because vllmClientConfig.Protocol carries no omitempty and renderVLLMClientConfig
		// writes the field unconditionally -- so the value this project chose is always the one that
		// reaches :193.
		Version: "v0.19.1rc1-2120-gcdad5a32e",
		Source:  "vllm_ascend/distributed/kv_transfer/kv_pool/ascend_store/backend/mooncake_backend.py:193-194,544",
		// The two spellings of one transport. The API enum is lowercase
		// (KVCacheBackendTransport.Protocol), so cann is what a remediation may name.
		Required:         "ascend",
		RequiredAPIValue: "cann",
	},
	EngineSGLang: {
		// Unconstrained, and the reason is worth recording because this file DOES compare protocol:
		// :487 tests it against "rdma" as one of four conditions for reusing an already-initialized
		// transfer engine, and the else branch at :496-498 builds its own instead. Either way the
		// value reaches store.setup() at :515 unexamined. That comparison is an optimization, not a
		// requirement, and it is the reason step 2 above is worded the way it is.
		Version:          "v0.5.18",
		Source:           "python/sglang/srt/mem_cache/storage/mooncake_store/mooncake_store.py:98,487,496-498,515",
		Required:         "",
		RequiredAPIValue: "",
	},
}

// checkTransport refuses a transport the engine's store backend does not accept.
//
// It is the ONE place the table's answer becomes a refusal, so both surfaces that render a client -
// the Pod admission webhook and the ModelDeployment reconciler - say the same thing about the same
// pair. A second implementation would agree today and diverge on whichever engine release lands next.
//
// An engine with no measured entry is let through, which is the side that claims less: refusing on a
// fact nobody read would turn an unmeasured engine into a broken one, while admitting it leaves the
// failure exactly where it already was.
//
// The message names BOTH halves of the pair, because neither half is wrong on its own: the transport
// is a legal value and the engine is a legal engine, and it is the combination that raises. It also
// names WHERE the transport came from - a reader who goes looking for it on the Binding will not find
// it, since the pool's KVCacheBackend is what carries it.
//
// TWO SPELLINGS APPEAR ON PURPOSE, in the two sentences that need them. What the pool OFFERS and what
// the engine ACCEPTS are reported in the artifact's spelling, because that is the value handed to the
// container. What to SET is the API's, because that sentence names a field the schema validates and
// its enum is case-sensitive - naming the artifact spelling there produces a schema rejection, so the
// operator's next step fails for a second, unrelated reason.
//
// It does not restate what an unset protocol resolves to. That mapping is mooncake.memberProtocols',
// and a copy of it here would be a second implementation of one table.
func checkTransport(engine Engine, protocol string) error {
	facts := engineTransportConstraint[engine]
	if facts.Required == "" || facts.Required == protocol {
		return nil
	}

	return newRefusal(ReasonTransportUnsupported,
		"engine %q accepts only the %q transport and this pool offers %q, so its store backend "+
			"would raise at startup instead of using the cache (measured at %s, %s). The transport "+
			"belongs to the KVCacheBackend the pool names, not to the Binding: set that backend's "+
			"spec.transport.protocol to %q, which is that same transport in the API's spelling, or "+
			"point this workload at a pool that already offers it. An unset protocol is not neutral "+
			"here: the field defaults to auto, which the backend resolves to one concrete transport "+
			"rather than to whatever an engine wants",
		engine, facts.Required, protocol, facts.Version, facts.Source, facts.RequiredAPIValue)
}

// MatchTransport is checkTransport's pool-aware half: where the singular check answers whether one
// engine-transport pair runs, this answers WHICH of a pool's effective transports the engine is
// handed. The offers are each member group's effective protocol in declaration order, computed by
// the caller through mooncake.MemberProtocols.
//
// THE MATCH IS THE FIRST OFFER IN DECLARATION ORDER THAT THE ENGINE'S CONSTRAINT ACCEPTS — and the
// first offer outright for an unconstrained engine, which accepts them all. Nothing matches an
// engine to a specific group: a pool names exactly one backend, and the engine only learns the
// master address, so the rule has to pick without a binding. Declaration order is deterministic
// and costs nothing to explain, and every accepted offer is one the engine can run on.
//
// A refusal means NO group satisfies the constraint, which is the case worth failing loudly for:
// admitted, the engine's store backend raises at startup on every group the pool has, and the
// container never serves a request. An engine with no measured entry claims less and is let
// through, on the same rule as the singular check.
//
// An empty offer list is NOT a transport answer: it is the no-store shape, which Render refuses
// for its own reason, so this returns no protocol and no error rather than borrowing that case.
//
// An empty offer INSIDE the list is a different thing and is skipped. A group's effective protocol
// is the artifact's spelling looked up from the API's, so an API value with no entry in that map
// resolves to the empty string. Nothing produces one today — a guard test asserts every enum value
// has an entry — but an unconstrained engine accepts any offer, so without this the ninth enum value
// added without its map entry would be handed to the engine as an empty transport variable rather
// than refused. Skipping rather than refusing outright is what lets the groups that DID resolve still
// answer; a pool where none of them did falls through to the refusal below.
func MatchTransport(engine Engine, offers []string) (string, error) {
	if len(offers) == 0 {
		return "", nil
	}

	facts := engineTransportConstraint[engine]
	for _, offer := range offers {
		if offer == "" {
			continue
		}
		if facts.Required == "" || facts.Required == offer {
			return offer, nil
		}
	}

	return "", newRefusal(ReasonTransportUnsupported,
		"engine %q accepts only the %q transport and no group in this pool offers it — the groups "+
			"offer %q — so its store backend would raise at startup instead of using the cache "+
			"(measured at %s, %s). The transport belongs to the KVCacheBackend the pool names, not "+
			"to the Binding: set that backend's spec.transport.protocol to %q or one member group's "+
			"transport.protocol, which is that same transport in the API's spelling, or point this "+
			"workload at a pool that already offers it. An unset protocol is not neutral here: the "+
			"field defaults to auto, which the backend resolves to one concrete transport rather "+
			"than to whatever an engine wants",
		engine, facts.Required, offers, facts.Version, facts.Source, facts.RequiredAPIValue)
}
