// This file holds what is measured about each engine rather than what is rendered for it: whether a
// given engine version forwards a reuse identity to the store, and which transport its store backend
// accepts.
//
// THE TWO ANSWERS ARE USED DIFFERENTLY, and the asymmetry is the measurement's, not a policy choice.
// The tenant answer is STAMPED: every Binding declares a reuse domain, so a refusal keyed on that
// would reject every Pod, and an engine that ignores the key ignores it in silence. The transport
// answer is a REFUSAL: an engine handed a transport its backend does not accept raises before it
// serves a request, so admitting the pair buys nothing and costs a container that cannot start.
//
// It is kept apart from the renderers because it ages differently. A renderer changes when this
// project decides to write something else; this table changes when somebody else ships a new engine
// build, and the only way to know is to go and read that build's source again.
package inject

// engineFacts is what has been measured about one engine version.
//
// It carries the version and the source line beside the answer on purpose. A bare boolean would be
// unfalsifiable six months from now: a reader could not tell whether "does not forward" was measured
// at a version still in use, or inherited from one nobody runs. With both recorded, the
// discriminating check below can be re-run without first finding the file again.
type engineFacts struct {
	// Version is the engine release the facts below were read from.
	Version string

	// TenantSource is the file and line the answer below was read at, in that release.
	//
	// It names the tenant measurement specifically, because this engine has a second measured answer
	// in transportFacts read from other lines of the same file. A field called Source would be
	// correct in one type and misleading in the other.
	TenantSource string

	// ForwardsTenant is whether a tenant identity reaches the store ON THE PATH THIS PROJECT RENDERS.
	//
	// Not "whether the engine supports a tenant". Answer the question about our configuration, not
	// about the engine. This row has been wrong in both directions for that one reason: vLLM-Ascend
	// was once marked true off a tenant-aware store that exists on upstream main and NOT on the
	// v0.19.1rc1 we pin, and the note recording that correction then predicted the answer would flip
	// back if we ever selected that engine's own connector. We now do, and it did not. Which connector we render and
	// whether a tenant travels are independent facts, and neither is readable off the other.
	//
	// It says only WHETHER a tenant arrives; the renderer decides HOW, and neither is derivable from
	// the other. SGLang takes MOONCAKE_TENANT_ID from the environment; vLLM has no tenant key at all.
	ForwardsTenant bool
}

