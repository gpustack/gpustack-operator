// Engine group-forward observation: the synthetic request that makes the GroupForward leg
// verifiable for a non-disaggregated multi-rank replica.
//
// A REPLICA'S FORWARD PATH IS VERIFIED BY MAKING IT CARRY A REQUEST AND SEEING THAT ONE COME BACK.
// The probe is one request per member, of one token, on the engine's own serving route, carrying a
// request identifier this controller chose. What makes the answer evidence rather than liveness is
// the binding: the response has to come back stamped with THAT identifier, so a server that is
// merely alive, or one whose answer belongs to some other request, cannot be mistaken for a
// forward that happened. Both engines expose exactly that binding on their serving routes, and
// neither exposes it on a health route — which is why the leg could not be verified before.
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
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/json"
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
// the route the probe posts to, how to render the request, and how to read the binding back out
// of the response.
//
// A MISSING ENTRY MEANS NO VERIFIED OBSERVATION EXISTS FOR THAT ENGINE, and the leg says so by
// name rather than guessing at one.
type modelDeploymentGroupForwardEngine struct {
	// Path is the engine route the probe posts to.
	Path string
	// Binding names, in words, the property that makes a response evidence of THIS operator's
	// request.
	Binding string
	// DisaggregatedReason says what a real request cannot prove on this engine's disaggregated
	// shape. It is engine-specific because the two engines fail the proof differently, and a
	// reader told only "P/D is unsupported" cannot tell which obstacle is in the way.
	DisaggregatedReason string
	// Body renders the probe request for one member.
	Body func(rid string) ([]byte, error)
	// Bound reads the answer. It returns an empty string when the answer is bound to this
	// operator's own request and says the request ran to completion, and otherwise the reason the
	// answer is not evidence of either. It is given the raw body so a malformed document reads as
	// a refusal rather than as a parse error the caller has to distinguish.
	Bound func(body []byte, rid string) string
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
		DisaggregatedReason: "vLLM's disaggregated transfer is a client-coordinated pair of " +
			"requests, so one request proves only the forward path of the instance that served it " +
			"and says nothing about the transfer between the prefill and decode roles; the " +
			"directed pair probe is the path that answers it, and it needs both groups' snapshots, " +
			"which this call site does not carry",
		Body: func(rid string) ([]byte, error) {
			return json.Marshal(map[string]any{
				"prompt":      modelDeploymentQualifyPrompt,
				"max_tokens":  1,
				"temperature": 0,
				"request_id":  rid,
			})
		},
		Bound: func(body []byte, rid string) string {
			var response struct {
				ID      string `json:"id"`
				Choices []struct {
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
			}
			if json.Unmarshal(body, &response) != nil {
				return modelDeploymentQualifyUnreadableAnswer
			}
			if response.ID != "cmpl-"+rid {
				return modelDeploymentQualifyUnboundAnswer
			}
			if len(response.Choices) == 0 || response.Choices[0].FinishReason == nil {
				return modelDeploymentQualifyUnfinishedAnswer
			}
			if !modelDeploymentQualifyFinishRan(*response.Choices[0].FinishReason) {
				return modelDeploymentQualifyAbortedAnswer
			}

			return ""
		},
	},
	workercore.ModelDeploymentEngineSGLang: {
		// /generate takes no model field, so there is nothing to get wrong there. The response
		// carries meta_info.id, which is the rid this operator sent, plus the finish reason the
		// engine recorded for it.
		Path:    "/generate",
		Binding: `the response meta_info.id is our rid`,
		DisaggregatedReason: "a prefill or decode group is verified by a directed pair probe, " +
			"which needs both groups' snapshots, and this call site carries only one of them; " +
			"the engine's own readiness request cannot stand in for it, because that request " +
			"takes the fake-bootstrap shortcut, which skips the all-reduce that would gate the " +
			"transfer on every tensor-parallel rank",
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
		Bound: func(body []byte, rid string) string {
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
				return modelDeploymentQualifyUnreadableAnswer
			}
			if response.MetaInfo.ID != rid {
				return modelDeploymentQualifyUnboundAnswer
			}
			reason := modelDeploymentQualifyFinishType(response.MetaInfo.FinishReason)
			if reason == "" {
				return modelDeploymentQualifyUnfinishedAnswer
			}
			if !modelDeploymentQualifyFinishRan(reason) {
				return modelDeploymentQualifyAbortedAnswer
			}

			return ""
		},
	},
}

// modelDeploymentQualifyPrompt is the text the probe sends. It is a single token of work on either
// engine, and its content does not matter: what is being observed is that the request came back
// bound to its own identifier.
const modelDeploymentQualifyPrompt = "ok"

// The refusals an answer earns when it is not evidence that the group forwarded the request. Each
// one says what about the answer is missing, because an answer that cannot be used is a hold and a
// hold has to say why.
const (
	// modelDeploymentQualifyUnreadableAnswer is an answer this operator cannot read at all.
	modelDeploymentQualifyUnreadableAnswer = "answered with a document this operator cannot read, " +
		"so the answer is not evidence that this group forwarded one"
	// modelDeploymentQualifyUnboundAnswer is an answer that carries somebody else's request.
	modelDeploymentQualifyUnboundAnswer = "answered, but not bound to this operator's own request, " +
		"so the answer is not evidence that this group forwarded one"
	// modelDeploymentQualifyUnfinishedAnswer is an answer that stopped without saying why.
	modelDeploymentQualifyUnfinishedAnswer = "answered, but it recorded no finish reason, so the " +
		"answer is not evidence that this group forwarded one"
	// modelDeploymentQualifyAbortedAnswer is an answer whose request did not run to completion.
	modelDeploymentQualifyAbortedAnswer = "answered, but its finish reason reports the request did " +
		"not complete, so the answer is not evidence that this group forwarded one"
)

// modelDeploymentQualifyNonceBytes is how much randomness one probe identifier carries. It is
// enough that two attempts never collide, and small enough that the identifier stays readable in
// an engine's own log line.
const modelDeploymentQualifyNonceBytes = 12

// modelDeploymentQualifyRequestID builds the identifier one probe request carries.
//
// IT IS FRESH PER ATTEMPT, so a body carried over from an earlier attempt cannot satisfy the
// binding: the prefix and the member's own identifier say which probe this is and which member it
// was sent to, and the nonce says which attempt it belonged to.
func modelDeploymentQualifyRequestID(member *core.Pod) (string, error) {
	nonce, err := modelDeploymentQualifyRandom(modelDeploymentQualifyNonceBytes)
	if err != nil {
		return "", err
	}

	return modelDeploymentQualifyRIDPrefix + string(member.UID) + "-" + nonce, nil
}

// modelDeploymentQualifyRandom returns a hexadecimal token read from the cryptographic source.
//
// IT IS READ FROM THERE AND NOT FROM A COUNTER, because the identifier leaves this operator in the
// engine's logs and in its request counters, and a predictable one would let a reader of those
// logs write an answer that binds.
func modelDeploymentQualifyRandom(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("the probe's random source failed: %w", err)
	}

	return hex.EncodeToString(buffer), nil
}

