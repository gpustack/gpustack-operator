package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func observationFixture(t *testing.T) (*Broker, TransferRequest, http.Handler) {
	t.Helper()
	b, generation, owner, _ := gateFixture(t)
	qualifyTestSource(t, b)
	session, perr := b.AdmitSessionContext(context.Background(), owner, "tenant", generation)
	require.Nil(t, perr)
	request := gateTransferRequest(session.ID, generation, strings.Repeat("a", 32))
	return b, request, NewSessionHandler(b, gateAuthorizer{digest: owner.Digest})
}

func observationRequest(t *testing.T, handler http.Handler, request TransferRequest, action string) *httptest.ResponseRecorder {
	t.Helper()
	method, path := http.MethodPost, "/v1/transfers"
	var body any = request
	switch action {
	case "status":
		method = http.MethodGet
		query := url.Values{
			"sessionID": {request.SessionID}, "podNamespace": {request.PodNamespace},
			"podName": {request.PodName}, "podUID": {request.PodUID}, "targetInstanceID": {request.TargetInstanceID},
		}
		path += "/" + request.RequestID + "?" + query.Encode()
		body = nil
	case "ack":
		path += "/" + request.RequestID + "/ack"
		body = map[string]string{
			"sessionID": request.SessionID, "podNamespace": request.PodNamespace,
			"podName": request.PodName, "podUID": request.PodUID, "targetInstanceID": request.TargetInstanceID,
			"registrationID": request.RegistrationID, "windowID": request.WindowID,
		}
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewReader(raw)))
	return recorder
}

func persistedObservation(t *testing.T, b *Broker, id string) OperationRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.root, operationDir, id+".json"))
	require.NoError(t, err)
	var op OperationRecord
	require.NoError(t, json.Unmarshal(raw, &op))
	return op
}

func TestTransferTransientObservationRecovery(t *testing.T) {
	for _, action := range []string{"submit", "status", "ack"} {
		for _, fault := range []string{"timeout", "truncated body", "malformed unavailable body"} {
			t.Run(action+"/"+fault, func(t *testing.T) {
				b, request, handler := observationFixture(t)
				failPath := "/v1/operations"
				if action != "submit" {
					failPath += "/" + action
				}
				var mu sync.Mutex
				calls := map[string]int{}
				var ackBodies []map[string]any
				private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					mu.Lock()
					calls[r.URL.Path]++
					if r.URL.Path == "/v1/operations/ack" {
						ackBodies = append(ackBodies, body)
					}
					fail := r.URL.Path == failPath && calls[r.URL.Path] == 1
					mu.Unlock()
					if fail {
						switch fault {
						case "timeout":
							select {
							case <-r.Context().Done():
							case <-time.After(250 * time.Millisecond):
							}
						case "truncated body":
							w.Header().Set("Content-Length", "1000")
							_, _ = w.Write([]byte(`{"state":`))
						case "malformed unavailable body":
							http.Error(w, "not JSON; private-secret", http.StatusServiceUnavailable)
						}
						return
					}
					reply := PrivateReply{RequestID: request.RequestID, State: "submitted"}
					switch r.URL.Path {
					case "/v1/operations/status":
						reply.State, reply.Verdict = "terminal", "succeeded"
					case "/v1/operations/ack":
						reply.State, reply.Verdict = "acknowledged", "succeeded"
					}
					_ = json.NewEncoder(w).Encode(reply)
				}))
				t.Cleanup(private.Close)
				b.SetPrivateSource(&httpPrivateSource{address: private.URL, token: "private-secret", client: &http.Client{Timeout: 100 * time.Millisecond}})
				if action != "submit" {
					require.Equal(t, http.StatusOK, observationRequest(t, handler, request, "submit").Code)
				}
				if action == "ack" {
					require.Equal(t, http.StatusOK, observationRequest(t, handler, request, "status").Code)
				}
				failed := observationRequest(t, handler, request, action)
				require.Equal(t, http.StatusServiceUnavailable, failed.Code, failed.Body.String())
				require.NotContains(t, failed.Body.String(), "private-secret")
				op := persistedObservation(t, b, request.RequestID)
				require.Equal(t, "unknown", op.State)
				require.Equal(t, request, op.Request)
				require.Equal(t, action == "ack", op.ConsumerReleased)
				require.EqualValues(t, 1, b.Snapshot()[0].ActiveSessions)
				conflict := request
				conflict.RequestID = strings.Repeat("9", 32)
				_, err := b.SubmitTransfer(context.Background(), conflict, gatePod())
				require.ErrorContains(t, err, "window remains owned")
				recoveryAction, wantState := "status", "terminal"
				if action == "submit" {
					recoveryAction = "submit"
				}
				if action == "ack" {
					recoveryAction, wantState = "ack", "acknowledged"
				}
				recovered := observationRequest(t, handler, request, recoveryAction)
				require.Equal(t, http.StatusOK, recovered.Code, recovered.Body.String())
				require.Equal(t, wantState, persistedObservation(t, b, request.RequestID).State)
				if action == "ack" {
					require.Equal(t, http.StatusOK, observationRequest(t, handler, request, "ack").Code)
				}
				require.Equal(t, http.StatusOK, observationRequest(t, handler, request, "status").Code)
				mu.Lock()
				posts, acks := calls["/v1/operations"], calls["/v1/operations/ack"]
				bodies := append([]map[string]any(nil), ackBodies...)
				mu.Unlock()
				require.Equal(t, 1, posts, "recovery must not repost")
				if action == "ack" {
					require.Equal(t, 2, acks, "duplicate acknowledged request must not release twice")
					require.Equal(t, bodies[0], bodies[1], "ambiguous ACK must retry the exact identity")
				}
			})
		}
	}
}

