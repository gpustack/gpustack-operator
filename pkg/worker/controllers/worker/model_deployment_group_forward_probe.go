// Engine group-forward observation: the synthetic request that makes the GroupForward leg
// verifiable for a non-disaggregated multi-rank replica.
//
// A REPLICA'S FORWARD PATH IS VERIFIED BY MAKING IT CARRY A REQUEST AND SEEING THAT ONE COME BACK.
// The probe is one request per member, of one token, on the engine's own serving route, carrying a
// request identifier this controller chose. What makes the answer evidence rather than liveness is
// the binding: the response has to come back stamped with THAT identifier, so a server that is
// merely alive, or one whose answer belongs to some other request, cannot be mistaken for a
// forward that happened. Both engines expose exactly that binding at the versions this program
// documents, and neither exposes it on a health route — which is why the leg could not be verified
// before.
//
// THE PROBE COSTS THE ENGINE ONE TOKEN, and it is charged only where it buys something: this runs
// on the qualification evaluation of a HELD replica, which is the admission or the restore of a
// group that is currently held. An admitted replica is never probed, so a serving deployment pays
// nothing, and re-evaluation is the next reconcile rather than a retry inside this one.
//
// THE vLLM PROBE'S LOG FOOTPRINT IS ACCEPTED AND CANNOT BE SUPPRESSED PER REQUEST. vLLM's OpenAI
// protocol at both versions this program reads carries no per-request metrics switch, so the probe
// appears in the engine's request log and its success counters like any other request. Making it
// disappear would mean switching off request logging for all production traffic to hide one rare
// probe, which is a worse trade than being visible. The identifier is therefore SELF-DESCRIBING, so
// an operator reading those logs can filter the probe out: it always begins with
// modelDeploymentQualifyRIDPrefix. SGLang does carry a per-request switch and the probe sets it.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// modelDeploymentQualifyRIDPrefix marks every request this operator makes on an engine's serving
// route, so the probe is identifiable in the engine's own logs and counters by anyone reading them.
const modelDeploymentQualifyRIDPrefix = "gpustack-qualify-"

// modelDeploymentGroupForwardBound is how long the whole replica's probe may take. It is ONE
// budget shared across every member asked, not one per member: the members are probed
// sequentially under it, so an early member that answers slowly consumes the budget the later
// members would have used. The bound sizes the whole pass's patience for one replica, exactly
// like the router observation budget sizes one collection.
//
// IT IS A BOUND AND NOT A DEADLINE THE ENGINE HONORS, because neither engine's request struct
// carries a timeout: vLLM's CompletionRequest at both versions read here has no such field, and
// SGLang's GenerateReqInput has none either. A member that is saturated therefore queues the
// request rather than refusing it, and the only thing standing between a busy engine and a blocked
// reconcile is this. The budget is deliberately generous relative to one token of work, because the
// cost of expiring early is a healthy group held for a whole pass.
const modelDeploymentGroupForwardBound = 15 * time.Second

// modelDeploymentGroupForwardMaxResponseBytes bounds what a probe will read. One token of
// answer is a small document, and a member answering with something enormous is a member whose
// answer cannot be bound.
const modelDeploymentGroupForwardMaxResponseBytes = 1 << 20

// modelDeploymentGroupForwardFetch performs one probe request against one member's engine and
// returns the response body.
//
// IT IS A FUNCTION RATHER THAN A DIAL BECAUSE THE STATES THIS FILE HAS TO GET RIGHT ARE FAILURES,
// and a real dial cannot be made to fail on demand. A nil fetch means the leg has no transport and
// therefore makes no observation, which reads Unknown and holds.
type modelDeploymentGroupForwardFetch func(ctx context.Context, url string, body []byte) ([]byte, error)

