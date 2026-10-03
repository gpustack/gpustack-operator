package elasticengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const (
	// completionPath is the native OpenAI-compatible completion route. The observer uses it
	// because the pinned frontend validates the data parallel rank header there.
	completionPath = "/v1/completions"
	// rankHeader is the native header the pinned frontend reads the rank from.
	rankHeader = "X-data-parallel-rank"
	// sentinelRank is outside every nonzero world, so asking for it makes the frontend
	// state the bound it currently enforces.
	sentinelRank = -1
	// maxObservedWorldWidth bounds how many rank forwards one observation may make. An
	// untrusted answer never decides how much probe traffic this client generates.
	maxObservedWorldWidth = 64
	// forwardTokens is the one token each rank forward asks for.
	forwardTokens = 1
)

// boundaryPrefix and boundarySuffix frame the bound in the pinned frontend's out-of-range
// message: "data_parallel_rank -1 is out of range [0, N).".
const (
	boundaryPrefix = "data_parallel_rank -1 is out of range [0, "
	boundarySuffix = ")."
)

// RankForward records one rank's directed forward. It is evidence that the rank served, not
// evidence of what identity backs it.
type RankForward struct {
	Rank   int
	Served bool
}

// NativeWorldObservation is the measured result of the native observation protocol. It carries
// what the engine's frontend enforced and which ranks actually served a one-token completion.
//
// It says nothing about which Pod or actor holds a rank, and equal boundary reads around the
// forwards cannot exclude an ABA change across them. It is an input to a controller's decision,
// never a deletion authority by itself.
type NativeWorldObservation struct {
	// ObservedWidth is the bound the frontend enforced on both boundary reads.
	ObservedWidth int
	// Boundaries are the two boundary reads, in call order. They are equal by construction of
	// a successful observation and retained so a caller can see the bookends.
	Boundaries [2]int
	// Forwards are the per-rank results, one per rank in [0, ObservedWidth).
	Forwards []RankForward
}

// observeRequest is the bounded completion body the observer sends.
type observeRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
}

// ObserveNativeWorld measures the engine's native world: a rank -1 boundary read states the
// enforced bound, one real one-token forward per rank proves each rank serves, and a second
// boundary read bookends the forwards.
//
// The caller supplies the expected width. The observation is refused unless both boundary reads
// agree with each other and with it, so an untrusted answer can neither redirect the probe
// traffic nor widen it: the forwards never run unless the bound equals the expected width
// first. The client's timeout bounds the whole observation and honors an earlier caller
// deadline. Nothing here retries, and the mutation routes are never sent.
//
// The returned observation is native evidence only. The caller must pair it with a complete Ray
// rank-to-member mapping and its own uncached identity reads before acting on it.
func (c *Client) ObserveNativeWorld(
	ctx context.Context, model, prompt string, expectedWidth int,
) (*NativeWorldObservation, *Error) {
	const op = "observe native world"

	if expectedWidth < 1 || expectedWidth > maxObservedWorldWidth {
		return nil, invalid(op,
			"the expected width %d is outside the bounded range [1, %d]",
			expectedWidth, maxObservedWorldWidth)
	}
	if model == "" {
		return nil, invalid(op, "the model name is required")
	}

	// One bound for the whole observation: the constructor timeout and any earlier caller
	// deadline both apply, shared by every boundary read and forward, so a slow answer or a
	// wide world cannot stretch the observation past what the caller allowed.
	//
	// That budget covers expectedWidth + 2 requests, so the caller sizes it for the width it
	// expects rather than for one request: Scale and IsScaling are given the whole bound alone,
	// while a world of expectedWidth ranks divides the same bound across every rank's forward.
	// A bound reached part way through is reported as KindAmbiguous, so a caller that wants to
	// tell exhaustion from a stall owns the width it asks for and the budget it passes.
	obsCtx, cancel := c.callContext(ctx)
	defer cancel()

	// The observer follows no redirect: a redirect is an answer this protocol does not model,
	// not a page to follow.
	httpc := *c.http
	httpc.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	httpc.Timeout = 0 // the shared observation bound is the only timeout here

	payload := observeRequest{
		Model:     model,
		Prompt:    prompt,
		MaxTokens: forwardTokens,
		Stream:    false,
	}

	bound := func() (int, *Error) {
		status, raw, err := c.observePost(obsCtx, &httpc, op, sentinelRank, payload)
		if err != nil {
			return 0, err
		}
		if status == http.StatusOK {
			return 0, &Error{
				Kind: KindMalformed, Op: op, StatusCode: status,
				Detail: "the engine answered the out-of-range probe with 200, so the rank header is not enforced; the bound cannot be observed",
			}
		}
		if status != http.StatusBadRequest {
			return 0, probeStatusFailure(op, status,
				"the engine did not answer the out-of-range probe with the native validation error")
		}
		width, perr := parseObservedBound(raw)
		if perr != nil {
			return 0, &Error{Kind: KindMalformed, Op: op, StatusCode: status, Detail: perr.Error()}
		}
		if width != expectedWidth {
			return 0, &Error{
				Kind: KindMalformed, Op: op, StatusCode: status,
				Detail: fmt.Sprintf("the observed native width %d does not match the expected width %d", width, expectedWidth),
			}
		}
		return width, nil
	}

	first, err := bound()
	if err != nil {
		return nil, err
	}

	forwards := make([]RankForward, 0, first)
	for rank := 0; rank < first; rank++ {
		if serr := c.observeForward(obsCtx, &httpc, op, rank, payload); serr != nil {
			return nil, serr
		}
		forwards = append(forwards, RankForward{Rank: rank, Served: true})
	}

	second, err := bound()
	if err != nil {
		return nil, err
	}

	return &NativeWorldObservation{
		ObservedWidth: first,
		Boundaries:    [2]int{first, second},
		Forwards:      forwards,
	}, nil
}