// modelDeploymentQualifyFinishRan reports whether a finish reason says the request ran to
// completion.
//
// A REASON THAT REPORTS THE REQUEST WAS ABORTED, OR THAT IT FAILED, IS NOT EVIDENCE THAT THIS
// GROUP FORWARDED ONE, so it is refused rather than accepted as any reason at all. A reason that
// says the answer was cut short is evidence: the engine ran, and it says where it stopped.
func modelDeploymentQualifyFinishRan(reason string) bool {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" || trimmed == "null" {
		return false
	}

	switch strings.ToLower(trimmed) {
	case "abort", "aborted", "error", "failed":
		return false
	}

	return true
}

// modelDeploymentQualifyFinishType reads the type out of a finish reason.
//
// THE ENGINE RECORDS THE REASON AS A DOCUMENT KEYED BY TYPE on the route this operator reads for a
// group, so the type is what has to be judged. A reason written as a plain string is judged as
// itself, because a reason whose shape this operator cannot read is still a reason.
func modelDeploymentQualifyFinishType(reason json.RawMessage) string {
	trimmed := strings.TrimSpace(string(reason))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var typed struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(reason, &typed) == nil && typed.Type != "" {
		return typed.Type
	}

	return trimmed
}

// observeModelDeploymentGroupForward classifies the group-forward evidence for one replica.
//
// The order of the questions below is the order of how much they cost to be wrong. A single-member
// replica is answered before anything else because it is the shape that must not move. A shape this
// operator has no observation for is answered before any network call, because an observation that
// was never read is not an observation. Only what survives both is worth a request.
func observeModelDeploymentGroupForward(
	ctx context.Context, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	view modelDeploymentReplicaView, fetch modelDeploymentGroupForwardFetch,
) modelDeploymentGroupForward {
	engineName := md.Spec.Engine.Name
	engine, known := modelDeploymentGroupForwardEngines[engineName]

	// A PREFILL OR DECODE GROUP IS ANSWERED BEFORE THE MEMBER COUNT, and it is never NotApplicable.
	// Its serving path is a pair, so a group of one member still has a transfer to prove, and
	// "there is nothing to verify" would be a claim nobody made an observation for. The directed
	// pair probe is the path that answers it, and it needs both groups' snapshots; this call site
	// carries one of them, so the group is held with the reason that names the probe that would.
	if workercore.ModelDeploymentRoleKindPrefill == ModelDeploymentEffectiveRoleKind(role) ||
		workercore.ModelDeploymentRoleKindDecode == ModelDeploymentEffectiveRoleKind(role) {
		if !known {
			return modelDeploymentGroupForwardUnsupportedFor(
				"engine " + engineName + " has no group-forward observation this operator has verified",
			)
		}
		return modelDeploymentGroupForwardUnsupportedFor(engine.DisaggregatedReason)
	}

	if len(view.Members) <= 1 {
		return modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardNotApplicable,
			Reason: "a replica of one member has no cross-member forward path to verify",
		}
	}

	// AN ENCODER IS ANSWERED BEFORE THE ENGINE, AND IT IS ANSWERED RATHER THAN UNSUPPORTED. It runs
	// the multimodal encoder and produces encoder-cache output, so it serves no tokens: there is no
	// completion request whose bound answer would prove anything about its forward path, and this
	// leg has no question to ask of it at any engine or version. Asking anyway would spend a request
	// against an engine that cannot answer it, and the only answer obtainable would be Unknown,
	// which the health predicate reads as a hold on a healthy group. NotApplicable says the
	// observation does not exist for the role, which is a different fact from the engine not
	// offering one, and only the former keeps an encoder out of the hold.
	if workercore.ModelDeploymentRoleKindEncode == ModelDeploymentEffectiveRoleKind(role) {
		return modelDeploymentGroupForward{
			State: modelDeploymentGroupForwardNotApplicable,
			Reason: "a replica of the Encode role serves no tokens, so its forward path has no " +
				"cross-member request a bound answer could verify",
		}
	}

	if !known {
		return modelDeploymentGroupForwardUnsupportedFor(
			"engine " + engineName + " has no group-forward observation this operator has verified",
		)
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
		rid, err := modelDeploymentQualifyRequestID(member)
		if err != nil {
			return modelDeploymentGroupForwardUnknownFor(
				"the probe's identifier for " + member.Name + " could not be generated: " + err.Error())
		}
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
		if refusal := engine.Bound(response, rid); refusal != "" {
			return modelDeploymentGroupForwardUnknownFor("member " + member.Name + " " + refusal)
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
//
// THE ANNOTATION NAMES THE ENGINE, AND NOT ALWAYS THE FRONT DOOR. A decoder the routing proxy
// fronts answers on two ports: the one its Service targets, which the proxy opens, and the engine's
// own port behind it. The annotation carries the engine's port, so a caller that wants the front
// door names its own port with modelDeploymentGroupForwardURLOnPort.
func modelDeploymentGroupForwardURL(member *core.Pod, path string) (string, error) {
	if member.Status.PodIP == "" {
		return "", fmt.Errorf("has no address to be asked at yet")
	}

	port := member.Annotations["prometheus.io/port"]
	if port == "" {
		return "", fmt.Errorf("does not carry the engine's serving port")
	}
	number, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return "", fmt.Errorf("does not carry a serving port this operator can read")
	}

	return modelDeploymentGroupForwardURLOnPort(member, int32(number), path)
}

// modelDeploymentGroupForwardURLOnPort builds the URL one member is asked at, on a port the caller
// names.
//
// THE PORT IS THE CALLER'S TO NAME, because a member behind a routing proxy answers on two: the one
// its Service targets, which the proxy opens, and the engine's own port behind it. A caller that
// wants the front door names the Service's port here; a caller that wants the engine names the
// engine's own.
func modelDeploymentGroupForwardURLOnPort(member *core.Pod, port int32, path string) (string, error) {
	if member.Status.PodIP == "" {
		return "", fmt.Errorf("has no address to be asked at yet")
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("does not carry a serving port this operator can ask at")
	}
	scheme := member.Annotations["prometheus.io/scheme"]
	if scheme == "" {
		scheme = "http"
	}

	return fmt.Sprintf("%s://%s:%d%s", scheme, member.Status.PodIP, port, path), nil
}

// modelDeploymentGroupForwardHostPort is a member's own serving address in the form a handshake
// names an endpoint in.
//
// IT IS THE ENGINE'S OWN ADDRESS, read off the same annotations, so the endpoint this operator names
// to another engine and the endpoint it would ask are the same one. It is the right address to name
// for a prefiller, which no proxy fronts, and the wrong one to ask for a decoder a proxy fronts.
func modelDeploymentGroupForwardHostPort(member *core.Pod) (string, error) {
	if member.Status.PodIP == "" {
		return "", fmt.Errorf("has no address to be named yet")
	}
	port := member.Annotations["prometheus.io/port"]
	if port == "" {
		return "", fmt.Errorf("does not carry the engine's serving port")
	}

	return member.Status.PodIP + ":" + port, nil
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

// defaultGroupForwardPairFetch performs one request of a directed pair.
//
// IT IS A SEPARATE TRANSPORT FROM THE ORDINARY ONE because a pair's request carries headers the
// ordinary one never needs: one engine reads the rank it means from a header, and another is told
// which prefiller to pair with through one.
func defaultGroupForwardPairFetch(
	ctx context.Context, request modelDeploymentGroupForwardPairRequest,
) ([]byte, error) {
	method := request.Method
	if method == "" {
		method = http.MethodPost
	}
	probeRequest, err := http.NewRequestWithContext(
		ctx, method, request.URL, strings.NewReader(string(request.Body)))
	if err != nil {
		return nil, fmt.Errorf("pair request could not be built: %w", err)
	}
	probeRequest.Header.Set("Content-Type", "application/json")
	for name, value := range request.Headers {
		probeRequest.Header.Set(name, value)
	}

	response, err := http.DefaultClient.Do(probeRequest)
	if err != nil {
		return nil, fmt.Errorf("pair request failed: %w", err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(response.Body, modelDeploymentGroupForwardMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("pair response could not be read: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine answered %s", response.Status)
	}

	return payload, nil
}

// modelDeploymentGroupForwardPairBound is how long one directed pair probe may take in all. It is
// ONE budget shared by every rank pair the attempt probes, the way the ordinary probe shares one
// budget across a replica's members: a rank pair that answers slowly consumes the budget the
// later rank pairs would have used.
const modelDeploymentGroupForwardPairBound = 60 * time.Second

// modelDeploymentGroupForwardPairMaxTokens is how many tokens the copied answer may take. The
// answer is one short line, and the cap is what keeps a probe from turning into a generation.
const modelDeploymentGroupForwardPairMaxTokens = 80

// modelDeploymentGroupForwardPairPrefillerHeader names the header the decoder's routing proxy
// reads the prefiller from.
//
// IT IS THE ROUTER'S OWN HEADER, the one its picker writes when it chooses a prefiller, and the
// proxy reads the same one. The operator writes it to name the prefiller it means, which is what
// turns a proxy that picks for itself into a pair this operator chose.
const modelDeploymentGroupForwardPairPrefillerHeader = "x-prefiller-host-port"

// modelDeploymentGroupForwardPairRankHeader names the header a data-parallel rank is read from.
const modelDeploymentGroupForwardPairRankHeader = "X-Data-Parallel-Rank"

// modelDeploymentGroupForwardPairBootstrapPortAnnotation publishes the prefiller's bootstrap port
// for whoever has to fetch its transfer endpoints from. The value is the port as a decimal string.
const modelDeploymentGroupForwardPairBootstrapPortAnnotation = "sglang.ai/bootstrap-port"

// modelDeploymentGroupForwardPairShape identifies the path used by a directed pair probe.
//
// IT IS A VALUE AND NOT A BOOLEAN, because the two shapes answer different questions. The
// head-to-head shape drives both heads itself and enumerates every rank pair of the two groups, and
// the proxy shape asks the decode group's routing proxy one question and lets the proxy pick the
// rank inside the engine it fronts. A reader that cannot tell them apart would read a proxy-shape
// Verified as a rank-covering proof, which it is not.
type modelDeploymentGroupForwardPairShape string

const (
	// modelDeploymentGroupForwardPairShapeHeadToHead is the path where this operator drives
	// both heads and every rank pair of the two groups is asked.
	modelDeploymentGroupForwardPairShapeHeadToHead modelDeploymentGroupForwardPairShape = "HeadToHead"

	// modelDeploymentGroupForwardPairShapeViaRoutingProxy is the path where the decode group's
	// routing proxy drives the pair and picks the rank inside the engine it fronts.
	modelDeploymentGroupForwardPairShapeViaRoutingProxy modelDeploymentGroupForwardPairShape = "ViaRoutingProxy"
)

// modelDeploymentGroupForwardPairAttempt is the typed record of one directed pair probe: the two
// group generations it bound, the identifiers it sent, and the rooms it joined the legs on.
//
// BOTH GENERATIONS ARE HERE AND NOT ONLY THE ONE THE RESULT CARRIES, because a probe speaks for
// two groups at once and a reader has to be able to tell which two.
type modelDeploymentGroupForwardPairAttempt struct {
	// PrefillGeneration is the membership generation of the prefill group this attempt probed.
	PrefillGeneration modelDeploymentMembershipGeneration
	// DecodeGeneration is the membership generation of the decode group this attempt probed.
	DecodeGeneration modelDeploymentMembershipGeneration
	// Shape is the shape the attempt took, so a reader can tell a proxy-shape answer from a
	// rank-covering one without reading its reason.
	Shape modelDeploymentGroupForwardPairShape
	// RequestIDs are the identifiers the attempt sent, one per pair it probed.
	RequestIDs []string
	// BootstrapRooms are the rooms the attempt joined the legs on, one per pair it probed. The
	// proxy path records none, because the proxy picks the room there.
	BootstrapRooms []int64
	// Mooncake carries the mooncake exposition lines both halves answered with, one entry per
	// series, prefixed by the half. The prefill half's series are push evidence; the connector
	// exports no counter for a successful read, so the copied answer stays the read-side proof.
	Mooncake []string
}

// modelDeploymentGroupForwardPairResult is the outcome of one directed pair probe: one result per
// role, and the attempt that produced them.
type modelDeploymentGroupForwardPairResult struct {
	// Prefill is the prefill group's result.
	Prefill modelDeploymentGroupForward
	// Decode is the decode group's result.
	Decode modelDeploymentGroupForward
	// Attempt is the typed record of what the probe asked and of what it sent.
	Attempt modelDeploymentGroupForwardPairAttempt
}

// modelDeploymentGroupForwardPairRequest is one request of a directed pair: where it goes, what it
// carries, and what it says about the rank it means.
//
// IT IS A SEPARATE SHAPE FROM THE ORDINARY PROBE'S REQUEST because a pair's request carries headers
// the ordinary one never needs: one engine reads the rank it means from a header, and another is
// told which prefiller to pair with through one.
type modelDeploymentGroupForwardPairRequest struct {
	// Method is the HTTP method. An empty method is POST, which is what every route this operator
	// probes reads a request on.
	Method string
	// URL is where the request goes.
	URL string
	// Body is the request document. A request that reads no document carries none.
	Body []byte
	// Headers are the request headers. A request that needs none carries none.
	Headers map[string]string
}

// modelDeploymentGroupForwardPairFetch performs one request of a directed pair and returns the
// response body.
//
// IT IS A FUNCTION RATHER THAN A DIAL BECAUSE THE STATES THIS FILE HAS TO GET RIGHT ARE FAILURES,
// and a real dial cannot be made to fail on demand. A nil fetch means the pair has no transport
// and therefore makes no observation, which reads Unknown and holds.
type modelDeploymentGroupForwardPairFetch func(
	ctx context.Context, request modelDeploymentGroupForwardPairRequest,
) ([]byte, error)

// modelDeploymentGroupForwardPairLeg is one request of a directed pair: where it goes, what it
// carries, and what its answer must show.
type modelDeploymentGroupForwardPairLeg struct {
	// Name is what the leg is called in a reason.
	Name string
	// Request is the request this leg carries.
	Request modelDeploymentGroupForwardPairRequest
	// Copied says the leg's answer must carry the copied text. A leg that does not is checked for
	// its binding alone: the prefiller hands over the KV and answers with a partial generation,
	// which says nothing about whether the transfer happened.
	Copied bool
}

// modelDeploymentGroupForwardPairReply is what one leg's answer says.
type modelDeploymentGroupForwardPairReply struct {
	// Bound says the answer carries this operator's own request identifier.
	Bound bool
	// Reason is the finish reason the engine recorded.
	Reason string
	// Content is the text the answer produced.
	Content string
}

// modelDeploymentGroupForwardPairInput is what one pair's requests are built from.
type modelDeploymentGroupForwardPairInput struct {
	// Model is the model the engine serves.
	Model string
	// RequestID is the identifier the legs of this pair carry.
	RequestID string
	// Expected is the text the decoder must copy back.
	Expected string
	// RequestIDField is the body field the engine reads the request identifier from.
	RequestIDField string
	// ChatFields are the fields beside the identifier and the messages that the engine's chat
	// route reads.
	ChatFields map[string]any
	// PrefillURL and DecodeURL are where the two heads are asked. The decode URL is the front door
	// of a decoder the routing proxy fronts, and the engine's own address otherwise.
	PrefillURL string
	DecodeURL  string
	// BootstrapHost is the prefiller's own address, which the decode side is told to fetch the
	// transfer endpoints from.
	BootstrapHost string
	// BootstrapPort is the port the prefiller's transfer endpoints are served on.
	BootstrapPort int
	// BootstrapAddress is the prefiller's address in the form the engine's handshake names it in.
	BootstrapAddress string
	// BootstrapRoom is the room the two legs are joined on.
	BootstrapRoom int64
	// PrefillRank and DecodeRank are the data-parallel ranks this pair addresses.
	PrefillRank int
	DecodeRank  int
	// RemoteEngineID is the prefiller's engine id for this rank, where the engine's handshake
	// names one.
	RemoteEngineID string
	// TransferID is the transfer's own identifier, where the engine's handshake carries one.
	TransferID string
}

// modelDeploymentGroupForwardPairEngine is everything one engine contributes to a directed pair
// probe: the route its requests go to, the fields its chat route reads, and the requests the two
// heads of one rank pair carry.
//
// THE ENGINE'S DIFFERENCES LIVE HERE AND NOT IN THE PROBE, because the probe is the part that has
// to be right about failure, and a per-engine flow would be a second place where that is decided.
type modelDeploymentGroupForwardPairEngine struct {
	// Path is the route the requests go to.
	Path string
	// RequestIDField is the body field the engine reads the request identifier from.
	RequestIDField string
	// IDPrefix is what the engine puts in front of the request identifier in its answer's id.
	IDPrefix string
	// ChatFields are the fields beside the identifier and the messages that the engine's chat
	// route reads.
	ChatFields map[string]any
	// Heads renders the requests the two heads of one rank pair carry.
	Heads func(in modelDeploymentGroupForwardPairInput) ([]modelDeploymentGroupForwardPairLeg, error)
}

// modelDeploymentGroupForwardPairEngines is the per-engine matrix of the directed pair probe. An
// engine absent from it has no verified observation, and the probe names it rather than guessing
// at a reason.
var modelDeploymentGroupForwardPairEngines = map[string]modelDeploymentGroupForwardPairEngine{
	workercore.ModelDeploymentEngineSGLang: {
		// The chat route is the one that takes a request identifier and answers with it, and it is
		// the route the measured pair ran on.
		Path:           "/v1/chat/completions",
		RequestIDField: "rid",
		// The engine puts nothing in front of the identifier in its answer's id.
		IDPrefix: "",
		// The thinking switch is what keeps a model that reasons out loud from answering with the
		// reasoning, which the copy would then not match.
		ChatFields: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
		Heads: func(in modelDeploymentGroupForwardPairInput) ([]modelDeploymentGroupForwardPairLeg, error) {
			prefill, err := modelDeploymentGroupForwardPairSGLangBody(in, in.PrefillRank)
			if err != nil {
				return nil, err
			}
			decode, err := modelDeploymentGroupForwardPairSGLangBody(in, in.DecodeRank)
			if err != nil {
				return nil, err
			}

			return []modelDeploymentGroupForwardPairLeg{
				{
					Name:    "prefill",
					Request: modelDeploymentGroupForwardPairRequest{URL: in.PrefillURL, Body: prefill},
				},
				{
					Name: "decode",
					Request: modelDeploymentGroupForwardPairRequest{
						URL: in.DecodeURL, Body: decode,
					},
					Copied: true,
				},
			}, nil
		},
	},
	workercore.ModelDeploymentEngineVLLM: {
		Path:           "/v1/chat/completions",
		RequestIDField: "request_id",
		// The engine prefixes the identifier with the kind of completion it answered.
		IDPrefix: "chatcmpl-",
		Heads: func(in modelDeploymentGroupForwardPairInput) ([]modelDeploymentGroupForwardPairLeg, error) {
			// THE ENGINE ID OF THE PREFILLER'S RANK IS WHAT THE HANDSHAKE NAMES THE PREFILLER BY,
			// and this operator does not read the prefiller's own registry to learn it. A pair that
			// needs it and cannot have it holds closed rather than guess an identifier.
			if in.RemoteEngineID == "" {
				return nil, fmt.Errorf(
					"the probe cannot discover the prefiller engine id for rank %d, and the "+
						"connector's handshake will not name the prefiller without it; the id is held "+
						"in the prefiller's own transfer registry", in.PrefillRank)
			}
			prefill, err := modelDeploymentGroupForwardPairVLLMBody(in, map[string]any{
				"do_remote_decode":  true,
				"do_remote_prefill": false,
				"transfer_id":       in.TransferID,
			})
			if err != nil {
				return nil, err
			}
			decode, err := modelDeploymentGroupForwardPairVLLMBody(in, map[string]any{
				"do_remote_decode":      false,
				"do_remote_prefill":     true,
				"remote_bootstrap_addr": in.BootstrapAddress,
				"remote_engine_id":      in.RemoteEngineID,
				"transfer_id":           in.TransferID,
			})
			if err != nil {
				return nil, err
			}

			return []modelDeploymentGroupForwardPairLeg{
				{
					Name: "prefill",
					Request: modelDeploymentGroupForwardPairRequest{
						URL:     in.PrefillURL,
						Body:    prefill,
						Headers: map[string]string{modelDeploymentGroupForwardPairRankHeader: strconv.Itoa(in.PrefillRank)},
					},
				},
				{
					Name: "decode",
					Request: modelDeploymentGroupForwardPairRequest{
						URL: in.DecodeURL, Body: decode,
					},
					Copied: true,
				},
			}, nil
		},
	},
}

// observeModelDeploymentGroupForwardPair probes a prefill group and a decode group as one
// directed pair, and answers for both roles.
//
// THE TWO GROUPS ARE PROBED AS ONE PAIR because neither of them serves on its own: the prefiller
// computes the KV and the decoder consumes it, so a request that comes back with the copied answer
// is evidence about both, and a request that does not come back is evidence about neither.
//
// EVERY FAILURE IS UNKNOWN FOR BOTH ROLES, never Failed. A pair that could not be observed says
// nothing about which side is at fault, and blaming a healthy peer for a pair that could not be
// read would withdraw a group that serves.
func observeModelDeploymentGroupForwardPair(
	ctx context.Context, md *workercore.ModelDeployment,
	prefill, decode modelDeploymentReplicaView,
	fetch modelDeploymentGroupForwardPairFetch,
) modelDeploymentGroupForwardPairResult {
	// THE SHAPE FOLLOWS THE DEPLOYMENT AND NOT THE ENGINE: a decode role that carries the routing
	// proxy is reached through that proxy, and the proxy is what drives the transfer leg, so the
	// operator names the prefiller to it and asks one question. A decode role with no proxy is
	// reached directly, and then the operator drives both heads itself.
	decodeRole := modelDeploymentGroupForwardPairDecodeRole(md)
	shape := modelDeploymentGroupForwardPairShapeHeadToHead
	if decodeRole != nil && ModelDeploymentRoleFrontedByProxy(md, decodeRole) {
		shape = modelDeploymentGroupForwardPairShapeViaRoutingProxy
	}
	viaProxy := shape == modelDeploymentGroupForwardPairShapeViaRoutingProxy

	attempt := modelDeploymentGroupForwardPairAttempt{
		PrefillGeneration: modelDeploymentGenerationOf(prefill),
		DecodeGeneration:  modelDeploymentGenerationOf(decode),
		Shape:             shape,
	}

	engineName := md.Spec.Engine.Name
	engine, known := modelDeploymentGroupForwardPairEngines[engineName]
	if !known {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForwardUnsupportedFor(
			"engine "+engineName+" has no group-forward observation this operator has verified",
		))
	}

	if fetch == nil {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardUnknown,
			Reason: "no group-forward transport is wired, so neither side of the pair could be asked",
		})
	}

	prefillHead, reason := modelDeploymentGroupForwardPairHead(prefill)
	if reason != "" {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardUnknown,
			Reason: "the prefill group has no address to answer for it: " + reason,
		})
	}
	decodeHead, reason := modelDeploymentGroupForwardPairHead(decode)
	if reason != "" {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardUnknown,
			Reason: "the decode group has no address to answer for it: " + reason,
		})
	}

	prefillURL, err := modelDeploymentGroupForwardURL(prefillHead, engine.Path)
	if err != nil {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardUnknown,
			Reason: "the prefill group has no address to be asked at: " + err.Error(),
		})
	}
	decodeURL, err := modelDeploymentGroupForwardPairDecodeURL(
		md, decodeRole, decodeHead, engine.Path, viaProxy)
	if err != nil {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardUnknown,
			Reason: "the decode group has no address to be asked at: " + err.Error(),
		})
	}

	// THE RANKS COME FROM THE ROLES' OWN BOOKS, which are the only place the engine's data-parallel
	// width is declared. A width nobody declared is the engine's own default of one.
	pair, err := modelDeploymentDeclaredParallelismPair(md)
	if err != nil {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State:  modelDeploymentGroupForwardUnknown,
			Reason: "the roles' declared parallelism could not be read: " + err.Error(),
		})
	}
	prefillWidth := pair.Prefill.DataParallel
	if prefillWidth < 1 {
		prefillWidth = 1
	}
	decodeWidth := pair.Decode.DataParallel
	if decodeWidth < 1 {
		decodeWidth = 1
	}
	if reason := modelDeploymentGroupForwardPairWidthBound("prefill", prefillWidth, shape); reason != "" {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State: modelDeploymentGroupForwardUnknown, Reason: reason,
		})
	}
	if reason := modelDeploymentGroupForwardPairWidthBound("decode", decodeWidth, shape); reason != "" {
		return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
			State: modelDeploymentGroupForwardUnknown, Reason: reason,
		})
	}

	probeCtx, cancel := context.WithTimeout(ctx, modelDeploymentGroupForwardPairBound)
	defer cancel()

	for prefillRank := 0; prefillRank < prefillWidth; prefillRank++ {
		for decodeRank := 0; decodeRank < decodeWidth; decodeRank++ {
			if viaProxy && (prefillRank > 0 || decodeRank > 0) {
				// The proxy picks the rank inside the engine it fronts, so there is one pair to
				// ask about rather than one per rank. The declared widths do not change that:
				// the shape is one question whatever they are.
				continue
			}
			reply, room, reason := modelDeploymentGroupForwardPairProbe(
				probeCtx, md, engine, prefillHead, prefillURL, decodeURL,
				prefillWidth, prefillRank, decodeRank, viaProxy, fetch,
			)
			if reason != "" {
				return modelDeploymentGroupForwardPairHeld(attempt, modelDeploymentGroupForward{
					State: modelDeploymentGroupForwardUnknown, Reason: reason,
				})
			}
			attempt.RequestIDs = append(attempt.RequestIDs, reply)
			if !viaProxy {
				// THE ROOM IS RECORDED FOR EVERY HEAD-TO-HEAD PAIR, and a room of zero is a
				// room like any other. The proxy shape records none, because the proxy picks
				// the room it joins the pair on and this operator never builds one there.
				attempt.BootstrapRooms = append(attempt.BootstrapRooms, room)
			}
			// THE MOONCAKE SERIES ARE READ FROM BOTH HALVES while the pair is fresh: the prefill
			// half's exposition carries the push evidence, and the decode half's carries whatever
			// its connector did about it.
			attempt.Mooncake = append(attempt.Mooncake,
				modelDeploymentGroupForwardPairMooncakeSeries(probeCtx, prefillURL, engine.Path, "prefill", fetch)...)
			attempt.Mooncake = append(attempt.Mooncake,
				modelDeploymentGroupForwardPairMooncakeSeries(probeCtx, decodeURL, engine.Path, "decode", fetch)...)
		}
	}

	return modelDeploymentGroupForwardPairVerified(attempt, len(attempt.RequestIDs))
}