// modelDeploymentGroupForwardEngine is everything this operator knows how to do with ONE engine:
// the route the probe posts to, the versions whose binding was read at that version, how to render
// the request, and how to read the binding back out of the response.
//
// A MISSING ENTRY MEANS NO VERIFIED OBSERVATION EXISTS FOR THAT ENGINE, and the leg says so by
// name rather than guessing at one.
type modelDeploymentGroupForwardEngine struct {
	// Path is the engine route the probe posts to.
	Path string
	// Binding names, in words, the property that makes a response evidence of THIS operator's
	// request. It appears in the reason a version is refused, because "the binding was not read at
	// your version" is only actionable if the reader knows which binding was meant.
	Binding string
	// VerifiedVersions are the engine versions whose probe semantics were read at that version.
	// A version outside this set is Unsupported, and the reason names the version, because a
	// binding that was not read is not a binding.
	VerifiedVersions []string
	// DisaggregatedReason says what a real request cannot prove on this engine's disaggregated
	// shape. It is engine-specific because the two engines fail the proof differently, and a
	// reader told only "P/D is unsupported" cannot tell which obstacle is in the way.
	DisaggregatedReason string
	// Body renders the probe request for one member.
	Body func(rid string) ([]byte, error)
	// Bound reports whether a response proves THAT request completed. It is given the raw body so
	// a malformed document reads as "not bound" rather than as a parse error the caller has to
	// distinguish.
	Bound func(body []byte, rid string) bool
}

// modelDeploymentGroupForwardEngines is the per-engine matrix. An engine absent from it has no
// verified observation, and the leg names it rather than holding with a guess at a reason.
var modelDeploymentGroupForwardEngines = map[string]modelDeploymentGroupForwardEngine{
	workercore.ModelDeploymentEngineVLLM: {
		// /v1/completions is the route whose request struct carries request_id, documented at both
		// versions this program reads as "used through out the inference process and return in
		// response", and whose serving path returns it as the response id prefixed "cmpl-". The
		// model field is left out: it is optional at both versions, and naming a model the engine
		// does not serve would turn a working group into a 404 and a permanent hold.
		Path:    "/v1/completions",
		Binding: `the response id is "cmpl-" + our request_id`,
		VerifiedVersions: []string{
			"0.25.1", "0.29.0",
		},
		DisaggregatedReason: "vLLM's disaggregated transfer is a client-coordinated pair of " +
			"requests, so one request proves only the forward path of the instance that served it " +
			"and says nothing about the transfer between the prefill and decode roles",
		Body: func(rid string) ([]byte, error) {
			return json.Marshal(map[string]any{
				"prompt":      modelDeploymentQualifyPrompt,
				"max_tokens":  1,
				"temperature": 0,
				"request_id":  rid,
			})
		},
		Bound: func(body []byte, rid string) bool {
			var response struct {
				ID      string `json:"id"`
				Choices []struct {
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal(body, &response) != nil {
				return false
			}
			if response.ID != "cmpl-"+rid {
				return false
			}

			return len(response.Choices) > 0 && response.Choices[0].FinishReason != nil
		},
	},
	workercore.ModelDeploymentEngineSGLang: {
		// /generate takes no model field, so there is nothing to get wrong there. The response
		// carries meta_info.id, which is the rid this operator sent, plus the finish reason the
		// engine recorded for it.
		Path:    "/generate",
		Binding: `the response meta_info.id is our rid`,
		VerifiedVersions: []string{
			// 0.5.18 is the version the rid echo was read at. 0.5.21 is admitted on the
			// surrounding handler semantics, which are byte-identical there, and that asymmetry
			// is stated in the guard's reason rather than smoothed over.
			"0.5.18", "0.5.21",
		},
		DisaggregatedReason: "on a disaggregated shape SGLang's own readiness request takes the " +
			"fake-bootstrap shortcut, which skips the all-reduce that would gate the transfer on " +
			"every tensor-parallel rank, and a real bootstrap is allocated outside the engine by a " +
			"component this operator does not run",
		Body: func(rid string) ([]byte, error) {
			return json.Marshal(map[string]any{
				"text":        modelDeploymentQualifyPrompt,
				"rid":         rid,
				"log_metrics": false,
				"sampling_params": map[string]any{
					"max_new_tokens": 1,
					"temperature":    0,
				},
			})
		},
		Bound: func(body []byte, rid string) bool {
			var response struct {
				MetaInfo struct {
					ID string `json:"id"`
					// The engine records its finish reason as a DOCUMENT keyed by type rather than
					// as a string, so this is read as raw JSON: the question is whether a reason
					// arrived, not what shape it took. A string-typed field would reject every real
					// answer this engine gives and hold a working group forever.
					FinishReason json.RawMessage `json:"finish_reason"`
				} `json:"meta_info"`
			}
			if json.Unmarshal(body, &response) != nil {
				return false
			}
			if response.MetaInfo.ID != rid {
				return false
			}

			reason := strings.TrimSpace(string(response.MetaInfo.FinishReason))

			return reason != "" && reason != "null"
		},
	},
}

// modelDeploymentQualifyPrompt is the text the probe sends. It is a single token of work on either
// engine, and its content does not matter: what is being observed is that the request came back
// bound to its own identifier.
const modelDeploymentQualifyPrompt = "ok"

// observeModelDeploymentGroupForward classifies the group-forward evidence for one replica.
//
// The order of the questions below is the order of how much they cost to be wrong. A single-member
// replica is answered before anything else because it is the shape that must not move. A shape or
// version this operator has not verified is answered before any network call because an observation
// that was never read is not an observation. Only what survives both is worth a request.
func observeModelDeploymentGroupForward(
	ctx context.Context, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	view modelDeploymentReplicaView, fetch modelDeploymentGroupForwardFetch,
) modelDeploymentGroupForward {
	if len(view.Members) <= 1 {
		return modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardNotApplicable,
			Reason: "a replica of one member has no cross-member forward path to verify",
		}
	}

	engineName := md.Spec.Engine.Name
	engine, known := modelDeploymentGroupForwardEngines[engineName]
	if !known {
		return modelDeploymentGroupForwardUnsupportedFor(
			"engine " + engineName + " has no group-forward observation this operator has verified",
		)
	}

	if !slices.Contains(engine.VerifiedVersions, md.Spec.Engine.Version) {
		return modelDeploymentGroupForwardUnsupportedFor(
			"the " + engineName + " " + md.Spec.Engine.Version + " probe binding has not been " +
				"read at that version, and " + engine.Binding + " was read at " +
				strings.Join(engine.VerifiedVersions, " and "),
		)
	}

	if workercore.ModelDeploymentRoleKindPrefill == ModelDeploymentEffectiveRoleKind(role) ||
		workercore.ModelDeploymentRoleKindDecode == ModelDeploymentEffectiveRoleKind(role) {
		return modelDeploymentGroupForwardUnsupportedFor(engine.DisaggregatedReason)
	}

	if fetch == nil {
		return modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardUnknown,
			Reason: "no group-forward transport is wired, so no member could be asked",
		}
	}

	return probeModelDeploymentGroupForward(ctx, engine, view, fetch)
}

