package worker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
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
		return []byte(strings.ReplaceAll(answer, probeRIDPlaceholder, probeRequestRID(body))), nil
	}

	return boundProbeBody(body), nil
}

// boundProbeBody renders the answer the named engine would give to the request just sent, which is
// the only answer that counts as evidence.
func boundProbeBody(body []byte) []byte {
	rid := probeRequestRID(body)

	if strings.Contains(string(body), "sampling_params") {
		return []byte(`{"text":"ok","meta_info":{"id":"` + rid +
			`","finish_reason":{"type":"length"}}}`)
	}

	return []byte(`{"id":"cmpl-` + rid + `","choices":[{"text":"ok","finish_reason":"length"}]}`)
}

// probeRIDPlaceholder is where a scripted answer says "the identifier this request carried".
//
// THE IDENTIFIER IS FRESH PER ATTEMPT, so an answer cannot be written down once and replayed: a
// scripted answer names the identifier with this placeholder and the fixture fills it in with the
// one the request carried.
const probeRIDPlaceholder = "{{rid}}"

// probeRequestRID reads the identifier a request carried, whichever field the engine reads it
// from.
func probeRequestRID(body []byte) string {
	var sent struct {
		RequestID string `json:"request_id"`
		RID       string `json:"rid"`
	}
	_ = json.Unmarshal(body, &sent)
	if sent.RequestID != "" {
		return sent.RequestID
	}

	return sent.RID
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
			answer: `{"id":"cmpl-` + probeRIDPlaceholder + `","choices":[{"text":"ok","finish_reason":null}]}`,
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
			answer: `{"text":"ok","meta_info":{"id":"` + probeRIDPlaceholder + `"}}`,
		},
		{
			name:   "an SGLang member that answers null for its finish reason is a hold",
			engine: workercore.ModelDeploymentEngineSGLang,
			answer: `{"text":"ok","meta_info":{"id":"` + probeRIDPlaceholder + `","finish_reason":null}}`,
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
// the answer is the one an ordinary group of one member has always had.
func TestGroupForwardProbeLeavesTheSingleMemberPathUntouched(t *testing.T) {
	for _, engine := range []string{
		workercore.ModelDeploymentEngineVLLM, workercore.ModelDeploymentEngineSGLang, "TensorRT",
	} {
		t.Run(engine, func(t *testing.T) {
			md, role := probeDeployment(engine, "")
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

// TestGroupForwardURLReadsThePortTheAnnotationNames proves that the port the member's own annotation
// names is read as the number it is, and not narrowed into a different one.
//
// THE ANNOTATION IS THE OPERATOR'S OWN MARKER, and the operator reads it back off the live member to
// learn where to ask. A number too large for the port's own width would otherwise be narrowed into a
// different, perfectly valid port: 4294967376 is 2^32 plus 80, so narrowing it leaves 80, which the
// range check that follows accepts. The probe would then ask the wrong port and read the answer as
// evidence about the group.
//
// THE CASES ARE THE BOUNDARY, so the table names what each value is: a port the operator wrote, and a
// number that is not one.
func TestGroupForwardURLReadsThePortTheAnnotationNames(t *testing.T) {
	cases := []struct {
		name    string
		port    string
		wantURL string
		wantErr string
	}{
		{
			name:    "the port the operator wrote is asked at",
			port:    "8000",
			wantURL: "http://" + probeMemberIP(0, 0) + ":8000/v1/chat/completions",
		},
		{
			name:    "a number that narrows into a valid port is refused",
			port:    "4294967376",
			wantErr: "does not carry a serving port this operator can read",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			member := probeMember(0, 0, "uid-a")
			member.Annotations["prometheus.io/port"] = tc.port

			address, err := modelDeploymentGroupForwardURL(member, "/v1/chat/completions")

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, address)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantURL, address)
		})
	}
}

// boundProbeFetch is a transport whose every member answers the request it was sent, bound to that
// request. A case that is about some OTHER leg passes this so the group leg is a stated fact —
// verified — instead of a side effect of the case having passed no transport at all.
func boundProbeFetch(_ context.Context, _ string, body []byte) ([]byte, error) {
	return boundProbeBody(body), nil
}

// probePairDeployment builds the deployment under a directed pair probe: a prefill role and a decode
// role, each declaring its own data-parallel width, and a router when the deployment has one.
//
// THE WIDTHS ARE DECLARED BECAUSE THEY ARE THE ONLY PLACE THE ENGINE'S DATA-PARALLEL WIDTH IS
// WRITTEN DOWN, and a probe that guessed them would ask about ranks the engine does not have.
func probePairDeployment(
	engine string, prefillWidth, decodeWidth int, router string,
) *workercore.ModelDeployment {
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"},
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: engine, Image: "engine:latest"},
			Model:  workercore.ModelDeploymentModel{Name: "Qwen/Qwen3-30B-A3B"},
		},
	}
	if router != "" {
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: router}
	}
	md.Spec.Roles = []workercore.ModelDeploymentRole{
		{
			Name: "prefill", Replicas: 1, ReplicaSize: 1, InstanceType: "h20-8x",
			Kind:      workercore.ModelDeploymentRoleKindPrefill,
			ExtraArgs: []string{"--dp-size", strconv.Itoa(prefillWidth)},
		},
		{
			Name: "decode", Replicas: 1, ReplicaSize: 1, InstanceType: "h20-8x",
			Kind:      workercore.ModelDeploymentRoleKindDecode,
			ExtraArgs: []string{"--dp-size", strconv.Itoa(decodeWidth)},
		},
	}

	return md
}

