package elasticengine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorded is what the engine saw, so a test can check the method, the path and the body the client
// actually put on the wire rather than what it intended to.
type recorded struct {
	Method string
	Path   string
	Body   string
}

// answerMode is how a stand-in engine replies. It is an enumerated fact about the wire, so a case
// row can name the shape of a reply without carrying a handler of its own.
type answerMode int

const (
	// answerReply sends the status and the body as given.
	answerReply answerMode = iota
	// answerRedirect sends the status with a Location header.
	answerRedirect
	// answerDisconnect takes the connection away without answering at all.
	answerDisconnect
	// answerStallHeaders accepts the request and never sends the headers.
	answerStallHeaders
	// answerStallBody sends the headers and then stops writing.
	answerStallBody
)

// answer is the whole reply a stand-in engine makes. It is data on purpose. A row that carried its
// own handler would be a procedure, and a procedure in a table hides what the case is about.
type answer struct {
	mode     answerMode
	status   int
	body     string
	location string
}

// engine is a controllable stand-in for the native routes. It counts what it was asked for, so a
// test can assert that a mutation happened exactly once rather than inferring it from the reply.
type engine struct {
	mu       sync.Mutex
	requests []recorded
	reply    answer
	server   *httptest.Server
}

func newEngine(t *testing.T, reply answer) *engine {
	t.Helper()

	e := &engine{reply: reply}
	e.server = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.server.Close)

	return e
}

// serve is the single execution handler for every case. It records the request and then produces
// the reply the row asked for.
func (e *engine) serve(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 0)
	if r.Body != nil {
		buffer := make([]byte, 4096)
		read, _ := r.Body.Read(buffer)
		body = buffer[:read]
	}
	e.mu.Lock()
	e.requests = append(e.requests, recorded{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	e.mu.Unlock()

	switch e.reply.mode {
	case answerReply:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.reply.status)
		_, _ = w.Write([]byte(e.reply.body))
	case answerRedirect:
		w.Header().Set("Location", e.reply.location)
		w.WriteHeader(e.reply.status)
	case answerDisconnect:
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case answerStallHeaders:
		<-r.Context().Done()
	case answerStallBody:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.reply.status)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}
}

func (e *engine) seen() []recorded {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]recorded(nil), e.requests...)
}

func (e *engine) client(t *testing.T, timeout time.Duration) *Client {
	t.Helper()

	client, err := New(e.server.URL, timeout)
	require.NoError(t, err)

	return client
}

// okBody is the native acknowledgement for a resize that the engine applied.
const okBody = `{"message":"Scaled to 4 data parallel engines"}`

// quietBody is the native answer that says no resize is in progress.
const quietBody = `{"is_scaling_elastic_ep":false}`

// TestScaleSendsExactlyTheNativeRequest checks the wire itself: one POST, to the native path, with
// the two fields the native route reads and the timeout expressed in whole seconds.
func TestScaleSendsExactlyTheNativeRequest(t *testing.T) {
	e := newEngine(t, answer{mode: answerReply, status: http.StatusOK, body: okBody})
	client := e.client(t, 5*time.Second)

	require.NoError(t, client.Scale(context.Background(), 4, 2*time.Minute))

	seen := e.seen()
	require.Len(t, seen, 1, "one request, exactly once")
	assert.Equal(t, http.MethodPost, seen[0].Method)
	assert.Equal(t, scalePath, seen[0].Path)
	assert.JSONEq(t, `{"new_data_parallel_size":4,"drain_timeout":120}`, seen[0].Body)
}

// TestScaleIsSentOnceAndNeverRetried is the property the whole package exists for. Every failure
// shape that could tempt a client to try again is exercised, and the count is the assertion.
func TestScaleIsSentOnceAndNeverRetried(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply answer
		kind  Kind
	}{
		{
			name:  "a 408 is the engine reporting it could not drain in time",
			reply: answer{mode: answerReply, status: http.StatusRequestTimeout, body: `{"detail":"drain timeout"}`},
			kind:  KindNativeFailure,
		},
		{
			name:  "a 500 is the engine reporting it failed",
			reply: answer{mode: answerReply, status: http.StatusInternalServerError, body: `{"detail":"Scale failed"}`},
			kind:  KindNativeFailure,
		},
		{
			name:  "a 503 is the engine reconfiguring and did not apply it",
			reply: answer{mode: answerReply, status: http.StatusServiceUnavailable, body: `{"detail":"reconfiguring"}`},
			kind:  KindReconfiguring,
		},
		{
			name:  "a disconnected member leaves the answer ambiguous",
			reply: answer{mode: answerDisconnect},
			kind:  KindAmbiguous,
		},
		{
			name:  "a body that is not JSON is malformed rather than a failure",
			reply: answer{mode: answerReply, status: http.StatusOK, body: "not json at all"},
			kind:  KindMalformed,
		},
		{
			name: "an answer past the bound is refused",
			reply: answer{
				mode:   answerReply,
				status: http.StatusOK,
				body:   `{"message":"` + strings.Repeat("x", maxResponseBytes+16) + `"}`,
			},
			kind: KindMalformed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, tc.reply)
			client := e.client(t, 5*time.Second)

			err := client.Scale(context.Background(), 4, time.Minute)

			require.Error(t, err)
			var classified *Error
			require.ErrorAs(t, err, &classified)
			assert.Equal(t, tc.kind, classified.Kind, "detail=%s", classified.Detail)
			assert.Len(t, e.seen(), 1, "a mutation is sent once whatever the answer")
		})
	}
}

