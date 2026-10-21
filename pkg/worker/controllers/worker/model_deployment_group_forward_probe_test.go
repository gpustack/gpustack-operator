package worker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// probeMember is one replica member as the probe needs it: an address to ask, the serving port and
// scheme its own render stamped, and the UID the probe's identifier is built from.
func probeMember(ordinal, member int, uid string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      "server-r" + strconv.Itoa(ordinal) + "-m" + strconv.Itoa(member),
			Namespace: "team-a",
			UID:       types.UID(uid),
			Labels: map[string]string{
				modelDeploymentLabelKeyName:        modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance:    "qwen",
				modelDeploymentLabelKeyComponent:   "server",
				modelDeploymentReplicaOrdinalLabel: strconv.Itoa(ordinal),
				modelDeploymentMemberIndexLabel:    strconv.Itoa(member),
			},
			Annotations: map[string]string{
				"prometheus.io/port":   "8000",
				"prometheus.io/scheme": "http",
			},
		},
		Status: core.PodStatus{
			PodIP:      probeMemberIP(ordinal, member),
			Conditions: []core.PodCondition{{Type: core.PodReady, Status: core.ConditionTrue}},
		},
	}
}

// probeDeployment builds the deployment and role under test. The engine is the fact the per-engine
// guard reads, so it is what a case varies; the image is present because admission requires it and
// nothing here reads it.
func probeDeployment(engine string, kind workercore.ModelDeploymentRoleKind) (
	*workercore.ModelDeployment, *workercore.ModelDeploymentRole,
) {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: engine, Image: "engine:latest"},
		},
	}
	role := &workercore.ModelDeploymentRole{
		Name: "server", Replicas: 1, ReplicaSize: 2, InstanceType: "h20-8x", Kind: kind,
	}
	md.Spec.Roles = []workercore.ModelDeploymentRole{*role}

	return md, &md.Spec.Roles[0]
}

// probeFetch records every call and answers from a script, so a case can say what one member
// returns without a network and the test can say afterwards that the right members were asked.
type probeFetch struct {
	asked []string
	// answers maps a member address to the body that member returns. An address absent from it
	// gets the default body, which is the same document with the caller's own rid.
	answers map[string]string
	// failures maps a member address to the error that member answers with.
	failures map[string]error
}

func (f *probeFetch) fetch(_ context.Context, url string, body []byte) ([]byte, error) {
	f.asked = append(f.asked, url)
	// The member is identified by its host and port, which is what the script is filed under and
	// what a reader of a failure needs to see: the path is the same for every member.
	authority := url[strings.Index(url, "://")+3:]
	if slash := strings.Index(authority, "/"); slash >= 0 {
		authority = authority[:slash]
	}

	if err, failed := f.failures[authority]; failed {
		return nil, err
	}
	if answer, scripted := f.answers[authority]; scripted {
		return []byte(answer), nil
	}

	return boundProbeBody(body), nil
}

// boundProbeBody renders the answer the named engine would give to the request just sent, which is
// the only answer that counts as evidence.
func boundProbeBody(body []byte) []byte {
	var sent struct {
		RequestID string `json:"request_id"`
		RID       string `json:"rid"`
	}
	_ = json.Unmarshal(body, &sent)
	rid := []byte(sent.RequestID)
	if len(rid) == 0 {
		rid = []byte(sent.RID)
	}

	if strings.Contains(string(body), "sampling_params") {
		return []byte(`{"text":"ok","meta_info":{"id":"` + string(rid) +
			`","finish_reason":{"type":"length"}}}`)
	}

	return []byte(`{"id":"cmpl-` + string(rid) + `","choices":[{"text":"ok","finish_reason":"length"}]}`)
}

// probeMemberIP addresses a member by its ordinal AND its member index, because the members of one
// replica share an ordinal and a fixture that gave them one address would script both at once.
func probeMemberIP(ordinal, member int) string {
	return "10.0." + strconv.Itoa(ordinal) + "." + strconv.Itoa(1+member)
}

