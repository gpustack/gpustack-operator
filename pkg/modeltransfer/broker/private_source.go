package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// PrivateSource is the bounded, authenticated source-process control channel.
type PrivateSource interface {
	Call(context.Context, string, any) (PrivateReply, error)
}

// PrivateReply is the source process's operation or session verdict.
type PrivateReply struct {
	RequestID string `json:"requestID,omitempty"`
	State     string `json:"state"`
	Verdict   string `json:"verdict,omitempty"`
}

type httpPrivateSource struct {
	address string
	token   string
	client  *http.Client
}

// privateObservationError means the source's reply was not observed. The
// operation remains owned and may only be recovered through the same identity.
type privateObservationError struct{ reason string }

func (e *privateObservationError) Error() string {
	return "private source observation unavailable: " + e.reason
}

var errPrivateRedirect = errors.New("private redirect refused")

func unavailableObservation(err error) error {
	var networkErr *net.OpError
	reason := "transport interrupted"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "deadline exceeded"
	case errors.Is(err, context.Canceled):
		reason = "request canceled"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.As(err, &networkErr):
	default:
		return errors.New("private response is invalid")
	}
	return &privateObservationError{reason: reason}
}

// NewPrivateSource constructs the production loopback transport.
func NewPrivateSource(token string) PrivateSource {
	return &httpPrivateSource{address: "http://127.0.0.1:9252", token: token, client: &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil, MaxConnsPerHost: 64},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errPrivateRedirect },
	}}
}

func (s *httpPrivateSource) Call(ctx context.Context, path string, payload any) (PrivateReply, error) {
	var reply PrivateReply
	switch path {
	case "/v1/operations", "/v1/operations/status", "/v1/operations/ack", "/v1/sessions/close":
	default:
		return reply, errors.New("private path refused")
	}
	if s.address == "" || s.token == "" {
		return reply, errors.New("private source is unavailable")
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > maxControlBody {
		return reply, errors.New("private request is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.address+path, bytes.NewReader(raw))
	if err != nil {
		return reply, err
	}
	req.Header.Set("Authorization", s.token)
	req.Header.Set("Content-Type", "application/json")
	if path == "/v1/operations" || path == "/v1/operations/ack" {
		req.Header.Set("X-GPUStack-Wait-Ms", "50")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, errPrivateRedirect) {
			return reply, errPrivateRedirect
		}
		return reply, unavailableObservation(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return reply, &privateObservationError{reason: "source HTTP failure"}
	}
	if resp.StatusCode != http.StatusOK {
		return reply, errors.New("private source refused the request")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxControlBody+1))
	if err != nil {
		return reply, unavailableObservation(err)
	}
	if len(data) > maxControlBody || rejectDuplicateJSONKeys(string(data)) != nil {
		return reply, errors.New("private response is invalid")
	}
	if err := decodeStrictDocument(data, &reply); err != nil {
		return reply, errors.New("private response is invalid")
	}
	return reply, nil
}