// probePairMember is one member of a pair's group, with the prefiller's bootstrap port published on
// it when the prefiller is the one being built.
//
// THE BOOTSTRAP PORT IS PUBLISHED ONLY ON THE PREFILLER, because it is the prefiller that serves the
// transfer endpoints the decode side has to be told about.
func probePairMember(ordinal, member int, uid, bootstrapPort string) *core.Pod {
	pod := probeMember(ordinal, member, uid)
	if bootstrapPort != "" {
		pod.Annotations[modelDeploymentGroupForwardPairBootstrapPortAnnotation] = bootstrapPort
	}

	return pod
}

// probePairEnginePortMember is one member of a group whose own annotation carries the engine's
// port, which is what the render stamps on a decoder the routing proxy fronts.
//
// THE PORT IS THE ENGINE'S, because the proxy owns the port the Service targets and the annotation
// names the engine behind it. A probe that read the annotation here would be asking a port nothing
// outside the proxy answers.
func probePairEnginePortMember(ordinal, member int, uid, enginePort string) *core.Pod {
	pod := probeMember(ordinal, member, uid)
	pod.Annotations["prometheus.io/port"] = enginePort

	return pod
}

// probePairView is one group's members as the pair probe sees them.
func probePairView(role string, ordinal int, members ...*core.Pod) modelDeploymentReplicaView {
	return modelDeploymentReplicaView{
		Role: role, Ordinal: ordinal, Seated: true, Members: members,
	}
}

// probePairFetch answers a directed pair probe the way the engine does: the prefiller's answer
// carries the request's own identifier and a partial generation, and the decoder's answer carries
// the identifier and the exact text the request asked to be copied.
//
// THE TEXT IS READ OUT OF THE REQUEST, so the fixture answers what the probe asked for without the
// test having to restate it, and a probe that asked for something else would be answered with
// something else.
type probePairFetch struct {
	mutex sync.Mutex
	// asked records every request, in the order the legs were sent.
	asked []modelDeploymentGroupForwardPairRequest
	// prefillHost is the address the prefiller is asked at, which is how the fixture knows which
	// leg it is answering.
	prefillHost string
	// answers maps an address to the body that address returns.
	answers map[string]string
	// failures maps an address to the error that address answers with.
	failures map[string]error
}

func (f *probePairFetch) fetch(
	_ context.Context, request modelDeploymentGroupForwardPairRequest,
) ([]byte, error) {
	f.mutex.Lock()
	f.asked = append(f.asked, request)
	f.mutex.Unlock()

	authority := request.URL[strings.Index(request.URL, "://")+3:]
	if slash := strings.Index(authority, "/"); slash >= 0 {
		authority = authority[:slash]
	}
	// THE METRICS SCRAPE IS ANSWERED BY THE STUB ITSELF, minimally: the observation reads the
	// mooncake series of both halves, and a GET that fell through to the chat answer would hand it
	// a document that reads as no series at all.
	if request.Method == "GET" {
		return []byte("vllm:mooncake_bytes_transferred_count 0.0\n" +
			"vllm:mooncake_num_failed_recvs 0.0\n"), nil
	}
	if err, failed := f.failures[authority]; failed {
		return nil, err
	}
	if answer, scripted := f.answers[authority]; scripted {
		return []byte(strings.ReplaceAll(answer, probeRIDPlaceholder, probePairRequestRID(request.Body))), nil
	}

	return probePairAnswer(request.Body, authority == f.prefillHost), nil
}

// probePairAnswer renders the answer one leg of a pair gives.
//
// THE PREFILLER ANSWERS WITH A PARTIAL GENERATION AND A CUT-SHORT FINISH REASON, because it hands
// over the KV and stops there. The decoder answers with the copied text and a normal finish.
func probePairAnswer(body []byte, isPrefill bool) []byte {
	rid := probePairRequestRID(body)
	prefix := ""
	if probePairCarriesRequestIDField(body) {
		prefix = "chatcmpl-"
	}

	if isPrefill {
		return []byte(`{"id":"` + prefix + rid +
			`","choices":[{"message":{"content":"探测"},"finish_reason":"length"}]}`)
	}

	return []byte(`{"id":"` + prefix + rid + `","choices":[{"message":{"content":"` +
		probePairExpectedText(body) + `"},"finish_reason":"stop"}]}`)
}

// probePairRequestRID reads the identifier a leg's request carried.
func probePairRequestRID(body []byte) string {
	var sent struct {
		RequestID string `json:"request_id"`
		RID       string `json:"rid"`
	}
	_ = json.Unmarshal(body, &sent)
	if sent.RequestID != "" {
		return sent.RequestID
	}

	return sent.RID
}

// probePairCarriesRequestIDField says whether the request carried the identifier in the field the
// other engine reads it from, which is how the fixture knows which prefix the engine puts in front
// of the identifier in its answer.
func probePairCarriesRequestIDField(body []byte) bool {
	var sent struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(body, &sent)

	return sent.RequestID != ""
}