// probeHost is the key a scripted answer or failure is filed under, which is what the fetch sees
// once the scheme is stripped off the URL it is handed.
func probeHost(ordinal, member int) string {
	return probeMemberIP(ordinal, member) + ":8000"
}

// TestGroupForwardProbeAdmitsOnlyBoundAnswers is the per-engine verifiable matrix. One case per
// engine this operator reads, and the assertion is the whole contract: every member of the replica
// answered, each answer was bound to the request this operator sent it, and the leg is Verified
// rather than merely not-Unsupported.
func TestGroupForwardProbeAdmitsOnlyBoundAnswers(t *testing.T) {
	testCases := []struct {
		name   string
		engine string
	}{
		{name: "vLLM", engine: workercore.ModelDeploymentEngineVLLM},
		{name: "SGLang", engine: workercore.ModelDeploymentEngineSGLang},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md, role := probeDeployment(tc.engine, "")
			fetch := &probeFetch{}
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{
					probeMember(0, 0, "uid-a"),
					probeMember(0, 1, "uid-b"),
				},
			}

			got := observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardVerified, got.State,
				"a group whose every member carried this operator's own request is verified")
			assert.Len(t, fetch.asked, 2,
				"every member is asked, because one member's success says nothing about the others")
			for _, url := range fetch.asked {
				assert.Contains(t, url, ":8000", "the probe asks the member's own serving address")
				assert.NotContains(t, url, "Service", "no Service is on the probe path")
			}
		})
	}
}

// TestGroupForwardProbeRequestIsIdentifiable pins the request itself, because an operator reading
// the engine's log has to be able to tell this operator's probe from production traffic, and
// because the identifier is what the answer is bound to.
func TestGroupForwardProbeRequestIsIdentifiable(t *testing.T) {
	testCases := []struct {
		name       string
		engine     string
		wantPath   string
		wantFields map[string]any
	}{
		{
			name: "vLLM", engine: workercore.ModelDeploymentEngineVLLM,
			wantPath: "/v1/completions",
			wantFields: map[string]any{
				// The document is read back as generic JSON, so every number arrives a float.
				"max_tokens": float64(1), "temperature": float64(0),
			},
		},
		{
			name: "SGLang", engine: workercore.ModelDeploymentEngineSGLang,
			wantPath: "/generate",
			wantFields: map[string]any{
				"log_metrics": false,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md, role := probeDeployment(tc.engine, "")
			var seen string
			var sent []byte
			fetch := func(_ context.Context, url string, body []byte) ([]byte, error) {
				// The FIRST member's request, so the assertion is about one probe rather than
				// whichever member happened to be asked last.
				if seen == "" {
					seen, sent = url, body
				}

				return boundProbeBody(body), nil
			}
			// A multi-member replica, because a single-member one is answered before the engine is
			// even looked at and would ask nothing.
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{
					probeMember(0, 0, "uid-a"),
					probeMember(0, 1, "uid-b"),
				},
			}

			observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch)

			assert.Contains(t, seen, tc.wantPath, "the probe uses the engine's own serving route")

			var decoded map[string]any
			require.NoError(t, json.Unmarshal(sent, &decoded))
			for field, want := range tc.wantFields {
				assert.Equal(t, want, decoded[field], "request field %s", field)
			}
			rid, _ := decoded["request_id"].(string)
			if rid == "" {
				rid, _ = decoded["rid"].(string)
			}
			// Spelled out rather than compared against the constant: a constant that lost its value
			// would make this assertion agree with itself.
			assert.True(t, strings.HasPrefix(rid, "gpustack-qualify-"),
				"the identifier is self-describing so the probe can be filtered from the engine's "+
					"own logs and counters, got %q", rid)
			assert.Contains(t, rid, "uid-a", "the identifier carries the member it was sent to")
		})
	}
}