// engineTenantSupport is the measured answer per engine.
//
// WHY IT CANNOT SIMPLY GO AWAY, since removing it reads as tidying. Tenant injection CANNOT FAIL
// LOUD: an engine that does not read the key ignores it in silence, and nothing here observes that
// afterwards. So the injection stamp records the ACTION and its BASIS instead of an outcome, and
// this table IS that basis -- which engine version, and which source line, the answer came from.
// Deleting it would not remove an unused lookup; it would leave the stamp asserting something with
// nothing behind it.
//
// HOW TO RE-CHECK AN ENTRY, because this table is the one thing here that goes stale silently:
//
//  0. FIRST, find which connector or loader THIS PROJECT renders for that engine, and re-check that
//     one. Steps 1 and 2 answer a question about the engine; this row answers a question about our
//     configuration. Skipping this step is how the vLLM-Ascend row stayed true while the tenant it
//     described was being dropped: the capability was real, on a code path we never named.
//  1. Open the config class that connector actually reads, and look for a tenant key. A version that
//     grows or loses one changes this row.
//  2. Find that path's `.setup(` call and count the positional arguments. The tenant is the 11th
//     parameter of the client's positional overload, so anything shorter truncates before it.
//     A call using the dict overload forwards everything and needs no count.
//
// Both must pass before an entry becomes true. Flipping one flips every stamp from "not enforced" to
// "enforced", which is why the spec lists this table as a change to ask about rather than one to make:
// a wrong entry here does not fail, it makes the operator's one record of isolation state a lie.
var engineTenantSupport = map[Engine]engineFacts{
	EngineVLLM: {
		Version: "v0.25.1",
		// MooncakeStoreConfig has no tenant key, and the setup() call passes seven positional
		// arguments -- local_hostname, metadata_server, global_segment_size, local_buffer_size,
		// protocol, device_name, master_server_address -- stopping four short of the tenant.
		TenantSource:   "vllm/distributed/kv_transfer/kv_connector/v1/mooncake/store/worker.py:96-152,1040-1048",
		ForwardsTenant: false,
	},
	EngineVLLMAscend: {
		// FALSE, and as of the connector fix this is now the SIMPLE case: renderVLLM selects
		// AscendStoreConnector for this engine, so we do render the engine's own store path - and
		// that path still carries no tenant. The pinned v0.19.1rc1 has none anywhere: a grep over
		// vllm_ascend/ excluding tests returns zero hits, and its store config takes six keys with
		// no tenant among them (mooncake_backend.py:115-124).
		//
		// The history is kept because this row was wrong twice, each time by reading the answer off
		// something adjacent to it:
		//
		//  1. Marked TRUE, citing mooncake_backend.py:166-167,344,379 on upstream main. Those lines
		//     are real - they are on a connector that release does not have and we did not name.
		//  2. Corrected to FALSE with the reason "we render MooncakeStoreConnector, so that path is
		//     never taken". The verdict was right and the reason made a prediction that was wrong:
		//     that selecting AscendStoreConnector would flip this to true. We now select it and this
		//     row is unchanged.
		//
		// What the correction missed is worth more than the correction: "we render a name this
		// engine does not register" is not a statement about tenants at all. Its stronger meaning is
		// that the engine could not start, since the factory resolves that name against a registry.
		// The tenant question absorbed the fact and stopped.
		Version:        "v0.19.1rc1",
		TenantSource:   "vllm_ascend/.../ascend_store/backend/mooncake_backend.py:115-124 (the store we now select)",
		ForwardsTenant: false,
	},
	EngineSGLang: {
		// This one DOES forward, and the entry was wrong until a review caught it. The previous
		// value was read at "gateway-v0.3.1-1689" - a tag this project neither deploys nor tests,
		// which looked like a precise qualifier and qualified the wrong object. Re-read at the
		// version the runner catalog actually ships and this suite actually runs against.
		//
		// All three config paths take a tenant_id: from_file at :164, load_from_env at :208 (from
		// MOONCAKE_TENANT_ID), load_from_extra_config at :257. The store call then adds it to its
		// keyword arguments at :505, but only when it differs from the literal "default" - which is
		// why omitting a tenant and passing "default" behave identically against a master, an
		// observation this project made on a live cluster before knowing the cause. A client library
		// too old for the argument raises at :528 rather than dropping it, so that direction fails
		// closed; an SGLang build older than the variable simply never reads it, and that direction
		// has no signal at all.
		Version:        "v0.5.18",
		TenantSource:   "python/sglang/srt/mem_cache/storage/mooncake_store/mooncake_store.py:107,164,208,257,505-533",
		ForwardsTenant: true,
	},
}

// transportFacts is what has been measured about one engine version's transport requirement.
//
// The version and the source line sit beside the answer for the same reason they do on engineFacts: a
// bare string would be unfalsifiable later, and the discriminating check below has to be re-runnable
// without first finding the file again.
type transportFacts struct {
	// Version is the engine release the answer was read from.
	Version string

	// Source is the file and line the answer was read at, in that release.
	//
	// It is a field of its own rather than a share of engineFacts.TenantSource, because on
	// vLLM-Ascend the two answers come from different lines of the same file: the tenant question is
	// answered by the store config's key list, the transport question by a branch in the backend's
	// constructor. One field for both would send a reader re-checking one of them to the lines that
	// answer the other.
	Source string

	// Required is the ONE transport this engine's store backend accepts, in the artifact's own
	// spelling -- what mooncake.MemberProtocol renders, not what the API enum publishes.
	//
	// Empty means the engine refuses no transport, and that is a MEASURED answer rather than a
	// missing one: every engine has an entry and its Source names where the answer was read. The
	// distinction decides opposite outcomes -- an unmeasured engine has to be let through, an engine
	// measured as requiring "ascend" must not be.
	Required string
}

