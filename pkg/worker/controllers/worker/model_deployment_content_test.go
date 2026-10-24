package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	"gpustack.ai/gpustack/pkg/utils/json"
)

// THIS FILE CHECKS WHAT THE OPERATOR'S OWN SURFACES SAY ABOUT THE CONTENT THAT COMES BACK.
//
// THE OPERATOR READS ONE KIND OF ANSWER ITSELF: the completion envelope its own probe sent, whose
// binding it checks (model_deployment_group_forward_probe.go). It never assembles a user's event
// stream, never copies a user's text and never parses a user's structured answer, because the
// operator is not in that path: the Router and the engine are.
//
// SO THE FOUR CHECKS BELOW ARE THE FOUR SHAPES A USER'S TRAFFIC TAKES, and the helpers that judge
// them are written here rather than borrowed, because the repository has no such helper. The
// spec's test plan asks for exactly these: helpers that compare assembled event-stream text, exact
// Chinese text and parsed JSON objects.
//
// THE LOCAL DISPATCHER IS A HARNESS, AND IT IS NOT THE REAL ROUTER. It selects the engine Pods by
// the operator's own rendered requirement and answers the content the case asks for, and it refuses
// when the requirement admits nobody. It runs no Router image, no Router binary and no Router
// configuration rendering, so it proves NOTHING about any of them, about a shipped image, about
// real inference, or about Kubernetes. What it does prove is that the operator's own withdrawal and
// recovery change which Pods the operator's own selectors name, and that the four checks
// discriminate as the contract says.

// The four requests the contract names, and the answers the fake engine gives them.
//
// THE ARITHMETIC ANSWER IS A NUMBER AND IS COMPARED AS TEXT, because a model that answered "42
// (forty-two)" would be wrong and the check has to say so. THE CHINESE ANSWER IS COMPARED BYTE
// FOR BYTE, so a full-width character that became its ASCII look-alike, or a character that was
// dropped, is a different answer.
const (
	contentRequestArithmetic = `{"kind": "arithmetic"}`
	contentRequestChinese    = `{"kind": "chinese"}`
	contentRequestStream     = `{"kind": "stream"}`
	contentRequestStructured = `{"kind": "structured"}`

	contentAnswerArithmetic = "42"
	contentAnswerChinese    = "你好，世界"
	contentAnswerStructured = `{"answer": 42, "unit": "widgets"}`
)

// The wrong answers the dispatcher gives when a case asks it to.
//
// EVERY ONE OF THEM IS A REALISTIC MISTAKE: a model that counted wrong, a text that was normalized
// on its way through, a stream that was cut off before it said it was finished, and a number that
// arrived as a string. A control that only refuses nonsense proves nothing, so the controls have
// to refuse these as well.
const (
	contentWrongArithmetic = "41"
	contentWrongChinese    = "你好,世界"
	contentWrongStructured = `{"answer": "forty-two", "unit": "widgets"}`
)

// contentStream is the server-sent event stream the fake engine answers a streaming request with.
//
// IT IS TERMINATED, because a stream that is not terminated carries no complete answer: the
// assembler has to be able to tell the two apart, and it can only do that if the terminated one
// says so.
var contentStream = strings.Join([]string{
	"data: " + `{"choices": [{"delta": {"content": "你"}}]}`,
	"",
	"data: " + `{"choices": [{"delta": {"content": "好"}}]}`,
	"",
	"data: " + `{"choices": [{"delta": {"content": "，世界"}}]}`,
	"",
	"data: [DONE]",
	"",
}, "\n")

// contentTruncatedStream is the same stream cut off before it says it is finished.
//
// IT IS THE PREFIX OF A COMPLETE ANSWER, which is why it is the dangerous one: an assembler that
// did not check for the terminator would return the prefix, and a prefix is a plausible answer.
var contentTruncatedStream = strings.Join([]string{
	"data: " + `{"choices": [{"delta": {"content": "你"}}]}`,
	"",
	"data: " + `{"choices": [{"delta": {"content": "好"}}]}`,
	"",
}, "\n")