// TestGroupForwardProbeHoldsOnAnythingItCannotBind is the saturation and ambiguity contract. Every
// case here is a way the probe can fail to learn anything, and every one of them must read Unknown
// — which holds. None of them may read Verified, and none may read Failed, because a probe that
// could not be understood is an absence of evidence and a fault belongs to a definite health leg.
//
// EACH CASE ANSWERS WITH THE RIGHT ENGINE'S OWN DOCUMENT and the SECOND member's own identifier, so
// a case fails only for the reason it names: a case whose answer belonged to nobody would stay
// Unknown under every weakening of the binding, and would prove nothing about that binding.
func TestGroupForwardProbeHoldsOnAnythingItCannotBind(t *testing.T) {
	secondRID := modelDeploymentQualifyRIDPrefix + "uid-b"

	testCases := []struct {
		name   string
		engine string
		// answer is the second member's response; failure is the error it answers with instead.
		answer  string
		failure error
	}{
		{
			name:   "a member that never answers is a hold, not a fault",
			engine: workercore.ModelDeploymentEngineVLLM,
			// A saturated engine queues rather than refuses, so the bound expires with the request
			// still in it. That is the coordinator's saturation case.
			failure: errors.New("context deadline exceeded"),
		},
		{
			name:   "a vLLM member that answers for somebody else's request is a hold",
			engine: workercore.ModelDeploymentEngineVLLM,
			answer: `{"id":"cmpl-someone-else","choices":[{"text":"ok","finish_reason":"length"}]}`,
		},
		{
			name:   "a vLLM member that never finished is a hold",
			engine: workercore.ModelDeploymentEngineVLLM,
			// A choice that arrived with no reason is the case the finish-reason rule owns: a
			// request that stopped without saying why is not one this operator can say forwarded.
			answer: `{"id":"cmpl-` + secondRID + `","choices":[{"text":"ok","finish_reason":null}]}`,
		},
		{
			name:   "a vLLM member that answers with a document this operator cannot read is a hold",
			engine: workercore.ModelDeploymentEngineVLLM,
			answer: "not json at all",
		},
		{
			name:   "a vLLM member that answers with an engine error is a hold",
			engine: workercore.ModelDeploymentEngineVLLM,
			answer: `{"error":"engine is loading"}`,
		},
		{
			name:   "an SGLang member that answers for somebody else's request is a hold",
			engine: workercore.ModelDeploymentEngineSGLang,
			answer: `{"text":"ok","meta_info":{"id":"someone-else","finish_reason":{"type":"length"}}}`,
		},
		{
			name:   "an SGLang member that never recorded a finish reason is a hold",
			engine: workercore.ModelDeploymentEngineSGLang,
			answer: `{"text":"ok","meta_info":{"id":"` + secondRID + `"}}`,
		},
		{
			name:   "an SGLang member that answers null for its finish reason is a hold",
			engine: workercore.ModelDeploymentEngineSGLang,
			answer: `{"text":"ok","meta_info":{"id":"` + secondRID + `","finish_reason":null}}`,
		},
		{
			name:   "an SGLang member that answers with a document this operator cannot read is a hold",
			engine: workercore.ModelDeploymentEngineSGLang,
			answer: "<html>502</html>",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md, role := probeDeployment(tc.engine, "")
			fetch := &probeFetch{}
			if tc.answer != "" {
				fetch.answers = map[string]string{probeHost(0, 1): tc.answer}
			}
			if tc.failure != nil {
				fetch.failures = map[string]error{probeHost(0, 1): tc.failure}
			}
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{
					probeMember(0, 0, "uid-a"),
					probeMember(0, 1, "uid-b"),
				},
			}

			got := observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardUnknown, got.State,
				"an answer this operator cannot bind leaves the group held, and never faults it")
			assert.NotEqual(t, modelDeploymentGroupForwardVerified, got.State)
			assert.NotEmpty(t, got.Reason, "a hold says what could not be learned")
		})
	}
}

