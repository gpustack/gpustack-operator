// This file holds what is measured about each engine rather than what is rendered for it: whether a
// given engine version forwards a reuse identity to the store. The admission webhook reads the answer
// and stamps it onto the Pod; nothing here refuses, because every Binding declares a reuse domain and a
// refusal keyed on that would reject every Pod.
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

	// Source is the file and line the answer was read at, in that release.
	Source string

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
// WHO READS IT, AND WHY IT CANNOT SIMPLY GO AWAY. Removing this table has been proposed and is
// declared void; the reason is recorded here because the proposal was reasonable and will occur to
// the next reader too. It has three live consumers: engineFactsFor, which separates "measured as
// truncating" from "never measured" and gates injection on the difference; SupportsTenant, which is
// what the SGLang renderer asks before emitting a tenant; and TenantSupportSource, which the
// admission path reads to stamp the version behind the answer.
//
// The third one settles it. Tenant injection CANNOT FAIL LOUD -- an engine that does not read the
// key ignores it in silence, and neither the config key nor the environment variable is observable
// from here afterwards. That is why the stamp records the ACTION and its BASIS rather than an
// outcome, and this table is that basis: which engine version, and which source line, the answer
// came from. Deleting it would not remove an unused lookup, it would remove the evidence a decision
// already made is standing on, leaving a stamp that asserts something with nothing behind it.
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
		Source:         "vllm/distributed/kv_transfer/kv_connector/v1/mooncake/store/worker.py:96-152,1040-1048",
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
		Source:         "vllm_ascend/.../ascend_store/backend/mooncake_backend.py:115-124 (the store we now select)",
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
		Source:         "python/sglang/srt/mem_cache/storage/mooncake_store/mooncake_store.py:107,164,208,257,505-533",
		ForwardsTenant: true,
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
	return facts.Version, facts.Source
}