func TestOperationObservationPreservesFinalVerdict(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     string
		released  bool
		reply     PrivateReply
		callErr   error
		wantState string
	}{
		{name: "late timeout after terminal", state: "terminal", callErr: &privateObservationError{reason: "deadline exceeded"}, wantState: "terminal"},
		{name: "late timeout after acknowledgement", state: "acknowledged", released: true, callErr: &privateObservationError{reason: "deadline exceeded"}, wantState: "acknowledged"},
		{name: "conflicting acknowledgement", state: "consumerReleased", released: true, reply: PrivateReply{State: "acknowledged", Verdict: "failed"}, wantState: "unknown"},
		{name: "explicit native unknown after release", state: "consumerReleased", released: true, reply: PrivateReply{State: "unknown"}, wantState: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, request, _ := observationFixture(t)
			op, err := b.SubmitTransfer(context.Background(), request, gatePod())
			require.NoError(t, err)
			op.State, op.Verdict, op.TerminalVerdict, op.ConsumerReleased = tc.state, "succeeded", "succeeded", tc.released
			require.NoError(t, b.persistOperationLocked(&op))
			reply := tc.reply
			reply.RequestID = request.RequestID
			got, err := b.applyOperationReply(request.RequestID, reply, tc.callErr)
			require.NoError(t, err)
			require.Equal(t, tc.wantState, got.State)
			require.Equal(t, "succeeded", got.TerminalVerdict)
			require.Equal(t, tc.released, got.ConsumerReleased)
			require.Equal(t, got, persistedObservation(t, b, request.RequestID))
		})
	}
}

func TestOperationObservationJournalFailureClosesAdmission(t *testing.T) {
	b, request, _ := observationFixture(t)
	_, err := b.SubmitTransfer(context.Background(), request, gatePod())
	require.NoError(t, err)
	dir := filepath.Join(b.root, operationDir)
	require.NoError(t, os.Rename(dir, dir+"-saved"))
	require.NoError(t, os.WriteFile(dir, []byte("unavailable"), 0o600))
	_, err = b.applyOperationReply(request.RequestID, PrivateReply{}, &privateObservationError{reason: "deadline exceeded"})
	require.Error(t, err)
	var observationErr *privateObservationError
	require.False(t, errors.As(err, &observationErr), "journal failure must not become a retriable observation")
	require.False(t, b.operationsReady)
	require.Equal(t, "submitted", b.operations[request.RequestID].State)
}

func TestPrivateObservationCancellationAndRedirect(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "canceled request", cancel: true},
		{name: "redirect refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			}))
			t.Cleanup(server.Close)
			private := NewPrivateSource("private-secret").(*httpPrivateSource)
			private.address = server.URL
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			_, err := private.Call(ctx, "/v1/operations/status", map[string]string{"requestID": strings.Repeat("e", 32)})
			require.Error(t, err)
			var observationErr *privateObservationError
			require.Equal(t, tc.cancel, errors.As(err, &observationErr))
			require.NotContains(t, err.Error(), "private-secret")
		})
	}
}

func TestPrivateObservationHTTPFraming(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		retry    bool
	}{
		{name: "connection lost", retry: true},
		{name: "invalid status", response: "not HTTP\r\n\r\n"},
		{name: "invalid chunk", response: "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\ninvalid\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, writer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = writer.WriteString(tc.response)
				_ = writer.Flush()
			}))
			t.Cleanup(server.Close)
			private := NewPrivateSource("private-secret").(*httpPrivateSource)
			private.address = server.URL
			_, err := private.Call(context.Background(), "/v1/operations/status", map[string]string{})
			require.Error(t, err)
			var observationErr *privateObservationError
			require.Equal(t, tc.retry, errors.As(err, &observationErr))
		})
	}
}

func TestTransferInvalidObservationRemainsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "native unknown", body: `{"requestID":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","state":"unknown"}`},
		{name: "foreign identity", body: `{"requestID":"ffffffffffffffffffffffffffffffff","state":"submitted"}`},
		{name: "invalid verdict", body: `{"requestID":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","state":"terminal","verdict":"maybe"}`},
		{name: "invalid JSON", body: `{"state":`},
		{name: "duplicate field", body: `{"state":"submitted","state":"unknown"}`},
		{name: "unknown field", body: `{"state":"submitted","secret":"private-secret"}`},
		{name: "refused", status: http.StatusForbidden, body: "private-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, request, handler := observationFixture(t)
			private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(private.Close)
			b.SetPrivateSource(&httpPrivateSource{address: private.URL, token: "private-secret", client: private.Client()})
			response := observationRequest(t, handler, request, "submit")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"state":"unknown"`)
			require.NotContains(t, response.Body.String(), "private-secret")
			require.Equal(t, "unknown", persistedObservation(t, b, request.RequestID).State)
		})
	}
}