// TestScaleRequiresTheNativeAcknowledgement checks that a 200 is read as the object the route
// promises. A body that is merely valid JSON did not acknowledge a resize, and a nil error there
// would tell the caller the mutation landed on the strength of a document the route never sends.
func TestScaleRequiresTheNativeAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		valid bool
	}{
		{name: "the native message is accepted", body: okBody, valid: true},
		{name: "a null answer is refused", body: `null`},
		{name: "an array answer is refused", body: `[]`},
		{name: "a scalar answer is refused", body: `true`},
		{name: "a missing message is refused", body: `{}`},
		{name: "a null message is refused", body: `{"message":null}`},
		{name: "a message that is not a string is refused", body: `{"message":4}`},
		{name: "a second document is refused", body: `{"message":"Scaled"} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, answer{mode: answerReply, status: http.StatusOK, body: tc.body})
			client := e.client(t, 5*time.Second)

			err := client.Scale(context.Background(), 4, time.Minute)

			if tc.valid {
				require.NoError(t, err)

				return
			}
			var classified *Error
			require.ErrorAs(t, err, &classified)
			assert.Equal(t, KindMalformed, classified.Kind, "detail=%s", classified.Detail)
			assert.Len(t, e.seen(), 1, "a refused answer does not undo the one request sent")
		})
	}
}

// TestAnInvalidInputNeverReachesTheEngine checks that a local refusal is local.
func TestAnInvalidInputNeverReachesTheEngine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		timeout time.Duration
	}{
		{name: "a zero width is refused", size: 0, timeout: time.Minute},
		{name: "a negative width is refused", size: -2, timeout: time.Minute},
		{name: "a zero drain timeout is refused", size: 4, timeout: 0},
		{name: "a negative drain timeout is refused", size: 4, timeout: -time.Minute},
		{name: "a sub-second drain timeout is refused", size: 4, timeout: 500 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, answer{mode: answerReply, status: http.StatusOK, body: okBody})
			client := e.client(t, 5*time.Second)

			err := client.Scale(context.Background(), tc.size, tc.timeout)

			require.Error(t, err)
			var classified *Error
			require.ErrorAs(t, err, &classified)
			assert.Equal(t, KindInvalidInput, classified.Kind)
			assert.Empty(t, e.seen(), "a refused input must not reach the engine")
		})
	}
}

// TestTheConstructorRefusesWhatItCannotUse covers the constructor's own rule, including that a
// refusal never repeats a base URL back, because a base URL may carry a credential.
func TestTheConstructorRefusesWhatItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		timeout time.Duration
		secret  string
	}{
		{name: "a zero timeout is refused", baseURL: "http://engine.invalid", timeout: 0},
		{name: "a negative timeout is refused", baseURL: "http://engine.invalid", timeout: -time.Second},
		{name: "an empty base URL is refused", baseURL: "  ", timeout: time.Second},
		{name: "a base URL that is not a URL is refused", baseURL: "://", timeout: time.Second},
		{name: "a base URL with no host is refused", baseURL: "http://", timeout: time.Second},
		{name: "a base URL with the wrong scheme is refused", baseURL: "file:///tmp/x", timeout: time.Second},
		{
			name:    "a malformed base URL does not repeat the credential",
			baseURL: "http://reader:private-credential@localhost:bad",
			timeout: time.Second,
			secret:  "private-credential",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := New(tc.baseURL, tc.timeout)

			require.Error(t, err)
			assert.Nil(t, client)
			if tc.secret != "" {
				assert.NotContains(t, err.Error(), tc.secret, "a refusal must not carry a credential")
			}
		})
	}

	t.Run("a positive timeout and a valid URL are accepted", func(t *testing.T) {
		client, err := New("https://engine.invalid:8000", 250*time.Millisecond)

		require.NoError(t, err)
		require.NotNil(t, client)
	})
}

// TestIsScalingRequiresTheFieldToBeThere checks that a quiet engine is never invented out of an
// answer that did not say anything.
func TestIsScalingRequiresTheFieldToBeThere(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		scaling bool
		kind    Kind
	}{
		{name: "an explicit true is accepted", body: `{"is_scaling_elastic_ep":true}`, scaling: true},
		{name: "an explicit false is accepted", body: quietBody},
		{name: "a null answer is refused", body: `null`, kind: KindMalformed},
		{name: "a missing field is refused", body: `{}`, kind: KindMalformed},
		{name: "a null field is refused", body: `{"is_scaling_elastic_ep":null}`, kind: KindMalformed},
		{name: "a string field is refused", body: `{"is_scaling_elastic_ep":"false"}`, kind: KindMalformed},
		{name: "a number field is refused", body: `{"is_scaling_elastic_ep":0}`, kind: KindMalformed},
		{name: "a trailing value is refused", body: `{"is_scaling_elastic_ep":false}{"again":true}`, kind: KindMalformed},
		{name: "an answer that is not an object is refused", body: `[true]`, kind: KindMalformed},
		{name: "an answer that is not JSON is refused", body: `nope`, kind: KindMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, answer{mode: answerReply, status: http.StatusOK, body: tc.body})
			client := e.client(t, 5*time.Second)

			scaling, err := client.IsScaling(context.Background())

			if tc.kind == KindMalformed {
				var classified *Error
				require.ErrorAs(t, err, &classified)
				assert.Equal(t, tc.kind, classified.Kind, "detail=%s", classified.Detail)
				assert.False(t, scaling, "a refused answer is not a quiet engine")

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.scaling, scaling)
		})
	}
}

// TestIsScalingReachesTheNativeRoute checks the method and path, and that the route is read the way
// the native router reads it.
func TestIsScalingReachesTheNativeRoute(t *testing.T) {
	e := newEngine(t, answer{mode: answerReply, status: http.StatusOK, body: quietBody})
	client := e.client(t, 5*time.Second)

	_, err := client.IsScaling(context.Background())

	require.NoError(t, err)
	seen := e.seen()
	require.Len(t, seen, 1)
	assert.Equal(t, http.MethodPost, seen[0].Method)
	assert.Equal(t, isScalingPath, seen[0].Path)
}

// TestAMutationIsNotReplayedToWhateverAnswersARedirect checks the redirect refusal directly.
//
// A POST that follows a redirect may be delivered to a second listener, which for a mutation means
// a second application of a command that was meant for one member.
func TestAMutationIsNotReplayedToWhateverAnswersARedirect(t *testing.T) {
	second := atomic.Int32{}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		second.Add(1)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(elsewhere.Close)

	e := newEngine(t, answer{
		mode:     answerRedirect,
		status:   http.StatusTemporaryRedirect,
		location: elsewhere.URL + scalePath,
	})
	client := e.client(t, 5*time.Second)

	err := client.Scale(context.Background(), 4, time.Minute)

	require.Error(t, err, "a redirected mutation is not an accepted mutation")
	var classified *Error
	require.ErrorAs(t, err, &classified)
	assert.Equal(t, int32(0), second.Load(),
		"the second listener must never be asked to apply the mutation")
}

// TestACallIsBoundedWhenTheMemberStalls covers both halves of a stalled exchange, because a member
// can stop answering before the headers or after them.
func TestACallIsBoundedWhenTheMemberStalls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply answer
	}{
		{
			name:  "a member that never sends headers",
			reply: answer{mode: answerStallHeaders},
		},
		{
			name:  "a member that sends headers and then stops",
			reply: answer{mode: answerStallBody, status: http.StatusOK},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, tc.reply)
			client := e.client(t, 250*time.Millisecond)

			started := time.Now()
			err := client.Scale(context.Background(), 4, time.Minute)
			elapsed := time.Since(started)

			require.Error(t, err)
			var classified *Error
			require.ErrorAs(t, err, &classified)
			assert.Equal(t, KindAmbiguous, classified.Kind, "detail=%s", classified.Detail)
			assert.Less(t, elapsed, 3*time.Second,
				"the call must end near its bound rather than hanging")
		})
	}
}

// TestACallerDeadlineShorterThanTheTimeoutWins checks that a caller's own bound is kept, because a
// client that ignored it would hold the caller past what it decided it could afford.
func TestACallerDeadlineShorterThanTheTimeoutWins(t *testing.T) {
	e := newEngine(t, answer{mode: answerStallHeaders})
	client := e.client(t, 30*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := client.Scale(ctx, 4, time.Minute)
	elapsed := time.Since(started)

	require.Error(t, err)
	var classified *Error
	require.ErrorAs(t, err, &classified)
	assert.Equal(t, KindAmbiguous, classified.Kind)
	assert.Less(t, elapsed, 3*time.Second, "the caller's shorter bound is the one that applies")
}

// TestACancelledCallerIsAmbiguousRatherThanFailed is the distinction the state machine depends on.
func TestACancelledCallerIsAmbiguousRatherThanFailed(t *testing.T) {
	e := newEngine(t, answer{mode: answerStallHeaders})
	client := e.client(t, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	err := client.Scale(ctx, 4, time.Minute)

	require.Error(t, err)
	var classified *Error
	require.ErrorAs(t, err, &classified)
	assert.Equal(t, KindAmbiguous, classified.Kind,
		"a cancelled mutation may already have been applied")
	assert.True(t, errors.Is(classified.Err, context.Canceled) || classified.Err != nil)
}

// TestTheClientIsUsableConcurrently checks the immutability the type promises.
func TestTheClientIsUsableConcurrently(t *testing.T) {
	e := newEngine(t, answer{mode: answerReply, status: http.StatusOK, body: quietBody})
	client := e.client(t, 5*time.Second)

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.IsScaling(context.Background())
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	assert.Len(t, e.seen(), 16)
}