// TestGroupForwardProbeHoldsWhenOneMemberOfTheGroupIsSilent pins the whole-group requirement: a
// group whose second member cannot answer is held even when the first answered perfectly, because
// the leg is about the group and one member's success is not a statement about its peers.
func TestGroupForwardProbeHoldsWhenOneMemberOfTheGroupIsSilent(t *testing.T) {
	md, role := probeDeployment(workercore.ModelDeploymentEngineVLLM, "")
	fetch := &probeFetch{failures: map[string]error{probeHost(0, 1): errors.New("no route to host")}}
	view := modelDeploymentReplicaView{
		Role: "server", Ordinal: 0, Seated: true,
		Members: []*core.Pod{
			probeMember(0, 0, "uid-a"),
			probeMember(0, 1, "uid-b"),
		},
	}

	got := observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardUnknown, got.State,
		"one silent member holds the whole group")
	assert.Contains(t, got.Reason, probeMember(0, 1, "uid-b").Name,
		"the reason names the member an operator can go and look at")
}

// TestGroupForwardProbeIsUnsupportedWhereNoObservationExists pins the Unsupported reasons, which
// are the ones an operator reads when a group stays held. Each names its own obstacle, because
// the two engines fail the proof differently and "P/D is unsupported" does not say which.
func TestGroupForwardProbeIsUnsupportedWhereNoObservationExists(t *testing.T) {
	testCases := []struct {
		name        string
		engine      string
		kind        workercore.ModelDeploymentRoleKind
		wantInWords string
	}{
		{
			name:        "an engine with no verified observation is named",
			engine:      "TensorRT",
			wantInWords: "no group-forward observation this operator has verified",
		},
		{
			name:        "a vLLM prefill role carries the client-coordinated reason",
			engine:      workercore.ModelDeploymentEngineVLLM,
			kind:        workercore.ModelDeploymentRoleKindPrefill,
			wantInWords: "pair of requests",
		},
		{
			name:        "a vLLM decode role carries the same reason",
			engine:      workercore.ModelDeploymentEngineVLLM,
			kind:        workercore.ModelDeploymentRoleKindDecode,
			wantInWords: "pair of requests",
		},
		{
			name:        "an SGLang disaggregated role carries the all-reduce reason",
			engine:      workercore.ModelDeploymentEngineSGLang,
			kind:        workercore.ModelDeploymentRoleKindPrefill,
			wantInWords: "all-reduce",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md, role := probeDeployment(tc.engine, tc.kind)
			fetch := &probeFetch{}
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{
					probeMember(0, 0, "uid-a"),
					probeMember(0, 1, "uid-b"),
				},
			}

			got := observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardUnsupported, got.State,
				"a shape with no verified observation stays held, not admitted")
			assert.Contains(t, got.Reason, tc.wantInWords,
				"the reason names its own obstacle: %s", got.Reason)
			assert.Empty(t, fetch.asked,
				"nothing is asked of an engine before it is known to have a verified observation")
		})
	}
}