// modelDeploymentGroupForwardUnsupportedFor is the Unsupported answer with the reason that says
// which obstacle is in the way, kept in one place so no two call sites word it differently.
func modelDeploymentGroupForwardUnsupportedFor(reason string) modelDeploymentGroupForward {
	return modelDeploymentGroupForward{
		State:  modelDeploymentGroupForwardUnsupported,
		Reason: reason,
	}
}

// probeModelDeploymentGroupForward asks EVERY member of the replica and requires every answer to
// be bound to its own request.
//
// EVERY MEMBER, because the leg is about a group and one member's success says nothing about the
// others: a member whose peers are gone completes its own forward only for the collectives it owns,
// and a group whose cross-member wiring has failed leaves exactly the members that can still
// answer alone in saying so. Requiring all of them is what makes the answer a statement about the
// replica.
//
// A SINGLE UNBOUND OR UNANSWERED MEMBER IS UNKNOWN, NEVER FAILED. The engine said nothing about
// the health of the group; this operator ran out of patience, ran into a queue, or read a document
// it could not bind. A probe that cannot be understood is an absence of evidence, and the definite
// health legs are where a fault belongs.
func probeModelDeploymentGroupForward(
	ctx context.Context, engine modelDeploymentGroupForwardEngine,
	view modelDeploymentReplicaView, fetch modelDeploymentGroupForwardFetch,
) modelDeploymentGroupForward {
	probeCtx, cancel := context.WithTimeout(ctx, modelDeploymentGroupForwardBound)
	defer cancel()

	for _, member := range view.Members {
		rid := modelDeploymentQualifyRIDPrefix + string(member.UID)
		body, err := engine.Body(rid)
		if err != nil {
			return modelDeploymentGroupForwardUnknownFor(
				"the probe request for " + member.Name + " could not be rendered: " + err.Error())
		}

		url, err := modelDeploymentGroupForwardURL(member, engine.Path)
		if err != nil {
			return modelDeploymentGroupForwardUnknownFor(
				"member " + member.Name + " " + err.Error())
		}

		response, err := fetch(probeCtx, url, body)
		if err != nil {
			return modelDeploymentGroupForwardUnknownFor(
				"member " + member.Name + " did not answer the probe within the bound: " +
					err.Error() + "; an unanswered probe is an absence of evidence, not a fault")
		}
		if !engine.Bound(response, rid) {
			return modelDeploymentGroupForwardUnknownFor(
				"member " + member.Name + " answered, but not bound to this operator's own " +
					"request, so the answer is not evidence that this group forwarded one")
		}
	}

	return modelDeploymentGroupForward{
		State:  modelDeploymentGroupForwardVerified,
		Reason: "every member returned the one-token request this operator sent, bound to its own identifier",
		// The observation is a point in time read inside this pass, and it is only ever read
		// while the replica is held, so it carries no freshness state of its own: there is no
		// later pass that could act on a stale copy of it.
		Generation:  modelDeploymentGenerationOf(view),
		Observation: strconv.Itoa(len(view.Members)) + " members answered, each bound to its own request",
	}
}