// modelDeploymentGroupForwardPairProbe asks one pair's legs and reads their answers.
func modelDeploymentGroupForwardPairProbe(
	ctx context.Context, md *workercore.ModelDeployment, engine modelDeploymentGroupForwardPairEngine,
	prefillHead *core.Pod, prefillURL, decodeURL string,
	prefillWidth, prefillRank, decodeRank int, viaProxy bool,
	fetch modelDeploymentGroupForwardPairFetch,
) (string, int64, string) {
	nonce, err := modelDeploymentQualifyRandom(modelDeploymentQualifyNonceBytes)
	if err != nil {
		return "", 0, "the pair's identifier could not be generated: " + err.Error()
	}
	requestID := modelDeploymentQualifyRIDPrefix + "pd-" + nonce

	expected := modelDeploymentGroupForwardPairExpected(prefillRank, decodeRank)

	input := modelDeploymentGroupForwardPairInput{
		Model:          md.Spec.Model.Name,
		RequestID:      requestID,
		Expected:       expected,
		RequestIDField: engine.RequestIDField,
		ChatFields:     engine.ChatFields,
		PrefillURL:     prefillURL,
		DecodeURL:      decodeURL,
		PrefillRank:    prefillRank,
		DecodeRank:     decodeRank,
	}

	var legs []modelDeploymentGroupForwardPairLeg
	if viaProxy {
		leg, err := modelDeploymentGroupForwardPairProxyLeg(input, prefillHead)
		if err != nil {
			return "", 0, err.Error()
		}
		legs = []modelDeploymentGroupForwardPairLeg{leg}
	} else {
		input.BootstrapHost = prefillHead.Status.PodIP
		port, err := modelDeploymentGroupForwardPairBootstrapPort(prefillHead)
		if err != nil {
			return "", 0, err.Error()
		}
		input.BootstrapPort = port
		input.BootstrapAddress = prefillHead.Status.PodIP + ":" + strconv.Itoa(port)
		input.BootstrapRoom, err = modelDeploymentGroupForwardPairRoom(prefillWidth, prefillRank)
		if err != nil {
			return "", 0, err.Error()
		}
		input.TransferID = "xfer-" + requestID
		legs, err = engine.Heads(input)
		if err != nil {
			return "", 0, "the pair's requests could not be rendered: " + err.Error()
		}
	}

	replies, failures := modelDeploymentGroupForwardPairSend(ctx, legs, fetch, requestID, engine.IDPrefix)
	for i := range legs {
		if failures[i] != nil {
			return "", 0, "the " + legs[i].Name + " leg did not answer within the bound: " +
				failures[i].Error() + "; a pair that could not be observed is an absence of " +
				"evidence, and it is not attributed to one side"
		}
		reply := replies[i]
		if !reply.Bound {
			return "", 0, "the " + legs[i].Name + " leg answered, but not bound to this operator's own " +
				"request, so the answer is not evidence that the pair carried one"
		}
		if !legs[i].Copied {
			if !modelDeploymentQualifyFinishRan(reply.Reason) {
				return "", 0, "the " + legs[i].Name + " leg answered, but its finish reason reports " +
					"the request did not complete, so the answer is not evidence that the pair " +
					"carried one"
			}
			continue
		}
		if reply.Reason != "stop" {
			return "", 0, "the " + legs[i].Name + " leg answered, but it did not finish normally, so " +
				"the answer is not evidence that the pair carried one"
		}
		// THE COMPARE READS THE ANSWER'S VISIBLE TEXT. The chat template wraps its answer in a
		// <think>…</think> block on this route whatever the thinking switch says -- case-9 run2's
		// enriched reason caught the byte-perfect copy sitting behind an EMPTY wrapper -- and the
		// wrapper is the template's noise, not the model's answer. The block is stripped before the
		// compare; the rejected answer stays RAW in the reason, where the wrapper is still visible
		// for the next classification.
		if visible := modelDeploymentGroupForwardPairVisibleCopy(reply.Content); visible != expected {
			// THE ANSWER IS NAMED IN THE REASON, bounded and unstripped, so the run's evidence can
			// tell a model that drifted from a context that was never whole. A reason that discarded
			// the answer left the classification — answer drift, a broken read, a thinking leak — to
			// a rerun this probe exists to avoid.
			return "", 0, "the " + legs[i].Name + " leg answered, but it did not carry the text the " +
				"request asked to be copied, so nothing shows the prefill's KV reached it (answer: \"" +
				modelDeploymentGroupForwardPairBounded(reply.Content, modelDeploymentGroupForwardPairAnswerBound) + "\")"
		}
	}

	return requestID, input.BootstrapRoom, ""
}

