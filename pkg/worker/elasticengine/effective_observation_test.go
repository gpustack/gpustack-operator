package elasticengine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpustack.ai/gpustack/pkg/worker/elasticprofile"
)

// boundaryBody is the pinned frontend's exact out-of-range answer for a width of n.
func boundaryBody(n int) string {
	return fmt.Sprintf(
		`{"error":{"message":"data_parallel_rank -1 is out of range [0, %d).","type":"BadRequestError","param":null,"code":400}}`,
		n,
	)
}

// forwardBody is a minimal real completion answer for one served token.
const forwardBody = `{"id":"cmpl-1","object":"text_completion","choices":[{"index":0,"text":" "}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

type probeServer struct {
	requests  int
	boundBody func(seq int) (int, string) // status, body per boundary read
	forward   func(seq int, rank string) (int, string)
	forwards  []string
	bodies    []struct {
		path   string
		method string
		rank   string
		body   string
	}
}

func (p *probeServer) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests++
		rank := r.Header.Get(rankHeader)
		body := readAll(t, r)
		p.bodies = append(p.bodies, struct {
			path, method, rank, body string
		}{r.URL.Path, r.Method, rank, body})

		if rank == "-1" {
			status, out := p.boundBody(len(p.forwards))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(out))
			return
		}
		p.forwards = append(p.forwards, rank)
		status, out := p.forward(len(p.forwards)-1, rank)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	})
}

func readAll(t *testing.T, r *http.Request) string {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("request body unreadable: %v", err)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return string(raw)
	}
	encoded, _ := json.Marshal(obj)
	return string(encoded)
}

func fixedBound(status int, body string) func(int) (int, string) {
	return func(int) (int, string) { return status, body }
}

func fixedForward(status int, body string) func(int, string) (int, string) {
	return func(int, string) (int, string) { return status, body }
}

func observe(t *testing.T, server *httptest.Server, expected int) (*NativeWorldObservation, *Error) {
	t.Helper()
	client, cerr := New(server.URL, 2*time.Second)
	if cerr != nil {
		t.Fatalf("client construction failed: %v", cerr)
	}
	return client.ObserveNativeWorld(context.Background(), "m", "p", expected)
}

// TestObserveNativeWorldHappyPath proves the exact wire shape: method, path, header, bounded
// body, bookended reads, one forward per rank, and no extra requests.
func TestObserveNativeWorldHappyPath(t *testing.T) {
	p := &probeServer{
		boundBody: fixedBound(http.StatusBadRequest, boundaryBody(2)),
		forward:   fixedForward(http.StatusOK, forwardBody),
	}
	server := httptest.NewServer(p.handler(t))
	defer server.Close()

	obs, oerr := observe(t, server, 2)
	if oerr != nil {
		t.Fatalf("observation refused: %v", oerr)
	}
	if obs.ObservedWidth != 2 || obs.Boundaries != [2]int{2, 2} {
		t.Fatalf("wrong observation: %+v", obs)
	}
	if len(obs.Forwards) != 2 || !obs.Forwards[0].Served || !obs.Forwards[1].Served {
		t.Fatalf("forwards wrong: %+v", obs.Forwards)
	}
	if p.requests != 4 {
		t.Fatalf("expected exactly 2 boundary reads and 2 forwards, saw %d", p.requests)
	}
	for i, b := range p.bodies {
		if b.method != http.MethodPost || b.path != "/v1/completions" {
			t.Fatalf("call %d: %s %s, want POST /v1/completions", i, b.method, b.path)
		}
		if b.rank == "" {
			t.Fatalf("call %d: rank header missing", i)
		}
		var body map[string]any
		if json.Unmarshal([]byte(b.body), &body) != nil {
			t.Fatalf("call %d: body not JSON: %q", i, b.body)
		}
		if body["model"] != "m" || body["prompt"] != "p" {
			t.Fatalf("call %d: body wrong: %v", i, body)
		}
		if mt, ok := body["max_tokens"].(float64); !ok || mt != 1 {
			t.Fatalf("call %d: max_tokens wrong: %v", i, body["max_tokens"])
		}
		if body["stream"] != false {
			t.Fatalf("call %d: stream must be false", i)
		}
	}
	if p.bodies[0].rank != "-1" || p.bodies[3].rank != "-1" {
		t.Fatalf("bookends must use the sentinel rank: %q %q", p.bodies[0].rank, p.bodies[3].rank)
	}
	if p.bodies[1].rank != "0" || p.bodies[2].rank != "1" {
		t.Fatalf("forwards must be rank-directed in order: %q %q", p.bodies[1].rank, p.bodies[2].rank)
	}
}

func TestObserveNativeWorldRefusals(t *testing.T) {
	cases := []struct {
		name       string
		bound      func(int) (int, string)
		forward    func(int, string) (int, string)
		expected   int
		wantKind   Kind
		wantSub    string
		wantNoSend bool // forwards must not start
	}{
		{
			name:     "second boundary disagrees with first",
			bound:    func(seq int) (int, string) { return http.StatusBadRequest, boundaryBody(2 + seq) },
			forward:  fixedForward(http.StatusOK, forwardBody),
			expected: 2, wantKind: KindMalformed, wantSub: "does not match the expected width",
		},
		{
			name:     "boundary read names a different world than expected",
			bound:    fixedBound(http.StatusBadRequest, boundaryBody(4)),
			forward:  fixedForward(http.StatusOK, forwardBody),
			expected: 2, wantKind: KindMalformed, wantSub: "does not match the expected width", wantNoSend: true,
		},
		{
			name:     "header ignored answers 200",
			bound:    fixedBound(http.StatusOK, forwardBody),
			forward:  fixedForward(http.StatusOK, forwardBody),
			expected: 2, wantKind: KindMalformed, wantSub: "rank header is not enforced",
		},
		{
			name:     "generic unrelated bad request",
			bound:    fixedBound(http.StatusBadRequest, `{"error":{"message":"model X not found","type":"NotFoundError","code":400}}`),
			forward:  fixedForward(http.StatusOK, forwardBody),
			expected: 2, wantKind: KindMalformed, wantSub: "not the native",
		},
		{
			name:     "reconfiguring engine on probe",
			bound:    fixedBound(http.StatusServiceUnavailable, `{"error":{"message":"busy"}}`),
			forward:  fixedForward(http.StatusOK, forwardBody),
			expected: 2, wantKind: KindReconfiguring,
		},
		{
			name:     "engine failure on probe",
			bound:    fixedBound(http.StatusInternalServerError, `{}`),
			forward:  fixedForward(http.StatusOK, forwardBody),
			expected: 2, wantKind: KindNativeFailure,
		},
		{
			name:     "redirect is refused, not followed",
			bound:    fixedBound(http.StatusFound, ""),
			forward:  fixedForward(http.StatusOK, forwardBody),
			expected: 2, wantKind: KindMalformed, wantSub: "native validation error",
		},
		{
			name:  "forward fails mid-set",
			bound: fixedBound(http.StatusBadRequest, boundaryBody(2)),
			forward: func(seq int, rank string) (int, string) {
				if rank == "1" {
					return http.StatusInternalServerError, `{}`
				}
				return http.StatusOK, forwardBody
			},
			expected: 2, wantKind: KindNativeFailure, wantSub: "rank 1",
		},
		{
			name:  "forward missing usage",
			bound: fixedBound(http.StatusBadRequest, boundaryBody(2)),
			forward: func(seq int, rank string) (int, string) {
				if rank == "0" {
					return http.StatusOK, `{"choices":[{"text":" "}]}`
				}
				return http.StatusOK, forwardBody
			},
			expected: 2, wantKind: KindMalformed, wantSub: "rank 0",
		},
		{
			name:  "forward carries an error object",
			bound: fixedBound(http.StatusBadRequest, boundaryBody(2)),
			forward: func(seq int, rank string) (int, string) {
				return http.StatusOK, `{"error":{"message":"late"}}`
			},
			expected: 2, wantKind: KindMalformed, wantSub: "rank 0",
		},
		{
			name:     "forward answer malformed",
			bound:    fixedBound(http.StatusBadRequest, boundaryBody(2)),
			forward:  fixedForward(http.StatusOK, `not-json`),
			expected: 2, wantKind: KindMalformed, wantSub: "rank 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &probeServer{boundBody: tc.bound, forward: tc.forward}
			server := httptest.NewServer(p.handler(t))
			defer server.Close()

			obs, oerr := observe(t, server, tc.expected)
			if oerr == nil {
				t.Fatalf("expected refusal, got observation %+v", obs)
			}
			if oerr.Kind != tc.wantKind {
				t.Fatalf("kind %v, want %v (detail: %s)", oerr.Kind, tc.wantKind, oerr.Detail)
			}
			if tc.wantSub != "" && !strings.Contains(oerr.Detail, tc.wantSub) {
				t.Fatalf("detail %q does not carry %q", oerr.Detail, tc.wantSub)
			}
			if tc.wantNoSend && p.forwards != nil {
				t.Fatalf("forwards ran despite refusal: %v", p.forwards)
			}
			if strings.Contains(oerr.Error(), "model") || strings.Contains(oerr.Detail, `"model"`) {
				t.Fatalf("error leaked request content: %s", oerr.Error())
			}
		})
	}
}

func TestObserveNativeWorldBoundRefusals(t *testing.T) {
	cases := []struct {
		name    string
		width   int
		model   string
		wantSub string
	}{
		{name: "zero width", width: 0, model: "m", wantSub: "outside the bounded range"},
		{name: "width above the probe bound", width: maxObservedWorldWidth + 1, model: "m", wantSub: "outside the bounded range"},
		{name: "missing model", width: 2, model: "", wantSub: "model name is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls++
			}))
			defer server.Close()
			client, cerr := New(server.URL, time.Second)
			if cerr != nil {
				t.Fatalf("client construction failed: %v", cerr)
			}
			obs, oerr := client.ObserveNativeWorld(context.Background(), tc.model, "p", tc.width)
			if oerr == nil || obs != nil {
				t.Fatalf("expected refusal, got %+v / %v", obs, oerr)
			}
			if oerr.Kind != KindInvalidInput || !strings.Contains(oerr.Detail, tc.wantSub) {
				t.Fatalf("wrong refusal: %v", oerr)
			}
			if calls != 0 {
				t.Fatalf("refused observation sent %d requests", calls)
			}
		})
	}
}

// TestObserveNativeWorldElasticProfileWidths is the cross-package half of the elastic profile
// contract: every width the profile supports is measurable here, and the first width above it
// is refused before a single request is sent.
//
// THE MAXIMUM IS THE SHARED ONE, so widening the profile widens this bound with it. A copy of
// the number written into this package instead is exactly what this test catches: the observer
// would go on refusing a width admission had already begun to allow, and no other test in this
// package would notice, because every other one measures a width inside both ranges.
func TestObserveNativeWorldElasticProfileWidths(t *testing.T) {
	for width := elasticprofile.WidthMin; width <= elasticprofile.WidthMax; width++ {
		p := &probeServer{
			boundBody: fixedBound(http.StatusBadRequest, boundaryBody(width)),
			forward:   fixedForward(http.StatusOK, forwardBody),
		}
		server := httptest.NewServer(p.handler(t))
		obs, oerr := observe(t, server, width)
		server.Close()
		if oerr != nil {
			t.Fatalf("profile width %d refused: %v", width, oerr)
		}
		if obs.ObservedWidth != width || len(obs.Forwards) != width {
			t.Fatalf("profile width %d measured %d ranks over bound %d",
				width, len(obs.Forwards), obs.ObservedWidth)
		}
	}

	cases := []struct {
		name  string
		width int
	}{
		{name: "the narrowest profile width is measurable", width: elasticprofile.WidthMin},
		{name: "the widest profile width is measurable", width: elasticprofile.WidthMax},
		{name: "one above the profile is refused", width: elasticprofile.WidthMax + 1},
		{name: "an empty world is refused", width: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls++
			}))
			defer server.Close()
			client, cerr := New(server.URL, time.Second)
			if cerr != nil {
				t.Fatalf("client construction failed: %v", cerr)
			}
			obs, oerr := client.ObserveNativeWorld(context.Background(), "m", "p", tc.width)
			measurable := tc.width >= elasticprofile.WidthMin && tc.width <= elasticprofile.WidthMax
			if measurable {
				// The engine never answered, so the measurement fails on its evidence. What
				// matters here is that it failed there and not at the width bound.
				if oerr != nil && strings.Contains(oerr.Detail, "outside the bounded range") {
					t.Fatalf("profile width %d refused by the bound: %v", tc.width, oerr)
				}

				return
			}
			if oerr == nil {
				t.Fatalf("width %d outside the profile was accepted: %+v", tc.width, obs)
			}
			if !strings.Contains(oerr.Detail, "outside the bounded range") {
				t.Fatalf("width %d refused for the wrong reason: %v", tc.width, oerr)
			}
			if calls != 0 {
				t.Fatalf("refused width %d still sent %d requests", tc.width, calls)
			}
		})
	}
}

// TestObserveNativeWorldProbeFloorIsNotTheProfileFloor records why this package states its own
// narrowest world: a single-rank world is real, so measuring one is not an error, and the
// profile's wider floor is a property of the profile rather than of an observation. The rule
// that does bind is that the floor may never rise above the profile's, or a profile-legal
// width would become unmeasurable.
func TestObserveNativeWorldProbeFloorIsNotTheProfileFloor(t *testing.T) {
	if minObservedWorldWidth != 1 {
		t.Fatalf("the probe floor is 1, this package declares %d", minObservedWorldWidth)
	}
	if minObservedWorldWidth > elasticprofile.WidthMin {
		t.Fatalf("the probe floor %d is above the profile floor %d, so a profile-legal width "+
			"would be unmeasurable", minObservedWorldWidth, elasticprofile.WidthMin)
	}
}

func TestObserveNativeWorldDeadline(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(boundaryBody(2)))
	}))
	defer slow.Close()
	client, cerr := New(slow.URL, 5*time.Second)
	if cerr != nil {
		t.Fatalf("client construction failed: %v", cerr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	obs, oerr := client.ObserveNativeWorld(ctx, "m", "p", 2)
	if oerr == nil || obs != nil {
		t.Fatalf("expected deadline refusal, got %+v / %v", obs, oerr)
	}
	if oerr.Kind != KindAmbiguous {
		t.Fatalf("kind %v, want ambiguous", oerr.Kind)
	}
}

func TestParseObservedBound(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    int
		wantErr bool
	}{
		{name: "native shape", body: boundaryBody(4), want: 4},
		{name: "wrong message text", body: `{"error":{"message":"out of range","type":"BadRequestError","code":400}}`, wantErr: true},
		{name: "wrong type", body: `{"error":{"message":"data_parallel_rank -1 is out of range [0, 2).","type":"InternalError","code":400}}`, wantErr: true},
		{name: "wrong code", body: `{"error":{"message":"data_parallel_rank -1 is out of range [0, 2).","type":"BadRequestError","code":422}}`, wantErr: true},
		{name: "param set", body: `{"error":{"message":"data_parallel_rank -1 is out of range [0, 2).","type":"BadRequestError","param":"rank","code":400}}`, wantErr: true},
		{name: "not an object", body: `[]`, wantErr: true},
		{name: "zero bound", body: boundaryBody(0), wantErr: true},
		{name: "negative bound", body: strings.Replace(boundaryBody(2), "[0, 2)", "[0, -2)", 1), wantErr: true},
		{name: "param absent despite required field", body: `{"error":{"message":"data_parallel_rank -1 is out of range [0, 2).","type":"BadRequestError","code":400}}`, wantErr: true},
		{name: "noncanonical plus bound", body: strings.Replace(boundaryBody(2), "[0, 2)", "[0, +2)", 1), wantErr: true},
		{name: "noncanonical leading zero bound", body: strings.Replace(boundaryBody(2), "[0, 2)", "[0, 02)", 1), wantErr: true},
		{name: "bound with inner space", body: strings.Replace(boundaryBody(2), "[0, 2)", "[0, 2 )", 1), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseObservedBound([]byte(tc.body))
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got %d", got)
			}
			if !tc.wantErr && (err != nil || got != tc.want) {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// These controls cover bounded observations and malformed replies.

func TestObserveNativeWorldConstructorTimeoutBoundsWholeObservation(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(boundaryBody(2)))
	}))
	defer slow.Close()
	client, cerr := New(slow.URL, 80*time.Millisecond)
	if cerr != nil {
		t.Fatalf("client construction failed: %v", cerr)
	}
	start := time.Now()
	obs, oerr := client.ObserveNativeWorld(context.Background(), "m", "p", 2)
	elapsed := time.Since(start)
	if oerr == nil || obs != nil {
		t.Fatalf("expected constructor-timeout refusal, got %+v / %v", obs, oerr)
	}
	if oerr.Kind != KindAmbiguous {
		t.Fatalf("kind %v, want ambiguous", oerr.Kind)
	}
	if elapsed >= time.Second {
		t.Fatalf("observation ran %v, the constructor timeout did not bound it", elapsed)
	}
}

func TestObserveNativeWorldCallerTighterBoundWins(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(boundaryBody(2)))
	}))
	defer slow.Close()
	client, cerr := New(slow.URL, 5*time.Second)
	if cerr != nil {
		t.Fatalf("client construction failed: %v", cerr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, oerr := client.ObserveNativeWorld(ctx, "m", "p", 2); oerr == nil || oerr.Kind != KindAmbiguous {
		t.Fatalf("expected caller-bound refusal, got %v", oerr)
	}
}

func TestObserveNativeWorldAlreadyCanceled(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()
	client, cerr := New(server.URL, time.Second)
	if cerr != nil {
		t.Fatalf("client construction failed: %v", cerr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, oerr := client.ObserveNativeWorld(ctx, "m", "p", 2); oerr == nil || oerr.Kind != KindAmbiguous {
		t.Fatalf("expected canceled refusal, got %v", oerr)
	}
	if calls != 0 {
		t.Fatalf("canceled observation sent %d requests", calls)
	}
}

func TestObserveNativeWorldCumulativeExpiry(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(rankHeader) == "-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(boundaryBody(4)))
			return
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte(forwardBody))
	}))
	defer server.Close()
	client, cerr := New(server.URL, 70*time.Millisecond)
	if cerr != nil {
		t.Fatalf("client construction failed: %v", cerr)
	}
	obs, oerr := client.ObserveNativeWorld(context.Background(), "m", "p", 4)
	if oerr == nil || obs != nil {
		t.Fatalf("expected cumulative-expiry refusal, got %+v / %v", obs, oerr)
	}
	if oerr.Kind != KindAmbiguous {
		t.Fatalf("kind %v (%s; status %d), want ambiguous", oerr.Kind, oerr.Detail, oerr.StatusCode)
	}
	if calls.Load() >= 1+4+1 {
		t.Fatalf("cumulative bound did not hold: %d calls", calls.Load())
	}
}

func TestObserveNativeWorldForwardShapeRefusals(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{name: "null choice", body: `{"id":"c","object":"text_completion","created":1,"model":"m","choices":[null],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, wantSub: "rank 0"},
		{name: "empty choices", body: `{"id":"c","object":"text_completion","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, wantSub: "rank 0"},
		{name: "wrong object field", body: `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"text":" "}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, wantSub: "rank 0"},
		{name: "missing object field", body: `{"id":"c","created":1,"model":"m","choices":[{"index":0,"text":" "}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, wantSub: "rank 0"},
		{name: "missing text in choice", body: `{"id":"c","object":"text_completion","created":1,"model":"m","choices":[{"index":0}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, wantSub: "rank 0"},
		{name: "wrong token count", body: `{"id":"c","object":"text_completion","created":1,"model":"m","choices":[{"index":0,"text":"ab"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`, wantSub: "rank 0"},
		{name: "missing usage", body: `{"id":"c","object":"text_completion","created":1,"model":"m","choices":[{"index":0,"text":" "}]}`, wantSub: "rank 0"},
		{name: "whitespace token stays legitimate", body: `{"id":"c","object":"text_completion","created":1,"model":"m","choices":[{"index":0,"text":"   ","finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, wantSub: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantServed := tc.wantSub == ""
			p := &probeServer{
				boundBody: fixedBound(http.StatusBadRequest, boundaryBody(2)),
				forward:   fixedForward(http.StatusOK, tc.body),
			}
			server := httptest.NewServer(p.handler(t))
			defer server.Close()
			obs, oerr := observe(t, server, 2)
			if wantServed {
				if oerr != nil || len(obs.Forwards) != 2 {
					t.Fatalf("legitimate answer refused: %v", oerr)
				}
				return
			}
			if oerr == nil {
				t.Fatalf("expected refusal, got %+v", obs)
			}
			if !strings.Contains(oerr.Detail, tc.wantSub) {
				t.Fatalf("detail %q lacks %q", oerr.Detail, tc.wantSub)
			}
		})
	}
}

func TestObserveNativeWorldSecretNeverLeaks(t *testing.T) {
	p := &probeServer{
		boundBody: fixedBound(http.StatusBadRequest,
			`{"error":{"message":"data_parallel_rank -1 is out of range [0, MODEL_PROMPT_SECRET).","type":"BadRequestError","code":400,"param":null}}`),
		forward: fixedForward(http.StatusOK, forwardBody),
	}
	server := httptest.NewServer(p.handler(t))
	defer server.Close()
	_, oerr := observe(t, server, 2)
	if oerr == nil {
		t.Fatalf("expected refusal for the malformed bound")
	}
	if strings.Contains(oerr.Detail, "MODEL_PROMPT_SECRET") || strings.Contains(oerr.Error(), "MODEL_PROMPT_SECRET") {
		t.Fatalf("raw body content leaked: %s / %s", oerr.Detail, oerr.Error())
	}
}

func TestObserveNativeWorldOversizedAnswers(t *testing.T) {
	huge := `{"pad":"` + strings.Repeat("x", maxResponseBytes+1024) + `"}`
	cases := []struct {
		name    string
		bound   func(int) (int, string)
		forward func(int, string) (int, string)
		wantSub string
	}{
		{
			name: "oversized boundary answer", wantSub: "byte bound",
			bound:   fixedBound(http.StatusBadRequest, huge),
			forward: fixedForward(http.StatusOK, forwardBody),
		},
		{
			name: "oversized forward answer", wantSub: "byte bound",
			bound:   fixedBound(http.StatusBadRequest, boundaryBody(2)),
			forward: fixedForward(http.StatusOK, huge),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &probeServer{boundBody: tc.bound, forward: tc.forward}
			server := httptest.NewServer(p.handler(t))
			defer server.Close()
			_, oerr := observe(t, server, 2)
			if oerr == nil {
				t.Fatalf("expected oversize refusal")
			}
			if oerr.Kind != KindMalformed || !strings.Contains(oerr.Detail, tc.wantSub) {
				t.Fatalf("wrong refusal: %v", oerr)
			}
		})
	}
}

func TestObserveNativeWorldRedirectTargetNeverReached(t *testing.T) {
	var targetHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(http.ResponseWriter, *http.Request) {
		targetHits++
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if _, oerr := observe(t, server, 2); oerr == nil {
		t.Fatalf("expected redirect refusal")
	}
	if targetHits != 0 {
		t.Fatalf("redirect was followed %d times", targetHits)
	}
}

func TestObserveNativeWorldTransportReadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(boundaryBody(2)))
		if f, ok := w.(http.Hijacker); ok {
			conn, _, _ := f.Hijack()
			if conn != nil {
				_ = conn.Close()
			}
		}
	}))
	defer server.Close()
	var later int
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		later++
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(boundaryBody(2)))
		if f, ok := w.(http.Hijacker); ok {
			conn, _, _ := f.Hijack()
			if conn != nil {
				_ = conn.Close()
			}
		}
	})
	if _, oerr := observe(t, server, 2); oerr == nil || oerr.Kind != KindAmbiguous {
		t.Fatalf("expected ambiguous transport refusal, got %v", oerr)
	}
	if later > 1 {
		t.Fatalf("client retried after a read failure: %d calls", later)
	}
}

func TestObserveNativeWorldNoCallsAfterRefusal(t *testing.T) {
	p := &probeServer{
		boundBody: fixedBound(http.StatusBadRequest, boundaryBody(2)),
		forward: func(seq int, rank string) (int, string) {
			return http.StatusInternalServerError, `{}`
		},
	}
	server := httptest.NewServer(p.handler(t))
	defer server.Close()
	if _, oerr := observe(t, server, 2); oerr == nil {
		t.Fatalf("expected refusal")
	}
	if p.requests != 2 {
		t.Fatalf("expected exactly one boundary read and one failed forward, saw %d", p.requests)
	}
}

// TestTheBoundaryPrefixNamesTheSentinelRank pins the coupling between the sentinel the observer asks
// with and the pinned frontend's message it parses the answer out of. The two live in different
// declarations, so a change to either alone still compiles and still passes every parse test, while
// every boundary read in the cluster starts failing as KindMalformed.
func TestTheBoundaryPrefixNamesTheSentinelRank(t *testing.T) {
	if !strings.HasPrefix(boundaryPrefix, fmt.Sprintf("data_parallel_rank %d ", sentinelRank)) {
		t.Errorf("the parsed prefix must name the rank the observer actually asks with: prefix=%q sentinel=%d",
			boundaryPrefix, sentinelRank)
	}
}