// probePairExpectedText reads the text a leg's request asked to be copied.
func probePairExpectedText(body []byte) string {
	var sent struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &sent)
	if len(sent.Messages) == 0 {
		return ""
	}
	content := sent.Messages[0].Content
	start := strings.Index(content, "『")
	end := strings.Index(content, "』")
	if start < 0 || end < 0 || end < start {
		return ""
	}

	return content[start+3 : end]
}

// probePairBody reads one recorded request's fields back.
func probePairBody(t *testing.T, request modelDeploymentGroupForwardPairRequest) map[string]any {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(request.Body, &decoded))

	return decoded
}

// TestGroupForwardProbePDPairCarriesTheRequestOnBothHeads is the positive contract of the directed
// pair probe. Every rank pair of the two groups is asked, every request carries one identifier, and
// the decoder's answer carries the text the request asked to be copied, which is the only evidence
// that the prefiller's KV crossed.
func TestGroupForwardProbePDPairCarriesTheRequestOnBothHeads(t *testing.T) {
	md := probePairDeployment(workercore.ModelDeploymentEngineSGLang, 2, 2, "")
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0,
		probePairMember(1, 0, "uid-d", ""),
		probePairMember(1, 1, "uid-d2", ""))
	fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

	got := observeModelDeploymentGroupForwardPair(
		context.Background(), md, prefill, decode, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardVerified, got.Prefill.State,
		"a prefill group whose pairs all carried the request is verified")
	assert.Equal(t, modelDeploymentGroupForwardVerified, got.Decode.State,
		"a decode group whose pairs all carried the request is verified")
	assert.NotEqual(t, got.Prefill.Generation, got.Decode.Generation,
		"each role carries the generation of its own group")
	assert.Equal(t, got.Prefill.Generation, got.Attempt.PrefillGeneration,
		"the attempt records the generation it bound")
	assert.Equal(t, got.Decode.Generation, got.Attempt.DecodeGeneration,
		"the attempt records the generation it bound")

	// TWO RANKS ON EACH SIDE IS FOUR PAIRS, AND A PAIR IS TWO REQUESTS plus the two halves' metrics
	// scrapes that follow it: the observation reads the mooncake exposition of both halves after
	// the legs answer. D12/A.2.
	assert.Len(t, fetch.asked, 16, "every rank pair of both groups is asked, both legs of it, and each pair is followed by the two halves' metrics scrapes")
	assert.Len(t, got.Attempt.RequestIDs, 4, "each rank pair carries its own identifier")

	rooms := map[int64]bool{}
	for _, request := range fetch.asked {
		if request.Method == "GET" {
			continue
		}
		body := probePairBody(t, request)
		rid, _ := body["rid"].(string)
		assert.Contains(t, rid, "gpustack-qualify-",
			"the identifier is self-describing so the probe can be filtered from the engine's logs")

		room, _ := body["bootstrap_room"].(float64)
		rooms[int64(room)] = true
		assert.Equal(t, "10.0.0.1", body["bootstrap_host"],
			"the request names the prefiller's own address, for the transfer endpoints")
		assert.Equal(t, float64(8998), body["bootstrap_port"],
			"the request names the port the prefiller published")

		rank, _ := body["routed_dp_rank"].(float64)
		if request.URL == "http://"+probeHost(0, 0)+"/v1/chat/completions" {
			assert.Equal(t, int64(rank), int64(room)%2,
				"the room's residue names the prefiller rank the request is routed to")
		}
	}
	assert.Len(t, rooms, 4, "each rank pair is joined on its own room")
}

// TestGroupForwardProbePDPairNamesTheAnswerItRejected holds that the copied check's failure
// reason carries the answer it rejected, bounded, so the run's evidence can tell a model that
// drifted from a context that was never whole.
//
// THE REASON USED TO DISCARD THE ANSWER. A hold that said only that the text did not match left
// the classification — answer drift, a broken read, a thinking leak — to a rerun this probe exists
// to avoid. D12/A.1.
func TestGroupForwardProbePDPairNamesTheAnswerItRejected(t *testing.T) {
	md := probePairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1, "")
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))

	t.Run("a short wrong answer is named in full", func(t *testing.T) {
		fetch := &probePairFetch{
			prefillHost: probeHost(0, 0),
			answers: map[string]string{
				probeHost(1, 0): `{"id":"{{rid}}","choices":[{"message":{"content":"别的文字"},"finish_reason":"stop"}]}`,
			},
		}
		got := observeModelDeploymentGroupForwardPair(
			context.Background(), md, prefill, decode, fetch.fetch)

		assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Prefill.State,
			"a decoder that did not carry the copy holds the pair")
		assert.Contains(t, got.Prefill.Reason, `(answer: "别的文字")`,
			"the reason names the answer it rejected, so drift is tellable from a broken read")
	})

	t.Run("a long answer is bounded on rune boundaries", func(t *testing.T) {
		long := strings.Repeat("长", 300)
		fetch := &probePairFetch{
			prefillHost: probeHost(0, 0),
			answers: map[string]string{
				probeHost(1, 0): `{"id":"{{rid}}","choices":[{"message":{"content":"` + long + `"},"finish_reason":"stop"}]}`,
			},
		}
		got := observeModelDeploymentGroupForwardPair(
			context.Background(), md, prefill, decode, fetch.fetch)

		assert.Contains(t, got.Prefill.Reason, strings.Repeat("长", 200),
			"the bound keeps the first two hundred characters")
		assert.NotContains(t, got.Prefill.Reason, strings.Repeat("长", 201),
			"the bound stops at two hundred characters")
		assert.Contains(t, got.Prefill.Reason, "…",
			"the cut is marked")
	})
}