// modelDeploymentGroupForwardPairThinkWrapper matches a leading <think>…</think> block, across
// lines, at the head of an answer.
var modelDeploymentGroupForwardPairThinkWrapper = regexp.MustCompile(`(?s)^<think>.*?</think>`)

// modelDeploymentGroupForwardPairVisibleCopy strips a leading <think>…</think> block from an
// answer and trims what remains, leaving the copy the decode leg was asked for.
//
// THE WRAPPER IS TEMPLATE NOISE, NOT CONTENT. The Qwen3 chat template emits the block on this
// route whatever the thinking switch says -- case-9 run2's enriched reason caught the byte-perfect
// copy sitting behind an EMPTY wrapper -- so the exact compare reads the answer with the block
// removed. Rejected answers stay raw in their reasons, where the wrapper is still visible.
func modelDeploymentGroupForwardPairVisibleCopy(answer string) string {
	return strings.TrimSpace(modelDeploymentGroupForwardPairThinkWrapper.ReplaceAllString(answer, ""))
}

// modelDeploymentGroupForwardPairAnswerBound is how much of a rejected answer the failure reason
// carries. Two hundred characters is enough to tell a drifted copy from an unrelated answer, and
// small enough that a reason stays a line.
const modelDeploymentGroupForwardPairAnswerBound = 200