// assembleSSEString reads a server-sent event stream and returns the content it carried.
//
// IT FAILS WHEN THE STREAM DID NOT TERMINATE. A stream that stops early concatenates into a PREFIX
// of the answer it was carrying, and a prefix is not an answer: without this check a truncated
// response would pass as a complete one.
func assembleSSEString(raw []byte) (string, error) {
	var content strings.Builder
	terminated := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			return "", fmt.Errorf("the stream carries a line that is not a data line: %q", line)
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			terminated = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return "", fmt.Errorf("the stream carries a chunk that is not JSON: %w", err)
		}
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
		}
	}
	if !terminated {
		return "", fmt.Errorf("the stream did not terminate")
	}

	return content.String(), nil
}

// chatCompletionContent reads a chat completion and returns the content it carried.
func chatCompletionContent(raw []byte) (string, error) {
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return "", fmt.Errorf("the answer is not a chat completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("the answer carries no choice")
	}

	return completion.Choices[0].Message.Content, nil
}

// parseStructuredAnswer reads a structured answer and returns the object it carried.
//
// THE COMPARISON IS ON THE OBJECT, NOT ON THE BYTES: two answers that say the same thing in
// different orders are the same answer, and a check that compared the bytes would call them
// different.
func parseStructuredAnswer(raw []byte) (map[string]any, error) {
	answer := map[string]any{}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("the answer is not a structured object: %w", err)
	}

	return answer, nil
}

// The four checks. EACH ONE EITHER ACCEPTS AN ANSWER OR REFUSES IT, so a case can ask the same
// question of a good answer and of a wrong one and compare the two outcomes.

// checkArithmeticAnswer accepts an answer that is the number itself, and refuses one that is not.
//
// THE COMPARISON IS ON THE WHOLE CONTENT, so "42 (forty-two)" is refused: the contract names the
// number, and an answer that carries something beside it is a different answer.
func checkArithmeticAnswer(raw []byte) error {
	content, err := chatCompletionContent(raw)
	if err != nil {
		return err
	}
	if content != contentAnswerArithmetic {
		return fmt.Errorf("the answer is %q and not the number the contract names", content)
	}

	return nil
}

// checkChineseAnswer accepts an answer that is the exact text, and refuses one that is not.
//
// THE COMPARISON IS BYTE FOR BYTE, so a full-width character that became its ASCII look-alike is
// refused, and so is a character that was dropped.
func checkChineseAnswer(raw []byte) error {
	content, err := chatCompletionContent(raw)
	if err != nil {
		return err
	}
	if content != contentAnswerChinese {
		return fmt.Errorf("the answer is %q and not the exact text the contract names", content)
	}

	return nil
}

// checkStreamAnswer accepts a complete event stream and refuses an incomplete one.
//
// THE COMPLETENESS IS CHECKED FIRST, because a truncated stream assembles into a plausible prefix
// and a prefix would otherwise be accepted as the answer.
func checkStreamAnswer(raw []byte) error {
	content, err := assembleSSEString(raw)
	if err != nil {
		return err
	}
	if content != contentAnswerChinese {
		return fmt.Errorf("the assembled stream is %q and not the exact text the contract names", content)
	}

	return nil
}

// checkStructuredAnswer accepts a structured answer that carries the named fields, and refuses one
// that does not.
//
// THE FIELDS ARE COMPARED AS VALUES AND NOT AS BYTES, so an answer that says the same thing in a
// different order is the same answer.
func checkStructuredAnswer(raw []byte) error {
	answer, err := parseStructuredAnswer(raw)
	if err != nil {
		return err
	}
	value, present := answer["answer"]
	if !present {
		return fmt.Errorf("the answer carries no number")
	}
	number, isNumber := value.(float64)
	if !isNumber {
		return fmt.Errorf("the answer carries %T where the contract names a number", value)
	}
	if number != 42 {
		return fmt.Errorf("the answer carries %v where the contract names 42", number)
	}
	unit, present := answer["unit"]
	if !present {
		return fmt.Errorf("the answer carries no unit")
	}
	if unit != "widgets" {
		return fmt.Errorf("the answer carries %q where the contract names widgets", unit)
	}

	return nil
}