// TestGroupForwardProbePDPairRecordsTheMooncakeCountersOfBothHalves holds that the observation
// records the mooncake series of both halves and says explicitly that the connector exports no
// read-side success counter, so a reader can tell push evidence from pull evidence.
//
// THE COUNTER THE RUN READ WAS THE SENDER'S. vllm:mooncake_bytes_transferred is the prefill half's
// histogram, and its _count says pushes happened; nothing exported counts a successful pull, which
// is why the copied answer is the read-side proof. D12/A.2.
func TestGroupForwardProbePDPairRecordsTheMooncakeCountersOfBothHalves(t *testing.T) {
	md := probePairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1, "")
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))
	exposition := "# HELP vllm:mooncake_bytes_transferred transferred bytes\n" +
		"vllm:mooncake_bytes_transferred_count 2.0\n" +
		"# HELP vllm:mooncake_num_failed_recvs failed receives\n" +
		"vllm:mooncake_num_failed_recvs 0.0\n"
	fetch := func(_ context.Context, request modelDeploymentGroupForwardPairRequest) ([]byte, error) {
		if strings.HasSuffix(request.URL, "/metrics") {
			return []byte(exposition), nil
		}
		return probePairAnswer(request.Body, strings.Contains(request.URL, probeHost(0, 0))), nil
	}

	got := observeModelDeploymentGroupForwardPair(
		context.Background(), md, prefill, decode, fetch)

	assert.Equal(t, modelDeploymentGroupForwardVerified, got.Prefill.State,
		"a pair that carried the request is still verified")
	assert.Contains(t, got.Prefill.Observation, "prefill: vllm:mooncake_bytes_transferred_count 2.0",
		"the prefill half's mooncake series are recorded as push evidence")
	assert.Contains(t, got.Decode.Observation, "decode: vllm:mooncake_num_failed_recvs 0.0",
		"the decode half's mooncake series are recorded beside them")
	assert.Contains(t, got.Decode.Observation, "no read-side success counter",
		"the observation says explicitly that no exported series counts a successful pull")
}