// modelDeploymentGroupForwardPairBounded cuts a text to at most limit characters on rune
// boundaries and marks the cut with an ellipsis. The engines answer in more than one script, and
// a bound that splits a character corrupts the very evidence it carries.
func modelDeploymentGroupForwardPairBounded(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// modelDeploymentGroupForwardPairMooncakeSeries reads one half's metrics exposition and reduces it
// to the mooncake series the observation records.
//
// THE PUSH IS NOT THE PULL. vllm:mooncake_bytes_transferred is the sender's histogram, and its
// _count is the number of transfers the prefill half completed; the connector exports no counter
// for a successful read — vllm:mooncake_num_failed_recvs counts failed pulls only — so the series
// are recorded as what they are, push evidence, and the copied answer stays the read-side proof.
func modelDeploymentGroupForwardPairMooncakeSeries(
	ctx context.Context, chatURL, enginePath, half string, fetch modelDeploymentGroupForwardPairFetch,
) []string {
	body, err := fetch(ctx, modelDeploymentGroupForwardPairRequest{
		Method: http.MethodGet,
		URL:    strings.TrimSuffix(chatURL, enginePath) + "/metrics",
	})
	if err != nil {
		return []string{half + ": the mooncake exposition did not answer: " + err.Error()}
	}
	series := make([]string, 0)
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "vllm:mooncake_") || strings.HasSuffix(trimmed, "_bucket") {
			continue
		}
		series = append(series, half+": "+trimmed)
	}
	if len(series) == 0 {
		return []string{half + ": the mooncake exposition carries no vllm:mooncake_ series"}
	}
	return series
}