// engineTransportConstraint is the measured answer per engine: which transport that engine's store
// backend accepts.
//
// WHY IT IS A TABLE AND NOT AN `if`. The constraint is a fact about somebody else's release, on the
// same footing as the tenant answer above: it moves when that project ships a new build, and the only
// way to know is to read that build's source again. A conditional would put the fact in the control
// flow of whoever asked, where the next engine gets added in one of the two places.
//
// WHAT IT PREVENTS. An engine that requires a transport RAISES at startup on every other one, so the
// container never serves a request -- a functional failure rather than a degradation. And the
// condition is not that somebody chose a wrong transport: KVCacheBackend.spec.transport.protocol
// defaults to Auto, which mooncake.MemberProtocol renders as "tcp", so a pool nobody configured hands
// vLLM-Ascend exactly the value it refuses. Everything left at its default is the case this exists for.
//
// HOW TO RE-CHECK AN ENTRY, because this table goes stale silently just as the tenant one does:
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
// list is WIDER than SelectableEngines(): vLLM-Ascend is renderable but not nameable, so its row is
// reachable only through the ModelDeployment path, where the operator derives the engine from the
// role's accelerator. Trying to trigger it from the Pod annotation instead gets a refusal from
// ParseEngine, on the engine name and not on the transport - a reader who reads that as "the table is
// wrong" has been answered by a different rule.
//
// A wrong entry fails in both directions, and neither direction is quiet: too strict refuses a pool
// that would have worked, too loose admits a container that raises before it serves anything.
var engineTransportConstraint = map[Engine]transportFacts{
	EngineVLLM: {
		Version: "v0.25.1",
		// protocol appears three times and is never compared: declared on the config class at :108,
		// read out of the file at :132, handed to store.setup() at :1045. No branch reads its value.
		Source:   "vllm/distributed/kv_transfer/kv_connector/v1/mooncake/store/worker.py:108,132,1045",
		Required: "",
	},
	EngineVLLMAscend: {
		// REQUIRES ascend, and this is the entry the table exists for. MooncakeBackend's constructor
		// admits protocol == "ascend" at :34 and raises NotImplementedError for everything else at
		// :68-69, so tcp, rdma and hip each abort the container at startup.
		//
		// The file reader would have hidden this from a reader who stopped at the config class:
		// from_file defaults an ABSENT protocol key to "ascend" (:121). That default never applies
		// here, because vllmClientConfig.Protocol carries no omitempty and renderVLLMClientConfig
		// writes the field unconditionally -- so the value this project chose is always the one that
		// reaches :34.
		Version:  "v0.19.1rc1",
		Source:   "vllm_ascend/.../ascend_store/backend/mooncake_backend.py:34,68-69 (v0.19.1rc1 is da421afa)",
		Required: "ascend",
	},
	EngineSGLang: {
		// Unconstrained, and the reason is worth recording because this file DOES compare protocol:
		// :487 tests it against "rdma" as one of four conditions for reusing an already-initialized
		// transfer engine, and the else branch at :496-498 builds its own instead. Either way the
		// value reaches store.setup() at :515 unexamined. That comparison is an optimization, not a
		// requirement, and it is the reason step 2 above is worded the way it is.
		Version:  "v0.5.18",
		Source:   "python/sglang/srt/mem_cache/storage/mooncake_store/mooncake_store.py:98,487,496-498,515",
		Required: "",
	},
}

// engineFactsFor returns what was measured for an engine, and whether anything was.
//
// The second return distinguishes "measured as truncating" from "never measured", which the zero
// value alone cannot: both would read as false, and only one of them is a fact.
func engineFactsFor(engine Engine) (engineFacts, bool) {
	facts, ok := engineTenantSupport[engine]
	return facts, ok
}

// SupportsTenant reports whether the given engine forwards a reuse identity to the store.
//
// It reads the measured table and nothing else, so the verdict built on it is data-driven: when an
// engine starts forwarding a tenant, one table entry changes and the stamp follows on its own. An
// unknown engine reports false, which is the side that claims less.
//
// True does NOT mean the Pod is isolated. It means a tenant is emitted on the path we render; whether
// the build in the container reads it is not knowable here, which is why the stamp records the action
// and never a verdict.
func SupportsTenant(engine Engine) bool {
	return engineTenantSupport[engine].ForwardsTenant
}

// TenantSupportSource returns the version and source line behind an engine's answer, so a stamp or a
// message says why rather than only what. An unmeasured engine yields empty strings, and a caller
// should treat that as the unknown-engine case rather than printing blanks.
func TenantSupportSource(engine Engine) (version, source string) {
	facts := engineTenantSupport[engine]
	return facts.Version, facts.TenantSource
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
			"spec.transport.protocol to %[2]q, or point this workload at a pool that already offers "+
			"it. An unset protocol is not neutral here: the field defaults to Auto, which the "+
			"backend resolves to one concrete transport rather than to whatever an engine wants",
		engine, facts.Required, protocol, facts.Version, facts.Source)
}