// TestGroupForwardProbePDPairHoldsOnAnythingItCannotSee is the fail-closed contract of the directed
// pair probe. Every case is a way the pair can fail to be observed, and every one of them reads
// Unknown for BOTH roles, because a pair that could not be observed says nothing about which side
// is at fault.
func TestGroupForwardProbePDPairHoldsOnAnythingItCannotSee(t *testing.T) {
	testCases := []struct {
		name        string
		engine      string
		router      string
		width       int
		answer      string
		failure     error
		noBootstrap bool
		wantInWords string
	}{
		{
			name:        "the decoder answering with another pair's text is a hold",
			engine:      workercore.ModelDeploymentEngineSGLang,
			answer:      `{"id":"` + probeRIDPlaceholder + `","choices":[{"message":{"content":"别的文字"},"finish_reason":"stop"}]}`,
			wantInWords: "did not carry the text",
		},
		{
			name:        "the decoder answering for another request is a hold",
			engine:      workercore.ModelDeploymentEngineSGLang,
			answer:      `{"id":"someone-else","choices":[{"message":{"content":"探测P0D0：星河春风。"},"finish_reason":"stop"}]}`,
			wantInWords: "not bound to this operator's own request",
		},
		{
			name:        "the decoder answering without an answer is a hold",
			engine:      workercore.ModelDeploymentEngineSGLang,
			answer:      `{"id":"` + probeRIDPlaceholder + `","choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
			wantInWords: "did not carry the text",
		},
		{
			name:        "the decoder that did not finish normally is a hold",
			engine:      workercore.ModelDeploymentEngineSGLang,
			answer:      `{"id":"` + probeRIDPlaceholder + `","choices":[{"message":{"content":"探测P0D0：星河春风。"},"finish_reason":"length"}]}`,
			wantInWords: "did not finish normally",
		},
		{
			name:        "a leg that never answers is a hold",
			engine:      workercore.ModelDeploymentEngineSGLang,
			failure:     errors.New("context deadline exceeded"),
			wantInWords: "did not answer within the bound",
		},
		{
			name:        "a prefiller that publishes no bootstrap port is a hold",
			engine:      workercore.ModelDeploymentEngineSGLang,
			noBootstrap: true,
			wantInWords: "publishes no bootstrap port",
		},
		{
			// The pair is reached at the decoder's own engine address, so the operator drives
			// both heads itself and has to build the connector's handshake. That handshake
			// names the prefiller by its engine id, which lives in the prefiller's own
			// transfer registry, and this operator does not read it.
			name:        "a vLLM pair with no routing proxy holds on the prefiller's engine id",
			engine:      workercore.ModelDeploymentEngineVLLM,
			wantInWords: "engine id",
		},
		{
			name:        "a group wider than the bound this operator probes is a hold",
			engine:      workercore.ModelDeploymentEngineSGLang,
			width:       modelDeploymentGroupForwardPairMaxWidth + 1,
			wantInWords: "above the bound of " + strconv.Itoa(modelDeploymentGroupForwardPairMaxWidth),
		},
		{
			// THE FRONT DOOR IS THE ONE TO ASK, and this row is written so that asking the
			// engine's port instead would be a different verdict: the engine's port is not
			// scripted here, so a probe that dialed it would have been answered and the
			// pair would not have held at all.
			name:        "a fronted decoder is asked at its front door and not at its engine port",
			engine:      workercore.ModelDeploymentEngineSGLang,
			router:      workercore.ModelDeploymentRouterLLMD,
			failure:     errors.New("connection refused"),
			wantInWords: "did not answer within the bound",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			width := tc.width
			if width < 1 {
				width = 1
			}
			md := probePairDeployment(tc.engine, width, width, tc.router)
			bootstrapPort := "8998"
			if tc.noBootstrap {
				bootstrapPort = ""
			}
			prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", bootstrapPort))
			decodeMember := probePairMember(1, 0, "uid-d", "")
			if tc.router != "" {
				// A fronted decoder answers on two ports: the one its Service targets, which
				// the routing proxy opens, and the engine's own port behind it. The
				// annotation carries the engine's port, so the fixture models the split the
				// render produces.
				decodeMember = probePairEnginePortMember(1, 0, "uid-d", "8200")
			}
			decode := probePairView("decode", 0, decodeMember)
			fetch := &probePairFetch{prefillHost: probeHost(0, 0)}
			if tc.answer != "" {
				fetch.answers = map[string]string{probeHost(1, 0): tc.answer}
			}
			if tc.failure != nil {
				fetch.failures = map[string]error{probeHost(1, 0): tc.failure}
			}

			got := observeModelDeploymentGroupForwardPair(
				context.Background(), md, prefill, decode, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Prefill.State,
				"a pair that could not be observed is held for the prefill role")
			assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Decode.State,
				"a pair that could not be observed is held for the decode role")
			assert.NotEqual(t, modelDeploymentGroupForwardFailed, got.Prefill.State,
				"a pair that could not be observed faults nobody")
			assert.NotEqual(t, modelDeploymentGroupForwardFailed, got.Decode.State,
				"a pair that could not be observed faults nobody")
			assert.Contains(t, got.Decode.Reason, tc.wantInWords,
				"the reason names what could not be learned: %s", got.Decode.Reason)
		})
	}
}

// TestGroupForwardProbePDPairIsUnsupportedOnAnEngineWithNoObservation pins the fail-closed direction
// for an engine the matrix says nothing about.
func TestGroupForwardProbePDPairIsUnsupportedOnAnEngineWithNoObservation(t *testing.T) {
	md := probePairDeployment("TensorRT", 1, 1, "")
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))
	fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

	got := observeModelDeploymentGroupForwardPair(
		context.Background(), md, prefill, decode, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardUnsupported, got.Prefill.State)
	assert.Equal(t, modelDeploymentGroupForwardUnsupported, got.Decode.State)
	assert.Contains(t, got.Decode.Reason, "no group-forward observation",
		"the reason names the engine it has no observation of")
	assert.Empty(t, fetch.asked, "nothing is asked of an engine with no verified observation")
}

// TestGroupForwardProbePDPairHoldsWithNoTransport pins the fail-closed direction of the pair probe.
func TestGroupForwardProbePDPairHoldsWithNoTransport(t *testing.T) {
	md := probePairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1, "")
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))

	got := observeModelDeploymentGroupForwardPair(
		context.Background(), md, prefill, decode, nil)

	assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Prefill.State,
		"a pair nobody could ask is held")
	assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Decode.State,
		"a pair nobody could ask is held")
}

// TestGroupForwardProbePDPairNamesThePrefillerToTheRoutingProxy covers the shape the operator's own
// rendering produces: the decode role carries the routing proxy, the proxy drives the transfer leg,
// and the operator names the prefiller in the header the router's picker writes.
//
// IT IS THE ROUTE THE OPERATOR CAN REALLY REACH, because the proxy is what the decode role's
// Service targets and what the gates grade.
//
// THE DECLARED WIDTHS DO NOT CHANGE HOW MANY QUESTIONS ARE ASKED: the proxy picks the rank inside
// the engine it fronts, so the shape is one question whatever the widths say.
func TestGroupForwardProbePDPairNamesThePrefillerToTheRoutingProxy(t *testing.T) {
	testCases := []struct {
		name         string
		engine       string
		prefillWidth int
		decodeWidth  int
	}{
		{
			name: "vLLM", engine: workercore.ModelDeploymentEngineVLLM,
			prefillWidth: 1, decodeWidth: 1,
		},
		{
			name: "SGLang", engine: workercore.ModelDeploymentEngineSGLang,
			prefillWidth: 1, decodeWidth: 1,
		},
		{
			name: "SGLang with ranks on both sides", engine: workercore.ModelDeploymentEngineSGLang,
			prefillWidth: 2, decodeWidth: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := probePairDeployment(
				tc.engine, tc.prefillWidth, tc.decodeWidth, workercore.ModelDeploymentRouterLLMD)
			prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
			decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))
			fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

			got := observeModelDeploymentGroupForwardPair(
				context.Background(), md, prefill, decode, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardVerified, got.Decode.State,
				"a decode group whose pair carried the request is verified")
			assert.Equal(t, modelDeploymentGroupForwardVerified, got.Prefill.State,
				"the prefill group the proxy paired with is verified with it")
			assert.Len(t, fetch.asked, 3,
				"the proxy drives the transfer leg, so the operator asks it one question, and the two halves' metrics scrapes follow it")
			assert.Len(t, got.Attempt.RequestIDs, 1,
				"the one question carries its own identifier")

			request := fetch.asked[0]
			assert.Contains(t, request.URL, probeHost(1, 0),
				"the decoder is reached at its own serving address")
			assert.Equal(t, probeHost(0, 0),
				request.Headers[modelDeploymentGroupForwardPairPrefillerHeader],
				"the request names the prefiller the proxy is to pair with")
		})
	}
}

// TestGroupForwardProbePDPairAttemptRecordsTheShape pins the typed shape value, so a reader can
// tell a proxy-path answer from a rank-covering one without reading the reason.
//
// THE TWO PATHS SHOW DIFFERENT THINGS: the head-to-head path drove both heads and can read the
// transfer in the decode answer, and the proxy path asked one question whose answer shows that
// the front door was alive and returned the text the request asked to be copied.
func TestGroupForwardProbePDPairAttemptRecordsTheShape(t *testing.T) {
	testCases := []struct {
		name          string
		router        string
		wantShape     modelDeploymentGroupForwardPairShape
		wantInWords   string
		wantNotInText string
		wantRooms     int
	}{
		{
			name:        "the head-to-head path names the transfer it read",
			router:      "",
			wantShape:   modelDeploymentGroupForwardPairShapeHeadToHead,
			wantInWords: "the prefill's KV reached it",
			wantRooms:   1,
		},
		{
			name:          "the proxy path names the proxy and claims no transfer",
			router:        workercore.ModelDeploymentRouterLLMD,
			wantShape:     modelDeploymentGroupForwardPairShapeViaRoutingProxy,
			wantInWords:   "the decode group's routing proxy answered the probe",
			wantNotInText: "the prefill's KV reached it",
			wantRooms:     0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := probePairDeployment(
				workercore.ModelDeploymentEngineSGLang, 1, 1, tc.router)
			prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
			decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))
			fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

			got := observeModelDeploymentGroupForwardPair(
				context.Background(), md, prefill, decode, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardVerified, got.Decode.State)
			assert.Equal(t, tc.wantShape, got.Attempt.Shape,
				"the attempt records the path it took")
			assert.Contains(t, got.Decode.Reason, tc.wantInWords,
				"the reason names what the path showed: %s", got.Decode.Reason)
			if tc.wantNotInText != "" {
				assert.NotContains(t, got.Decode.Reason, tc.wantNotInText,
					"the reason claims no more than the leg shows: %s", got.Decode.Reason)
			}
			assert.Len(t, got.Attempt.BootstrapRooms, tc.wantRooms,
				"the proxy path records no room, because the proxy picks it")
		})
	}
}

// TestGroupForwardProbePDPairAsksTheFrontedDecoderAtItsFrontDoor pins the address the proxy leg is
// sent to.
//
// A FRONTED DECODER ANSWERS ON TWO PORTS: the one its Service targets, which the routing proxy opens,
// and the engine's own port behind it. The annotation carries the engine's port, so a probe that read
// it would be asking a port nothing outside the proxy answers, and the engine behind the proxy does
// not drive the transfer leg on its own.
//
// THE ENGINE'S PORT IS SCRIPTED TO ANSWER WRONGLY, so a probe that dialed it would be held with the
// wrong verdict and this case would fail. The front door is the only address that can verify.
func TestGroupForwardProbePDPairAsksTheFrontedDecoderAtItsFrontDoor(t *testing.T) {
	md := probePairDeployment(
		workercore.ModelDeploymentEngineSGLang, 1, 1, workercore.ModelDeploymentRouterLLMD)
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairEnginePortMember(1, 0, "uid-d", "8200"))
	fetch := &probePairFetch{
		prefillHost: probeHost(0, 0),
		answers: map[string]string{
			probeMemberIP(1, 0) + ":8200": `{"id":"` + probeRIDPlaceholder +
				`","choices":[{"message":{"content":"别的文字"},"finish_reason":"stop"}]}`,
		},
	}

	got := observeModelDeploymentGroupForwardPair(
		context.Background(), md, prefill, decode, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardVerified, got.Decode.State,
		"the front door is the address the proxy answers on")
	assert.Len(t, fetch.asked, 3,
		"the proxy drives the transfer leg, so the operator asks it one question, and the two halves' metrics scrapes follow it")
	assert.Equal(t, "http://"+probeHost(1, 0)+"/v1/chat/completions", fetch.asked[0].URL,
		"the front door is the port the Service targets, not the engine's port behind it")
	assert.Equal(t, probeHost(0, 0),
		fetch.asked[0].Headers[modelDeploymentGroupForwardPairPrefillerHeader],
		"the prefiller is named by its own engine address, because no proxy fronts it")
}

// TestGroupForwardProbePDPairNamesTheSideItCannotAsk pins the side in the reason.
//
// A group whose members carry no leader index cannot answer for itself, and the reason has to say
// WHICH group it was: a hold that names neither side leaves an operator with two groups to check.
func TestGroupForwardProbePDPairNamesTheSideItCannotAsk(t *testing.T) {
	testCases := []struct {
		name        string
		prefill     modelDeploymentReplicaView
		decode      modelDeploymentReplicaView
		wantInWords string
	}{
		{
			name: "the prefill group is named",
			prefill: probePairView("prefill", 0,
				probePairMember(0, 1, "uid-p1", "8998"),
				probePairMember(0, 2, "uid-p2", "")),
			decode:      probePairView("decode", 0, probePairMember(1, 0, "uid-d", "")),
			wantInWords: "the prefill group has no address to answer for it",
		},
		{
			name:    "the decode group is named",
			prefill: probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998")),
			decode: probePairView("decode", 0,
				probePairMember(1, 1, "uid-d1", ""),
				probePairMember(1, 2, "uid-d2", "")),
			wantInWords: "the decode group has no address to answer for it",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := probePairDeployment(workercore.ModelDeploymentEngineSGLang, 1, 1, "")
			fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

			got := observeModelDeploymentGroupForwardPair(
				context.Background(), md, tc.prefill, tc.decode, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Prefill.State,
				"a group that could not be asked is held")
			assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Decode.State,
				"a group that could not be asked is held")
			assert.Contains(t, got.Decode.Reason, tc.wantInWords,
				"the reason names the side that could not be asked: %s", got.Decode.Reason)
			assert.Empty(t, fetch.asked, "nothing is asked of a group with no address to answer for it")
		})
	}
}