// contentDispatcher is the local stand-in for the path a request takes.
//
// IT IS A LOCAL, SELECTOR-DRIVEN DISPATCHER. It selects the engine Pods by the operator's own
// rendered requirement, read at request time, so a case that withdraws the group sees a refusal
// and a case that restores it sees the answer.
//
// IT IS NOT THE REAL ROUTER SOFTWARE. It runs no Router image, no Router binary and no Router
// configuration rendering, and it proves nothing about any of them.
type contentDispatcher struct {
	server *httptest.Server
	// wrong makes the dispatcher answer wrongly, so a case can ask the same checks to refuse a
	// bad answer at a moment when the operator's surfaces are still serving.
	//
	// IT IS ATOMIC BECAUSE THE HANDLER READS IT FROM ANOTHER GOROUTINE.
	wrong atomic.Bool
}

// newContentDispatcher stands the local dispatcher in front of the fixture's members.
func newContentDispatcher(t *testing.T, f *contractFixture) *contentDispatcher {
	t.Helper()

	d := &contentDispatcher{}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// THE REQUIREMENT IS READ AT REQUEST TIME, so the dispatcher sees the operator's CURRENT
		// decision rather than a copy taken when the case set it up.
		if len(f.selectedNow()) == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch strings.TrimSpace(string(body)) {
		case contentRequestArithmetic:
			w.Header().Set("Content-Type", "application/json")
			answer := contentAnswerArithmetic
			if d.wrong.Load() {
				answer = contentWrongArithmetic
			}
			_, _ = w.Write([]byte(`{"choices": [{"message": {"content": "` + answer + `"}}]}`))
		case contentRequestChinese:
			w.Header().Set("Content-Type", "application/json")
			answer := contentAnswerChinese
			if d.wrong.Load() {
				answer = contentWrongChinese
			}
			_, _ = w.Write([]byte(`{"choices": [{"message": {"content": "` + answer + `"}}]}`))
		case contentRequestStream:
			w.Header().Set("Content-Type", "text/event-stream")
			stream := contentStream
			if d.wrong.Load() {
				stream = contentTruncatedStream
			}
			_, _ = w.Write([]byte(stream))
		case contentRequestStructured:
			w.Header().Set("Content-Type", "application/json")
			answer := contentAnswerStructured
			if d.wrong.Load() {
				answer = contentWrongStructured
			}
			_, _ = w.Write([]byte(answer))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(d.server.Close)

	return d
}

// post sends one request to the dispatcher and returns the status and body it answered with.
func (d *contentDispatcher) post(t *testing.T, request string) (int, []byte) {
	t.Helper()

	response, err := http.Post(d.server.URL, "application/json", strings.NewReader(request))
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	return response.StatusCode, body
}

// selectedNow is the set of members the requirement admits, without an assertion, so a request
// handler may call it from its own goroutine.
//
// IT IS THE INTERSECTION OVER THE SURFACES THAT CARRY THE REQUIREMENT, because a request that
// arrives at any of them has to find the same group: a member that only one surface admits would
// be reachable on one front and not on the other.
func (f *contractFixture) selectedNow() []string {
	services := new(corev1.ServiceList)
	if err := f.cli.List(context.Background(), services, ctrlcli.InNamespace(f.md.Namespace)); err != nil {
		return nil
	}
	// THE PODS ARE LISTED HERE RATHER THAN THROUGH replicaPods, because that helper asserts on
	// the test's own goroutine and this runs on the handler's.
	pods := new(corev1.PodList)
	if err := f.cli.List(context.Background(), pods, ctrlcli.InNamespace(f.md.Namespace)); err != nil {
		return nil
	}
	live := []corev1.Pod{}
	for i := range pods.Items {
		if _, isRouter := pods.Items[i].Labels[modelDeploymentRouterLabelKey]; isRouter {
			continue
		}
		live = append(live, pods.Items[i])
	}

	selected := []string{}
	first := true
	for i := range services.Items {
		svc := &services.Items[i]
		if svc.Spec.ClusterIP == corev1.ClusterIPNone {
			continue
		}
		if svc.Spec.Selector[modelDeploymentLabelKeyEndpointEligible] == "" {
			continue
		}
		requirement := labels.SelectorFromSet(svc.Spec.Selector)
		admitted := []string{}
		for _, pod := range live {
			if requirement.Matches(labels.Set(pod.Labels)) {
				admitted = append(admitted, string(pod.UID))
			}
		}
		if first {
			selected = admitted
			first = false
			continue
		}
		// THE INTERSECTION KEEPS ONLY THE MEMBERS EVERY SURFACE ADMITS.
		kept := []string{}
		for _, uid := range selected {
			for _, other := range admitted {
				if uid == other {
					kept = append(kept, uid)
					break
				}
			}
		}
		selected = kept
	}

	return selected
}

// reportRankNotReady reports one rank of one group as not ready, which is a definite fault: the
// operator has been told the rank is not ready, so the group is withdrawn.
func reportRankNotReady(t *testing.T, f *contractFixture, ordinal int, index string) {
	t.Helper()

	for _, pod := range f.group(t, ordinal) {
		if pod.Labels[modelDeploymentMemberIndexLabel] != index {
			continue
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		require.NoError(t, f.cli.Status().Update(context.Background(), pod))
	}
}

// reportRankReady reports one rank of one group as ready again.
func reportRankReady(t *testing.T, f *contractFixture, ordinal int, index string) {
	t.Helper()

	for _, pod := range f.group(t, ordinal) {
		if pod.Labels[modelDeploymentMemberIndexLabel] != index {
			continue
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		require.NoError(t, f.cli.Status().Update(context.Background(), pod))
	}
}

// contentPhase is one moment of the withdrawal and recovery sequence.
//
// IT IS DATA AND NOT BEHAVIOR: the case says which moment it is and whether the second group's
// follower is faulted at that moment, and the loop below does the work, so the three moments
// cannot drift apart in how they are driven.
type contentPhase struct {
	name string
	// faulted says whether the second group's follower is reported not ready at this moment.
	faulted bool
}

// contentPhases are the three moments the contract names, in the order they happen.
//
// THE FIRST AND THE THIRD LOOK ALIKE AND ARE NOT: the third is reached only after the second has
// withdrawn the group, so it is the restoration and not the beginning.
var contentPhases = []contentPhase{
	{name: "before the fault", faulted: false},
	{name: "while an unaffected group serves", faulted: true},
	{name: "after the recovery", faulted: false},
}

// TestModelDeploymentContentContract checks the four content contracts against the operator's own
// surfaces, at the three moments the contract names.
//
// THE MOMENTS ARE BEFORE THE FAULT, WHILE AN UNAFFECTED GROUP IS SERVING, AND AFTER THE
// RECOVERY. The requests are the same at each of them, and the answer is the same at each of them,
// because the operator's withdrawal is about the group and not about the content.
//
// AT EVERY MOMENT THE WRONG ANSWER IS REFUSED AS WELL, because a check that only ever saw the
// right answer would not be a check.
func TestModelDeploymentContentContract(t *testing.T) {
	fixture := newContractFixture(t, 2, false)
	*fixture.serving = fixture.answering(false)
	fixture.setAllMembersReady(t, true)
	fixture.contractPass(t)

	// THE SECOND GROUP IS BESIDE THE FIRST, SO ONE OF THEM CAN SERVE WHILE THE OTHER FAULTS.
	fixture.addReplica(t)
	fixture.contractPass(t)
	assignEndpointEligibilityUIDs(t, fixture.cli)
	fixture.makeMembersBindable(t)
	fixture.setAllMembersReady(t, true)
	*fixture.serving = fixture.answering(false)
	fixture.contractPass(t)

	dispatcher := newContentDispatcher(t, fixture)

	for _, phase := range contentPhases {
		t.Run(phase.name, func(t *testing.T) {
			if phase.faulted {
				reportRankNotReady(t, fixture, 1, "1")
			} else {
				reportRankReady(t, fixture, 1, "1")
			}
			fixture.contractPass(t)

			// THE REQUIREMENT ADMITS SOMEBODY AT EVERY MOMENT, because the first group is not
			// the one that faults.
			require.NotEmpty(t, fixture.selectedBySurfaces(t),
				"the requirement admits a member at %s", phase.name)

			// THE GOOD ANSWERS ARE ACCEPTED.
			dispatcher.wrong.Store(false)
			assertContentAnswers(t, dispatcher, phase.name)

			// AND THE WRONG ONES ARE REFUSED, WHILE THE SURFACES ARE STILL SERVING.
			dispatcher.wrong.Store(true)
			assertContentAnswersRefused(t, dispatcher, phase.name)
			dispatcher.wrong.Store(false)
		})
	}
}

// assertContentAnswers runs the four requests and checks that the four answers are accepted.
func assertContentAnswers(t *testing.T, dispatcher *contentDispatcher, moment string) {
	t.Helper()

	status, body := dispatcher.post(t, contentRequestArithmetic)
	require.Equal(t, http.StatusOK, status, "the arithmetic request is answered at %s", moment)
	assert.NoError(t, checkArithmeticAnswer(body),
		"the arithmetic answer is the number itself at %s", moment)

	status, body = dispatcher.post(t, contentRequestChinese)
	require.Equal(t, http.StatusOK, status, "the Chinese request is answered at %s", moment)
	assert.NoError(t, checkChineseAnswer(body),
		"the Chinese answer is the exact text at %s", moment)

	status, body = dispatcher.post(t, contentRequestStream)
	require.Equal(t, http.StatusOK, status, "the streaming request is answered at %s", moment)
	assert.NoError(t, checkStreamAnswer(body),
		"the assembled stream is the exact text at %s", moment)

	status, body = dispatcher.post(t, contentRequestStructured)
	require.Equal(t, http.StatusOK, status, "the structured request is answered at %s", moment)
	assert.NoError(t, checkStructuredAnswer(body),
		"the structured answer carries its fields at %s", moment)
}

// assertContentAnswersRefused runs the four requests and checks that the four wrong answers are
// refused.
//
// THE SURFACE STILL SERVES, so the refusal is about the CONTENT and not about the operator having
// withdrawn the group. That distinction is the whole point: a refusal caused by an absent surface
// would pass this arm whatever the checks did.
func assertContentAnswersRefused(t *testing.T, dispatcher *contentDispatcher, moment string) {
	t.Helper()

	status, body := dispatcher.post(t, contentRequestArithmetic)
	require.Equal(t, http.StatusOK, status, "the surface still answers at %s", moment)
	assert.Error(t, checkArithmeticAnswer(body),
		"the wrong arithmetic answer is refused at %s", moment)

	status, body = dispatcher.post(t, contentRequestChinese)
	require.Equal(t, http.StatusOK, status, "the surface still answers at %s", moment)
	assert.Error(t, checkChineseAnswer(body),
		"the Chinese answer that lost its full-width character is refused at %s", moment)

	status, body = dispatcher.post(t, contentRequestStream)
	require.Equal(t, http.StatusOK, status, "the surface still answers at %s", moment)
	assert.Error(t, checkStreamAnswer(body),
		"the truncated stream is refused at %s", moment)

	status, body = dispatcher.post(t, contentRequestStructured)
	require.Equal(t, http.StatusOK, status, "the surface still answers at %s", moment)
	assert.Error(t, checkStructuredAnswer(body),
		"the structured answer whose number is a string is refused at %s", moment)
}

// TestModelDeploymentContentNeedsAnEligibleMember is the witness that the content checks are bound
// to the operator's own decision and not to the harness's constants.
//
// IT IS A WITNESS, NOT A CASE ABOUT CONTENT. The same request is sent before and after the
// operator withdraws the group: the first is answered, and the second is refused by the surface
// because the operator's requirement admits nobody. A harness that answered regardless would make
// the content checks vacuous, and this is what says it does not.
func TestModelDeploymentContentNeedsAnEligibleMember(t *testing.T) {
	fixture := newContractFixture(t, 2, false)
	*fixture.serving = fixture.answering(false)
	fixture.setAllMembersReady(t, true)
	fixture.contractPass(t)

	dispatcher := newContentDispatcher(t, fixture)

	// THE GROUP IS QUALIFIED, SO THE REQUIREMENT ADMITS ITS LEADER.
	require.NotEmpty(t, fixture.selectedBySurfaces(t), "the qualified group is admitted")
	status, body := dispatcher.post(t, contentRequestArithmetic)
	require.Equal(t, http.StatusOK, status, "the request is answered")
	assert.NoError(t, checkArithmeticAnswer(body), "and the answer is accepted")

	// THE GROUP IS WITHDRAWN, AND NOTHING ELSE IS SERVING.
	reportRankNotReady(t, fixture, 0, "1")
	fixture.contractPass(t)
	require.Empty(t, fixture.selectedBySurfaces(t), "the withdrawn group is admitted by nobody")

	// THE SAME REQUEST IS NOW REFUSED, BECAUSE THERE IS NO ANSWER TO CHECK.
	status, body = dispatcher.post(t, contentRequestArithmetic)
	assert.Equal(t, http.StatusServiceUnavailable, status,
		"the surface refuses when the operator's requirement admits nobody")
	assert.Error(t, checkArithmeticAnswer(body), "and there is no answer to check")
}

// TestModelDeploymentContentRejectsMalformedAnswers is the negative side of the four checks, on
// answers that never reach the dispatcher.
//
// EVERY CASE HERE IS AN ANSWER THE CONTRACT MUST REFUSE, AND EVERY CASE MUST FAIL ITS CHECK. A
// case that cannot fail is not a control, so each one is checked against a check that would have
// accepted it.
func TestModelDeploymentContentRejectsMalformedAnswers(t *testing.T) {
	t.Run("a truncated stream is not a complete answer", func(t *testing.T) {
		content, err := assembleSSEString([]byte(contentTruncatedStream))
		require.Error(t, err, "a stream that did not terminate is refused")
		assert.Empty(t, content, "and it yields no content at all")
		assert.NotEqual(t, contentAnswerChinese, content,
			"a prefix of the answer is not the answer")
		assert.Error(t, checkStreamAnswer([]byte(contentTruncatedStream)),
			"and the check refuses it")
	})
	t.Run("a stream carrying something that is not JSON is refused", func(t *testing.T) {
		_, err := assembleSSEString([]byte("data: not json\n\ndata: [DONE]\n"))
		require.Error(t, err, "a chunk that is not JSON is refused")
		assert.Error(t, checkStreamAnswer([]byte("data: not json\n\ndata: [DONE]\n")),
			"and the check refuses it")
	})

	t.Run("a stream carrying a line that is not a data line is refused", func(t *testing.T) {
		_, err := assembleSSEString([]byte("event: message\ndata: [DONE]\n"))
		require.Error(t, err, "a line that is not a data line is refused")
		assert.Error(t, checkStreamAnswer([]byte("event: message\ndata: [DONE]\n")),
			"and the check refuses it")
	})

	t.Run("an answer that is not a chat completion is refused", func(t *testing.T) {
		_, err := chatCompletionContent([]byte("not a completion"))
		require.Error(t, err, "an answer that is not a chat completion is refused")
		assert.Error(t, checkArithmeticAnswer([]byte("not a completion")),
			"and the check refuses it")
		assert.Error(t, checkChineseAnswer([]byte("not a completion")),
			"and the check refuses it")
	})

	t.Run("an answer with no choice is refused", func(t *testing.T) {
		_, err := chatCompletionContent([]byte(`{"choices": []}`))
		require.Error(t, err, "an answer with no choice is refused")
		assert.Error(t, checkArithmeticAnswer([]byte(`{"choices": []}`)),
			"and the check refuses it")
	})

	t.Run("a structured answer that is not an object is refused", func(t *testing.T) {
		_, err := parseStructuredAnswer([]byte("not an object"))
		require.Error(t, err, "a structured answer that is not an object is refused")
		assert.Error(t, checkStructuredAnswer([]byte("not an object")),
			"and the check refuses it")
	})

	t.Run("the wrong arithmetic answer is refused", func(t *testing.T) {
		wrong := `{"choices": [{"message": {"content": "41"}}]}`
		content, err := chatCompletionContent([]byte(wrong))
		require.NoError(t, err, "the wrong answer is still a chat completion")
		assert.NotEqual(t, contentAnswerArithmetic, content,
			"and it is not the number the contract names")
		assert.Error(t, checkArithmeticAnswer([]byte(wrong)),
			"and the check refuses it")
	})

	t.Run("the right number with a trailing character is refused", func(t *testing.T) {
		// THE NUMBER IS RIGHT AND THE ANSWER IS WRONG, because the answer carries something the
		// contract does not name. A check that read the number out of the content and stopped
		// there would accept this one.
		trailing := `{"choices": [{"message": {"content": "42 "}}]}`
		content, err := chatCompletionContent([]byte(trailing))
		require.NoError(t, err, "the answer is still a chat completion")
		require.NotEqual(t, contentAnswerArithmetic, content,
			"the trailing character makes it a different answer")
		assert.Error(t, checkArithmeticAnswer([]byte(trailing)),
			"and the check refuses it")
	})

	t.Run("a Chinese answer with one character altered is refused", func(t *testing.T) {
		altered := strings.Replace(contentAnswerChinese, "好", "号", 1)
		require.NotEqual(t, contentAnswerChinese, altered, "the altered answer differs")
		assert.NotEqual(t, contentAnswerChinese, altered,
			"and it is not the exact text the contract names")
		assert.Error(t, checkChineseAnswer([]byte(`{"choices": [{"message": {"content": "`+altered+`"}}]}`)),
			"and the check refuses it")
	})

	t.Run("a Chinese answer with a full-width character replaced by its look-alike is refused",
		func(t *testing.T) {
			altered := strings.Replace(contentAnswerChinese, "，", ",", 1)
			require.NotEqual(t, contentAnswerChinese, altered, "the altered answer differs")
			assert.NotEqual(t, contentAnswerChinese, altered,
				"and it is not the exact text the contract names")
			assert.Error(t, checkChineseAnswer([]byte(`{"choices": [{"message": {"content": "`+altered+`"}}]}`)),
				"and the check refuses it")
		})

	t.Run("a Chinese answer with a character dropped is refused", func(t *testing.T) {
		dropped := strings.Replace(contentAnswerChinese, "好", "", 1)
		require.NotEqual(t, contentAnswerChinese, dropped, "the dropped answer differs")
		assert.NotEqual(t, contentAnswerChinese, dropped,
			"and it is not the exact text the contract names")
		assert.Error(t, checkChineseAnswer([]byte(`{"choices": [{"message": {"content": "`+dropped+`"}}]}`)),
			"and the check refuses it")
	})

	t.Run("a structured answer with a field of the wrong type is refused", func(t *testing.T) {
		answer, err := parseStructuredAnswer([]byte(contentWrongStructured))
		require.NoError(t, err, "the answer is an object")
		assert.NotEqual(t, float64(42), answer["answer"],
			"and its number is not the number the contract names")
		assert.Error(t, checkStructuredAnswer([]byte(contentWrongStructured)),
			"and the check refuses it")
	})

	t.Run("a structured answer with a field missing is refused", func(t *testing.T) {
		answer, err := parseStructuredAnswer([]byte(`{"answer": 42}`))
		require.NoError(t, err, "the answer is an object")
		_, present := answer["unit"]
		assert.False(t, present, "and it does not carry the unit the contract names")
		assert.Error(t, checkStructuredAnswer([]byte(`{"answer": 42}`)),
			"and the check refuses it")
	})

	t.Run("a structured answer with a wrong value of the right type is refused", func(t *testing.T) {
		// THE TYPES ARE RIGHT AND THE VALUES ARE WRONG, so a check that only looked at the
		// shape of the object would accept this one.
		wrongNumber := `{"answer": 41, "unit": "widgets"}`
		answer, err := parseStructuredAnswer([]byte(wrongNumber))
		require.NoError(t, err, "the answer is an object")
		require.IsType(t, float64(0), answer["answer"],
			"and its number is a number, as the contract names")
		assert.NotEqual(t, float64(42), answer["answer"],
			"but it is not the number the contract names")
		assert.Error(t, checkStructuredAnswer([]byte(wrongNumber)),
			"and the check refuses it")

		wrongUnit := `{"answer": 42, "unit": "gadgets"}`
		answer, err = parseStructuredAnswer([]byte(wrongUnit))
		require.NoError(t, err, "the answer is an object")
		require.IsType(t, "", answer["unit"], "and its unit is a string, as the contract names")
		assert.NotEqual(t, "widgets", answer["unit"],
			"but it is not the unit the contract names")
		assert.Error(t, checkStructuredAnswer([]byte(wrongUnit)),
			"and the check refuses it")
	})
}