// observePost sends one completion probe with the rank header and returns the status and the
// bounded body. Every non-2xx status is an answer here, because the boundary read is defined by
// the engine's rejection; classification happens at the call sites.
func (c *Client) observePost(
	ctx context.Context, httpc *http.Client, op string, rank int, payload observeRequest,
) (int, []byte, *Error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, &Error{
			Kind: KindInvalidInput, Op: op,
			Detail: "the probe body could not be encoded", Err: err,
		}
	}

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+completionPath, strings.NewReader(string(encoded)))
	if err != nil {
		return 0, nil, &Error{
			Kind: KindInvalidInput, Op: op,
			Detail: "the probe request could not be built", Err: err,
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set(rankHeader, strconv.Itoa(rank))

	response, err := httpc.Do(request)
	if err != nil {
		kind := KindAmbiguous
		detail := "the probe could not be completed"
		if ctxErr := ctx.Err(); ctxErr != nil {
			detail = "the observer's bound was reached"
		}
		return 0, nil, &Error{Kind: kind, Op: op, Detail: detail, Err: err}
	}
	defer response.Body.Close()

	raw, rerr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if rerr != nil {
		return 0, nil, &Error{
			Kind: KindAmbiguous, Op: op,
			Detail: "the probe answer could not be read to its end", Err: rerr,
		}
	}
	if len(raw) > maxResponseBytes {
		return 0, nil, &Error{
			Kind: KindMalformed, Op: op, StatusCode: response.StatusCode,
			Detail: fmt.Sprintf("the probe answer exceeded the %d byte bound", maxResponseBytes),
		}
	}
	return response.StatusCode, raw, nil
}

// probeStatusFailure keeps the client's standing status classification on a probe answer the
// protocol cannot use: the engine's 503 stays reconfiguring, its 408/5xx stay native failures,
// and everything else is an answer this protocol does not model. The detail is the caller's.
func probeStatusFailure(op string, status int, detail string) *Error {
	switch {
	case status == http.StatusServiceUnavailable:
		return &Error{Kind: KindReconfiguring, Op: op, StatusCode: status, Detail: detail}
	case status == http.StatusRequestTimeout || status >= http.StatusInternalServerError:
		return &Error{Kind: KindNativeFailure, Op: op, StatusCode: status, Detail: detail}
	default:
		return &Error{Kind: KindMalformed, Op: op, StatusCode: status, Detail: detail}
	}
}

// parseObservedBound reads the width out of the pinned frontend's exact out-of-range message.
// The native schema is enforced exactly — the four producer fields and no others, param
// required and null, bound a canonical positive decimal — and any other shape is refused. No
// response content is ever echoed into an error: details are static classifications.
func parseObservedBound(raw []byte) (int, error) {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error == nil {
		return 0, fmt.Errorf("the probe answer is not the native error object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Error, &fields); err != nil {
		return 0, fmt.Errorf("the probe answer is not the native error object")
	}
	if len(fields) != 4 {
		return 0, fmt.Errorf("the probe answer is not the native bad-request shape")
	}
	var code int
	var messageType, message string
	for name, dst := range map[string]any{"code": &code, "type": &messageType, "message": &message} {
		field, ok := fields[name]
		if !ok || json.Unmarshal(field, dst) != nil {
			return 0, fmt.Errorf("the probe answer is not the native bad-request shape")
		}
	}
	paramRaw, hasParam := fields["param"]
	if !hasParam || string(paramRaw) != "null" {
		return 0, fmt.Errorf("the probe answer is not the native bad-request shape")
	}
	if code != http.StatusBadRequest || messageType != "BadRequestError" {
		return 0, fmt.Errorf("the probe answer is not the native bad-request shape")
	}
	if !strings.HasPrefix(message, boundaryPrefix) || !strings.HasSuffix(message, boundarySuffix) {
		return 0, fmt.Errorf("the probe answer does not carry the native out-of-range message")
	}
	widthText := strings.TrimSuffix(strings.TrimPrefix(message, boundaryPrefix), boundarySuffix)
	if !canonicalWidth.MatchString(widthText) {
		return 0, fmt.Errorf("the probe answer's bound is not a canonical positive width")
	}
	width, err := strconv.Atoi(widthText)
	if err != nil || width < 1 {
		return 0, fmt.Errorf("the probe answer's bound is not a positive width")
	}
	return width, nil
}

// canonicalWidth accepts exactly what the frontend's %d format prints for a positive width.
var canonicalWidth = regexp.MustCompile(`^[1-9][0-9]*$`)

// observeForward proves one rank serves: a real one-token completion with that rank's header.
func (c *Client) observeForward(
	ctx context.Context, httpc *http.Client, op string, rank int, payload observeRequest,
) *Error {
	status, raw, err := c.observePost(ctx, httpc, op, rank, payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return probeStatusFailure(op, status,
			fmt.Sprintf("rank %d did not serve the forward", rank))
	}
	var body struct {
		Object  string            `json:"object"`
		Choices []json.RawMessage `json:"choices"`
		Error   *json.RawMessage  `json:"error"`
		Usage   struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if jsonErr := json.Unmarshal(raw, &body); jsonErr != nil {
		return &Error{
			Kind: KindMalformed, Op: op, StatusCode: status,
			Detail: fmt.Sprintf("rank %d's answer is not a completion object", rank),
		}
	}
	// The pinned producer's non-streaming completion shape: object typed, at least one choice
	// that is an object carrying text (which may legitimately be empty or whitespace), no
	// error object, and exactly one completion token in the usage.
	notCompletion := fmt.Sprintf("rank %d's answer is not the native completion shape", rank)
	if body.Error != nil || body.Object != "text_completion" || len(body.Choices) == 0 {
		return &Error{Kind: KindMalformed, Op: op, StatusCode: status, Detail: notCompletion}
	}
	var choice struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(body.Choices[0], &choice) != nil || choice.Text == nil {
		return &Error{Kind: KindMalformed, Op: op, StatusCode: status, Detail: notCompletion}
	}
	if body.Usage.CompletionTokens != forwardTokens {
		return &Error{Kind: KindMalformed, Op: op, StatusCode: status, Detail: notCompletion}
	}
	return nil
}