// TestGroupForwardProbePDPairWidthBoundIsTheRoomsOwn pins the bound, and the two sides of it.
//
// THE ROOM IS A RANDOM 52-BIT VALUE SCALED BY THE WIDTH AND OFFSET BY THE RANK, so the largest
// room is 2^52 * width - 1. That stays inside the positive range of an int64 while the width is 2048
// and leaves it at 2049, which is where the bound sits: the widest group the room can still address
// is probed, and the first width past it is held.
func TestGroupForwardProbePDPairWidthBoundIsTheRoomsOwn(t *testing.T) {
	assert.Empty(t,
		modelDeploymentGroupForwardPairWidthBound(
			"prefill", modelDeploymentGroupForwardPairMaxWidth,
			modelDeploymentGroupForwardPairShapeHeadToHead),
		"the widest group the room can address is probed")
	assert.Contains(t,
		modelDeploymentGroupForwardPairWidthBound(
			"prefill", modelDeploymentGroupForwardPairMaxWidth+1,
			modelDeploymentGroupForwardPairShapeHeadToHead),
		"above the bound of "+strconv.Itoa(modelDeploymentGroupForwardPairMaxWidth),
		"the first width past the bound is held, and the reason names the bound")
	assert.Contains(t,
		modelDeploymentGroupForwardPairWidthBound(
			"prefill", modelDeploymentGroupForwardPairMaxWidth+1,
			modelDeploymentGroupForwardPairShapeViaRoutingProxy),
		"above this probe's supported limit of "+
			strconv.Itoa(modelDeploymentGroupForwardPairMaxWidth),
		"the proxy path's reason states the limit rather than a room this operator never builds")

	md := probePairDeployment(
		workercore.ModelDeploymentEngineSGLang, modelDeploymentGroupForwardPairMaxWidth+1, 1, "")
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))
	fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

	got := observeModelDeploymentGroupForwardPair(context.Background(), md, prefill, decode, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Prefill.State,
		"a group wider than the room can address is held")
	assert.Contains(t, got.Prefill.Reason, "the prefill group declares a data-parallel width",
		"the reason names the side and the width: %s", got.Prefill.Reason)
	assert.Empty(t, fetch.asked, "a group this operator does not probe is not asked")
}