// modelDeploymentGroupForwardPairSend asks every leg of a pair and reads the answers.
//
// THE LEGS GO OUT TOGETHER, because a pair is a pair only while both halves are in flight: the
// decode side waits for the prefiller, and a prefiller asked after its decoder gave up would be
// asked about a transfer that can no longer happen.
func modelDeploymentGroupForwardPairSend(
	ctx context.Context, legs []modelDeploymentGroupForwardPairLeg,
	fetch modelDeploymentGroupForwardPairFetch, requestID, idPrefix string,
) ([]modelDeploymentGroupForwardPairReply, []error) {
	replies := make([]modelDeploymentGroupForwardPairReply, len(legs))
	failures := make([]error, len(legs))

	var group sync.WaitGroup
	for i := range legs {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			body, err := fetch(ctx, legs[index].Request)
			if err != nil {
				failures[index] = err
				return
			}
			replies[index] = modelDeploymentGroupForwardPairChatRead(body, requestID, idPrefix)
		}(i)
	}
	group.Wait()

	return replies, failures
}

// modelDeploymentGroupForwardPairChatRead reads one leg's answer.
//
// THE BINDING IS THE ENGINE'S OWN: the answer's id has to carry the identifier this operator sent,
// with whatever the engine puts in front of it. An answer that carries an id is read as bound only
// when the whole id is the one this operator asked for.
func modelDeploymentGroupForwardPairChatRead(
	body []byte, requestID, idPrefix string,
) modelDeploymentGroupForwardPairReply {
	var response struct {
		ID      string `json:"id"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &response) != nil {
		return modelDeploymentGroupForwardPairReply{}
	}

	reply := modelDeploymentGroupForwardPairReply{Bound: response.ID == idPrefix+requestID}
	if len(response.Choices) == 0 || response.Choices[0].FinishReason == nil {
		return reply
	}
	reply.Reason = *response.Choices[0].FinishReason
	reply.Content = strings.TrimSpace(response.Choices[0].Message.Content)

	return reply
}

// modelDeploymentGroupForwardPairChatFields are the fields every leg of a directed pair carries
// besides the engine's own handshake: the model, the identifier, the messages and the shape of the
// answer.
func modelDeploymentGroupForwardPairChatFields(in modelDeploymentGroupForwardPairInput) map[string]any {
	fields := map[string]any{
		"model":       in.Model,
		"messages":    []map[string]string{{"role": "user", "content": modelDeploymentGroupForwardPairCopy(in.Expected)}},
		"max_tokens":  modelDeploymentGroupForwardPairMaxTokens,
		"temperature": 0,
		"stream":      false,
	}
	if in.RequestIDField != "" {
		fields[in.RequestIDField] = in.RequestID
	}
	for name, value := range in.ChatFields {
		fields[name] = value
	}

	return fields
}

// modelDeploymentGroupForwardPairSGLangBody renders the request one leg of a directed pair
// carries on the engine that names its rank in the body and its prefiller in the request.
func modelDeploymentGroupForwardPairSGLangBody(
	in modelDeploymentGroupForwardPairInput, rank int,
) ([]byte, error) {
	fields := modelDeploymentGroupForwardPairChatFields(in)
	fields["bootstrap_host"] = in.BootstrapHost
	fields["bootstrap_port"] = in.BootstrapPort
	fields["bootstrap_room"] = in.BootstrapRoom
	fields["routed_dp_rank"] = rank

	return json.Marshal(fields)
}

// modelDeploymentGroupForwardPairVLLMBody renders the request one leg of a directed pair carries
// on the engine whose handshake travels in the request's own transfer parameters.
func modelDeploymentGroupForwardPairVLLMBody(
	in modelDeploymentGroupForwardPairInput, transfer map[string]any,
) ([]byte, error) {
	fields := modelDeploymentGroupForwardPairChatFields(in)
	fields["kv_transfer_params"] = transfer

	return json.Marshal(fields)
}

// modelDeploymentGroupForwardPairProxyLeg renders the request a decoder's routing proxy carries.
//
// THE PROXY IS THE ONE THAT DRIVES THE TRANSFER LEG, so the operator does not build the
// handshake: it names the prefiller in the header the router's picker writes, and the proxy asks
// the prefiller's own registry for the transfer endpoints.
//
// THE LEG GOES TO THE PROXY'S OWN PORT, which is the front door the decode Service targets, and the
// prefiller is named by its own engine address, because no proxy fronts it.
//
// THE RANK COVERAGE IS NOT ASSERTED HERE, and this shape cannot show it. The routing proxy drives
// the pair, and the probe observes its response, this operator's own request identifier and the
// copied text. It does not observe the prefiller's response, because the proxy runs the prefill
// alongside the decode and streams back only the decode answer. It does not cover the individual
// data-parallel ranks either: the proxy picks the room it joins the pair on and the rank inside the
// engine it fronts, so this operator's room arithmetic steers nothing here. And with the prefiller
// header absent the proxy skips disaggregation and forwards decode-only, so a bound answer
// carrying the copied text is producible by the decode engine alone.
func modelDeploymentGroupForwardPairProxyLeg(
	in modelDeploymentGroupForwardPairInput, prefillHead *core.Pod,
) (modelDeploymentGroupForwardPairLeg, error) {
	address, err := modelDeploymentGroupForwardHostPort(prefillHead)
	if err != nil {
		return modelDeploymentGroupForwardPairLeg{}, fmt.Errorf(
			"the prefill group has no address to name to the decode side: %w", err)
	}
	body, err := json.Marshal(modelDeploymentGroupForwardPairChatFields(in))
	if err != nil {
		return modelDeploymentGroupForwardPairLeg{}, err
	}

	return modelDeploymentGroupForwardPairLeg{
		Name: "decode",
		Request: modelDeploymentGroupForwardPairRequest{
			URL:  in.DecodeURL,
			Body: body,
			Headers: map[string]string{
				modelDeploymentGroupForwardPairPrefillerHeader: address,
			},
		},
		Copied: true,
	}, nil
}

// modelDeploymentGroupForwardPairDecodeRole returns the deployment's decode role, which is the role
// the pair's decode leg is asked at.
//
// IT IS THE ROLE AND NOT A FLAG, because the port that role is reached on depends on what the
// operator rendered for it: a decoder the routing proxy fronts is asked at its front door, and a
// decoder with no proxy is asked at the engine's own address.
func modelDeploymentGroupForwardPairDecodeRole(
	md *workercore.ModelDeployment,
) *workercore.ModelDeploymentRole {
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if ModelDeploymentEffectiveRoleKind(role) == workercore.ModelDeploymentRoleKindDecode {
			return role
		}
	}

	return nil
}

// modelDeploymentGroupForwardPairDecodeURL builds the address the decode leg is asked at.
//
// A FRONTED DECODER IS ASKED AT ITS FRONT DOOR, which is the port its Service targets and the
// routing proxy opens. The annotation on such a member carries the engine's port behind that proxy,
// and a client that read it would ask a port nothing outside the proxy answers. A decoder with no
// proxy is asked at the engine's own address.
func modelDeploymentGroupForwardPairDecodeURL(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
	decodeHead *core.Pod, path string, viaProxy bool,
) (string, error) {
	if !viaProxy || role == nil {
		return modelDeploymentGroupForwardURL(decodeHead, path)
	}

	return modelDeploymentGroupForwardURLOnPort(
		decodeHead, ModelDeploymentRoleServingPort(md, role), path)
}

// modelDeploymentGroupForwardPairHead returns the member of a group that serves the engine's
// request route.
//
// IT IS THE LEADER BY CONSTRUCTION, because the other members of a disaggregated group run the
// engine's workers and serve no request route of their own: the leader routes a request to the
// rank the operator names, and every rank of the group is reached through that one address.
func modelDeploymentGroupForwardPairHead(view modelDeploymentReplicaView) (*core.Pod, string) {
	if len(view.Members) == 1 {
		return view.Members[0], ""
	}
	for _, member := range view.Members {
		if modelDeploymentAnsweringMemberLeader(member) {
			return member, ""
		}
	}

	return nil, "no member of the group carries the leader index"
}

// modelDeploymentGroupForwardPairBootstrapPort reads the port the prefiller's transfer endpoints
// are served on.
//
// IT IS READ OFF THE PREFILLER'S OWN ANNOTATION, which is the only place the port is published for
// whoever has to fetch those endpoints. A prefiller that publishes no port cannot be paired with,
// and saying so is better than dialing a number this operator guessed.
func modelDeploymentGroupForwardPairBootstrapPort(prefillHead *core.Pod) (int, error) {
	published := prefillHead.Annotations[modelDeploymentGroupForwardPairBootstrapPortAnnotation]
	if published == "" {
		return 0, fmt.Errorf(
			"the prefill head publishes no bootstrap port, so the decode side cannot be told where " +
				"to fetch the transfer endpoints from")
	}
	port, err := strconv.Atoi(published)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("the prefill head publishes a bootstrap port this operator cannot read: %q",
			published)
	}

	return port, nil
}

// modelDeploymentGroupForwardPairRoom allocates the bootstrap room one rank pair is joined on.
//
// THE ROOM'S LOW BITS CARRY THE PREFILL RANK: the room modulo the prefiller's data-parallel width
// is the rank the request is routed to, so a room built for one rank does not answer for another.
func modelDeploymentGroupForwardPairRoom(width, rank int) (int64, error) {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return 0, fmt.Errorf("the probe's random source failed: %w", err)
	}
	bits := binary.BigEndian.Uint64(buffer) >> 12

	return int64(bits*uint64(width) + uint64(rank)), nil
}

// modelDeploymentGroupForwardPairMaxWidth is the widest data-parallel group the pair probe
// addresses.
//
// IT IS THE ROOM ARITHMETIC'S OWN LIMIT and not a policy cap. The room is a random 52-bit value
// scaled by the width and offset by the rank, and the rank a room names is the room modulo the
// width, so the largest room is 2^52 * width - 1. That stays inside the positive range of an int64
// while the width is 2048 and leaves it at 2049. A width above the bound is therefore not probed:
// the room this operator would build for it no longer addresses the rank it names, and a request
// that reached the wrong rank would prove nothing about the pair.
const modelDeploymentGroupForwardPairMaxWidth = 2048

// modelDeploymentGroupForwardPairWidthBound is the reason a declared data-parallel width above the
// bound this operator probes is not probed, and an empty string when the width is inside it.
//
// THE SIDE IS NAMED, because the two groups declare their widths separately and a reader has to be
// able to tell which one this operator declined to probe.
//
// THE REASON FOLLOWS THE SHAPE, because the two shapes reach the bound differently. The head-to-head
// shape builds its own room, and a width above the bound is what would carry that room outside the
// range the rank arithmetic reads. The proxy shape never builds a room, because the proxy picks it,
// so there the bound is the probe's own supported limit and nothing more is claimed about it.
func modelDeploymentGroupForwardPairWidthBound(
	side string, width int, shape modelDeploymentGroupForwardPairShape,
) string {
	if width <= modelDeploymentGroupForwardPairMaxWidth {
		return ""
	}

	if shape == modelDeploymentGroupForwardPairShapeViaRoutingProxy {
		return "the " + side + " group declares a data-parallel width of " + strconv.Itoa(width) +
			", above this probe's supported limit of " +
			strconv.Itoa(modelDeploymentGroupForwardPairMaxWidth) +
			"; the limit applies to both pair shapes"
	}

	return "the " + side + " group declares a data-parallel width of " + strconv.Itoa(width) +
		", which is above the bound of " + strconv.Itoa(modelDeploymentGroupForwardPairMaxWidth) +
		" this operator probes; the bootstrap room is a random 52-bit value scaled by that width and " +
		"offset by the rank, so a width above the bound would carry the room outside the range the " +
		"rank arithmetic can address"
}

// modelDeploymentGroupForwardPairExpected is the text one pair's decoder must copy back.
//
// IT NAMES THE PAIR, because an answer carrying another pair's text is not this pair's answer,
// and the comparison is exact: anything else the decoder produced is not the copy.
func modelDeploymentGroupForwardPairExpected(prefillRank, decodeRank int) string {
	return fmt.Sprintf("探测P%dD%d：星河春风。", prefillRank, decodeRank)
}

// modelDeploymentGroupForwardPairCopy is the instruction the decoder's answer is checked against.
// It asks for the text inside the quotes and for nothing else, so the answer is a copy this
// operator can compare exactly.
func modelDeploymentGroupForwardPairCopy(expected string) string {
	return "逐字复制引号内文字，只输出文字本身：『" + expected + "』 /no_think"
}

// modelDeploymentGroupForwardPairHeld answers both roles with the same verdict, which is what a
// pair that could not be observed says: neither side is at fault, and neither side is proven.
func modelDeploymentGroupForwardPairHeld(
	attempt modelDeploymentGroupForwardPairAttempt, result modelDeploymentGroupForward,
) modelDeploymentGroupForwardPairResult {
	return modelDeploymentGroupForwardPairResult{
		Prefill: result,
		Decode:  result,
		Attempt: attempt,
	}
}

// modelDeploymentGroupForwardPairVerified answers both roles with the verdict a pair that carried
// the request earns, each with the generation of its own group.
//
// THE REASON FOLLOWS THE SHAPE, because the two shapes show different things. The head-to-head
// shape drove both heads and can read the transfer in the decode answer. The proxy shape asked the
// decode group's routing proxy one question, and that answer shows the proxy was alive, bound to
// this operator's own identifier, and returned the text the request asked to be copied. It does not
// show which rank pair served, because the proxy picked the rank, and it does not show the transfer
// itself, because the proxy drove that leg.
func modelDeploymentGroupForwardPairVerified(
	attempt modelDeploymentGroupForwardPairAttempt, pairs int,
) modelDeploymentGroupForwardPairResult {
	reason := "the prefill and decode legs both carried this operator's own request, and the " +
		"decode leg's answer shows the prefill's KV reached it"
	observation := fmt.Sprintf("%d rank pairs carried the request, each bound to its own identifier",
		pairs)
	if attempt.Shape == modelDeploymentGroupForwardPairShapeViaRoutingProxy {
		reason = "the decode group's routing proxy answered the probe while driving the pair with " +
			"the named prefiller, and its response matched this operator's own request identifier " +
			"and the exact text requested for copying"
		observation = "the routing proxy answered one request, bound to this operator's own identifier"
	}
	if len(attempt.Mooncake) > 0 {
		observation += "; the mooncake exposition: " + strings.Join(attempt.Mooncake, "; ")
	}
	// THE READ SIDE HAS NO SUCCESS COUNTER. The connector's exported series count the sender's
	// pushes and failed pulls only, so the observation says explicitly that the copied answer is
	// the evidence a pull happened; a reader cannot take the counters for it.
	observation += "; the mooncake connector exports no read-side success counter " +
		"(vllm:mooncake_num_failed_recvs counts failed pulls only), so the copied answer is the read-side evidence"

	return modelDeploymentGroupForwardPairResult{
		Prefill: modelDeploymentGroupForward{
			State:       modelDeploymentGroupForwardVerified,
			Reason:      reason,
			Generation:  attempt.PrefillGeneration,
			Observation: observation,
		},
		Decode: modelDeploymentGroupForward{
			State:       modelDeploymentGroupForwardVerified,
			Reason:      reason,
			Generation:  attempt.DecodeGeneration,
			Observation: observation,
		},
		Attempt: attempt,
	}
}