func modelDeploymentGroupForwardUnknownFor(reason string) modelDeploymentGroupForward {
	return modelDeploymentGroupForward{
		State:  modelDeploymentGroupForwardUnknown,
		Reason: reason,
	}
}

// modelDeploymentGroupForwardURL builds the member's own serving URL from the annotations the
// render already stamps on it, so the probe needs nothing this operator did not already write.
//
// THE ANNOTATION IS A HARD REQUIREMENT, AND A TAKE-OVER ROLE HAS NONE. The render stamps
// prometheus.io/port only for roles running the operator's own command line: for a replaced
// command the operator cannot know what the replacement serves, so it stamps nothing. A
// multi-member take-over role therefore cannot be probed and reads Unknown; no port is guessed
// from the container spec, because inventing a serving contract the command never declared
// would turn absence of evidence into a claim.
func modelDeploymentGroupForwardURL(member *core.Pod, path string) (string, error) {
	if member.Status.PodIP == "" {
		return "", fmt.Errorf("has no address to be asked at yet")
	}

	port := member.Annotations["prometheus.io/port"]
	if port == "" {
		return "", fmt.Errorf("does not carry the engine's serving port")
	}
	scheme := member.Annotations["prometheus.io/scheme"]
	if scheme == "" {
		scheme = "http"
	}

	return fmt.Sprintf("%s://%s:%s%s", scheme, member.Status.PodIP, port, path), nil
}

// defaultGroupForwardFetch is the production transport: a plain POST against the URL built from
// the member Pod's own IP and the serving port its render stamped, never a Service, which would
// sample one replica and silently answer for the rest.
//
// It is the same shape the router observation uses for its own reads, and deliberately NOT the exec
// channel the drain reader uses: that channel runs something inside a container and belongs to a
// different task. Nothing here reaches into a member to run anything.
func defaultGroupForwardFetch(ctx context.Context, url string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("probe request could not be built: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	// The bound is on the RESPONSE too. A member that answers with something enormous is not a
	// member whose answer this operator can bind, and reading it whole would spend the reconcile's
	// remaining budget on a document it will discard.
	payload, err := io.ReadAll(io.LimitReader(response.Body, modelDeploymentGroupForwardMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("probe response could not be read: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine answered %s", response.Status)
	}

	return payload, nil
}