// TestGroupForwardProbePDPairWidthBoundInTheProxyShape pins the proxy path's own bound reason.
//
// THE PROXY PICKS THE ROOM THERE, so the room arithmetic the head-to-head reason cites is never
// exercised, and a reason that cited it would be naming a calculation this operator does not make.
func TestGroupForwardProbePDPairWidthBoundInTheProxyShape(t *testing.T) {
	md := probePairDeployment(
		workercore.ModelDeploymentEngineSGLang, modelDeploymentGroupForwardPairMaxWidth+1, 1,
		workercore.ModelDeploymentRouterLLMD)
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))
	fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

	got := observeModelDeploymentGroupForwardPair(context.Background(), md, prefill, decode, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardUnknown, got.Prefill.State,
		"a group wider than the bound is held in the proxy path too")
	assert.Contains(t, got.Prefill.Reason,
		"above this probe's supported limit of "+
			strconv.Itoa(modelDeploymentGroupForwardPairMaxWidth),
		"the reason names the limit it applies to: %s", got.Prefill.Reason)
	assert.NotContains(t, got.Prefill.Reason, "the bootstrap room is a random 52-bit value",
		"the proxy path does not build a room, so it must not cite one")
	assert.Empty(t, fetch.asked, "a group this operator does not probe is not asked")
}