// TestGroupForwardProbeDoesNotProbeAnEncoder pins the role the leg has no question about.
//
// AN ENCODER SERVES NO TOKENS, so neither engine's completion route is an observation of its forward
// path: there is nothing to send that a bound answer could prove. Probing one would cost a request
// against an engine that cannot answer it, and the answer could only ever be Unknown, which reads as
// a hold on a healthy group. NotApplicable is therefore the whole answer, and it is asked BEFORE the
// engine and the transport, so no network call can reach an encoder at any engine.
//
// THE CASES SHARE ONE LOOP because the role is the only fact varied, and each engine is a separate
// assertion that the refusal does not depend on which engine happens to be deployed.
func TestGroupForwardProbeDoesNotProbeAnEncoder(t *testing.T) {
	testCases := []struct {
		name   string
		engine string
	}{
		{
			name:   "a vLLM encoder is not applicable",
			engine: workercore.ModelDeploymentEngineVLLM,
		},
		{
			name:   "an SGLang encoder is not applicable",
			engine: workercore.ModelDeploymentEngineSGLang,
		},
		{
			// An engine with no verified observation at all: an encoder is refused before that
			// question is asked, so the answer cannot become Unsupported by the engine the
			// deployment happens to name.
			name:   "an encoder on an engine with no verified observation is not applicable",
			engine: "TensorRT",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md, role := probeDeployment(tc.engine, workercore.ModelDeploymentRoleKindEncode)
			fetch := &probeFetch{}
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{
					probeMember(0, 0, "uid-a"),
					probeMember(0, 1, "uid-b"),
				},
			}

			got := observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardNotApplicable, got.State)
			assert.Contains(t, got.Reason, "Encode")
			assert.Empty(t, fetch.asked,
				"nothing is asked of an encoder, which serves no tokens to bind")
		})
	}
}

// TestGroupForwardProbeLeavesTheSingleMemberPathUntouched is the byte-identical guarantee the
// coordinator asked for. A single-member replica is answered before the engine and the transport
// are even looked at, so nothing this operator added can reach it: the fetch is never called, and
// the answer is the one T7 shipped.
func TestGroupForwardProbeLeavesTheSingleMemberPathUntouched(t *testing.T) {
	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang, "TensorRT",
	} {
		t.Run(engine, func(t *testing.T) {
			md, role := probeDeployment(engine, workercore.ModelDeploymentRoleKindPrefill)
			fetch := &probeFetch{}
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{probeMember(0, 0, "uid-a")},
			}

			got := observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardNotApplicable, got.State,
				"a replica of one member has no cross-member forward path, whatever the engine is")
			assert.Empty(t, fetch.asked,
				"the single-member path makes no request on any engine")
		})
	}
}

// TestGroupForwardProbeHoldsWithNoTransport pins the seam's fail-closed direction. A nil fetch is
// not a license to assume the group works.
func TestGroupForwardProbeHoldsWithNoTransport(t *testing.T) {
	md, role := probeDeployment(workercore.ModelDeploymentEngineVLLM, "")
	view := modelDeploymentReplicaView{
		Role: "server", Ordinal: 0, Seated: true,
		Members: []*core.Pod{
			probeMember(0, 0, "uid-a"),
			probeMember(0, 1, "uid-b"),
		},
	}

	got := observeModelDeploymentGroupForward(context.Background(), md, role, view, nil)

	assert.Equal(t, modelDeploymentGroupForwardUnknown, got.State,
		"a group nobody could ask is held, because nothing was learned about it")
}

// TestGroupForwardProbeHoldsWhenAMemberHasNoAddressYet covers the pass where a member exists but
// has no address: the pod was created this pass and has no IP. That is the unobserved case, not a
// fault, and it must not be reported as a group that could not forward.
func TestGroupForwardProbeHoldsWhenAMemberHasNoAddressYet(t *testing.T) {
	md, role := probeDeployment(workercore.ModelDeploymentEngineVLLM, "")
	unaddressed := probeMember(0, 1, "uid-b")
	unaddressed.Status.PodIP = ""
	fetch := &probeFetch{}
	view := modelDeploymentReplicaView{
		Role: "server", Ordinal: 0, Seated: true,
		Members: []*core.Pod{probeMember(0, 0, "uid-a"), unaddressed},
	}

	got := observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardUnknown, got.State)
	assert.Contains(t, got.Reason, "no address", "the reason says the member was not reachable yet")
}

// boundProbeFetch is a transport whose every member answers the request it was sent, bound to that
// request. A case that is about some OTHER leg passes this so the group leg is a stated fact —
// verified — instead of a side effect of the case having passed no transport at all.
func boundProbeFetch(_ context.Context, _ string, body []byte) ([]byte, error) {
	return boundProbeBody(body), nil
}