// TestGroupForwardProbePDPairAddressesEveryRankAtTheWidestRoom pins the room arithmetic where it
// is tightest: every rank of the widest group the room can still address is asked, and every room
// names the rank it was built for.
//
// THE ROOM IS SCALED BY THE WIDTH, so the widest group is the one where a scaled room comes closest
// to leaving the range the rank arithmetic reads.
func TestGroupForwardProbePDPairAddressesEveryRankAtTheWidestRoom(t *testing.T) {
	width := modelDeploymentGroupForwardPairMaxWidth
	md := probePairDeployment(workercore.ModelDeploymentEngineSGLang, width, 1, "")
	prefill := probePairView("prefill", 0, probePairMember(0, 0, "uid-p", "8998"))
	decode := probePairView("decode", 0, probePairMember(1, 0, "uid-d", ""))
	fetch := &probePairFetch{prefillHost: probeHost(0, 0)}

	got := observeModelDeploymentGroupForwardPair(context.Background(), md, prefill, decode, fetch.fetch)

	assert.Equal(t, modelDeploymentGroupForwardVerified, got.Prefill.State,
		"the widest group the room can address is probed")
	assert.Len(t, got.Attempt.RequestIDs, width, "every rank of the group is asked")
	assert.Len(t, got.Attempt.BootstrapRooms, width, "every rank of the group has its own room")
	for _, room := range got.Attempt.BootstrapRooms {
		assert.GreaterOrEqual(t, room, int64(0),
			"a room stays inside the range the rank arithmetic reads")
	}
}

// TestGroupForwardProbeNeverCallsADisaggregatedRoleNotApplicable pins the rule that a prefill or a
// decode group is never answered with "there is nothing to verify".
//
// ITS SERVING PATH IS A PAIR, so a group of one member still has a transfer to prove, and saying
// there is nothing to verify would be a claim nobody made an observation for.
func TestGroupForwardProbeNeverCallsADisaggregatedRoleNotApplicable(t *testing.T) {
	for _, kind := range []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode,
	} {
		t.Run(string(kind), func(t *testing.T) {
			md, role := probeDeployment(workercore.ModelDeploymentEngineSGLang, kind)
			fetch := &probeFetch{}
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{probeMember(0, 0, "uid-a")},
			}

			got := observeModelDeploymentGroupForward(
				context.Background(), md, role, view, fetch.fetch)

			assert.NotEqual(t, modelDeploymentGroupForwardNotApplicable, got.State,
				"a disaggregated group always has a transfer to prove")
			assert.Equal(t, modelDeploymentGroupForwardUnsupported, got.State,
				"the single-view entry point cannot answer it, and says so")
			assert.Contains(t, got.Reason, "directed pair probe",
				"the reason names the probe that does answer it")
			assert.Empty(t, fetch.asked, "nothing is asked of a group this call site cannot judge")
		})
	}
}

// TestGroupForwardProbeIdentifierIsFreshPerAttempt pins the identifier's freshness, because an
// identifier that repeats lets an answer captured from an earlier attempt satisfy the binding.
func TestGroupForwardProbeIdentifierIsFreshPerAttempt(t *testing.T) {
	md, role := probeDeployment(workercore.ModelDeploymentEngineVLLM, "")
	view := modelDeploymentReplicaView{
		Role: "server", Ordinal: 0, Seated: true,
		Members: []*core.Pod{
			probeMember(0, 0, "uid-a"),
			probeMember(0, 1, "uid-b"),
		},
	}
	var identifiers []string
	fetch := func(_ context.Context, _ string, body []byte) ([]byte, error) {
		identifiers = append(identifiers, probeRequestRID(body))
		return boundProbeBody(body), nil
	}

	observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch)
	observeModelDeploymentGroupForward(context.Background(), md, role, view, fetch)

	assert.Len(t, identifiers, 4)
	assert.NotEqual(t, identifiers[0], identifiers[2],
		"two attempts on the same member do not share an identifier")
	assert.Contains(t, identifiers[0], "uid-a", "the identifier carries the member it was sent to")
}

// TestGroupForwardProbeHoldsOnAnAbortedFinish pins the finish-reason rule: a request that was
// aborted, or that failed, is not evidence that the group forwarded one.
func TestGroupForwardProbeHoldsOnAnAbortedFinish(t *testing.T) {
	testCases := []struct {
		name   string
		engine string
		answer string
	}{
		{
			name:   "a vLLM member that reports an abort is a hold",
			engine: workercore.ModelDeploymentEngineVLLM,
			answer: `{"id":"cmpl-` + probeRIDPlaceholder +
				`","choices":[{"text":"ok","finish_reason":"abort"}]}`,
		},
		{
			name:   "an SGLang member that reports an abort is a hold",
			engine: workercore.ModelDeploymentEngineSGLang,
			answer: `{"text":"ok","meta_info":{"id":"` + probeRIDPlaceholder +
				`","finish_reason":{"type":"abort"}}}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md, role := probeDeployment(tc.engine, "")
			fetch := &probeFetch{answers: map[string]string{probeHost(0, 1): tc.answer}}
			view := modelDeploymentReplicaView{
				Role: "server", Ordinal: 0, Seated: true,
				Members: []*core.Pod{
					probeMember(0, 0, "uid-a"),
					probeMember(0, 1, "uid-b"),
				},
			}

			got := observeModelDeploymentGroupForward(
				context.Background(), md, role, view, fetch.fetch)

			assert.Equal(t, modelDeploymentGroupForwardUnknown, got.State,
				"a request that was aborted is not evidence that the group forwarded one")
			assert.Contains(t, got.Reason, "did not complete",
				"the reason says the request did not run to completion: %s", got.Reason)
		})
	}
}
