package mooncake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// The admin surface's paths. Every one of them lives on the leader's metrics port: that single port
// serves the Prometheus exposition and the HTTP admin API both, which is why status publishes it as
// its own named endpoint rather than leaving a reader to guess.
const (
	adminPathHealth   = "/health"
	adminPathMetrics  = "/metrics"
	adminPathSegments = "/get_segments_detail"
)

// adminReadLimit caps one admin response.
//
// The three routes read here serve a small health document, an exposition of a few dozen families,
// and one entry per mounted segment. The largest of those grows with the cluster, so the cap is set
// far above any of them rather than snugly around today's shapes: at roughly 250 bytes per segment
// this is tens of thousands of segments, while still being a bound.
const adminReadLimit = 8 << 20

// The distinguishable failures. They are sentinels because the caller BRANCHES on them: one maps to
// a phase, one to a condition message, and the rest to a retry — and a caller that could only see
// "an error" would report a starting leader as a broken one.
var (
	// ErrServicePlaneInactive is the leader answering that it is not serving yet.
	//
	// It reaches only the gated routes. /health and /metrics answer 200 in every state, so a caller
	// asking those never sees this — which is the point: the same fact is available there as
	// service_ready, and the two agree by construction because the gate tests the very field
	// /health publishes.
	ErrServicePlaneInactive = errors.New("kvcache: leader's service plane is not active")

	// ErrMalformedBody is a reply that arrived and did not parse. It is distinct from a transport
	// failure on purpose: the first says this leader is answering with something unexpected, the
	// second says nothing answered, and only the first is worth showing an operator verbatim.
	ErrMalformedBody = errors.New("kvcache: leader returned a body that does not parse")
)

// LeaderHealth is the leader's /health document.
type LeaderHealth struct {
	// Status is A CONSTANT. The leader assigns "ok" unconditionally before filling anything
	// else in, so it reads ok on a leader that is not serving, has no leader view and holds no
	// segment. It is carried here only so a caller that logs the document logs what was received;
	// nothing may branch on it.
	Status string

	// Role and HAState describe. ServiceReady is the only field in the document that JUDGES, and
	// it is what every readiness decision here rests on.
	Role         string
	HAState      string
	ServiceReady bool

	// LeaderAddress and ViewVersion are absent on a single-leader backend, which has no leader
	// view to report. Their absence is normal and is not an error.
	LeaderAddress string
	ViewVersion   *uint64
}

// leaderHealthBody mirrors the wire document. It is separate from LeaderHealth so the pointer that
// distinguishes "absent" from "empty" stays at the boundary instead of spreading inward.
type leaderHealthBody struct {
	Status  string `json:"status"`
	Role    string `json:"role"`
	HAState string `json:"ha_state"`
	// A POINTER, unlike every other bool this package decodes, because this one field is the
	// whole readiness verdict. As a plain bool an absent or null service_ready is indistinguishable
	// from an explicit false, so a body that is valid JSON and not this document at all — `{}`, or
	// something else's health endpoint answering on a mistyped address — would read as "up but not
	// serving" and sit at Provisioning forever, waiting for a field nothing is going to send.
	ServiceReady  *bool   `json:"service_ready"`
	LeaderAddress *string `json:"leader_address"`
	ViewVersion   *uint64 `json:"view_version"`
}

// DecodeLeaderHealth reads the /health document.
func DecodeLeaderHealth(body []byte) (LeaderHealth, error) {
	var wire leaderHealthBody
	if err := json.Unmarshal(body, &wire); err != nil {
		return LeaderHealth{}, fmt.Errorf("%w: %s: %w", ErrMalformedBody, adminPathHealth, err)
	}

	// A document without this field is not this document. Everything else here is descriptive and
	// an empty string for it is survivable; service_ready is the verdict the phase turns on, so
	// treating its absence as false would report an unrelated JSON body as a leader that is up and
	// not serving — and that reads as a phase, so nothing would ever call it an error.
	if wire.ServiceReady == nil {
		return LeaderHealth{}, fmt.Errorf("%w: %s: no service_ready", ErrMalformedBody,
			adminPathHealth)
	}

	health := LeaderHealth{
		Status:       wire.Status,
		Role:         wire.Role,
		HAState:      wire.HAState,
		ServiceReady: *wire.ServiceReady,
		ViewVersion:  wire.ViewVersion,
	}
	if wire.LeaderAddress != nil {
		health.LeaderAddress = *wire.LeaderAddress
	}
	return health, nil
}

// The exposition families this operator reads. Nothing else is copied into status: the leader
// already serves its own counters on a scrapeable endpoint, and a status field would be a second
// and staler copy of them.
const (
	metricTotalCapacityBytes     = "master_total_capacity_bytes"
	metricAllocatedBytes         = "master_allocated_bytes"
	metricTotalFileCapacityBytes = "master_total_file_capacity_bytes"
	metricAllocatedFileSizeBytes = "master_allocated_file_size_bytes"
)

// LeaderCapacity is what the exposition reports.
//
// Every figure is a POINTER, and the distinction it carries is the whole reason this type exists: a
// family the exposition did not carry is nil, and nil is not zero. A zero would read as an empty
// cache — which is a real and different state — so a family that was not reported must not be able
// to impersonate one.
type LeaderCapacity struct {
	TotalBytes         *int64
	AllocatedBytes     *int64
	TotalFileBytes     *int64
	AllocatedFileBytes *int64
}

// DecodeLeaderCapacity reads the four families this operator uses out of a Prometheus exposition.
//
// It parses the text itself rather than through the upstream parser, and that is a deliberate trade:
// the upstream one requires setting a PROCESS-WIDE metric-name validation mode before first use, and
// this process already hosts the operator's own metrics registry. Reaching for four numbers is not
// worth a global switch that another registry in the same binary would inherit.
//
// The four are plain unlabelled gauges. The per-segment figures the leader also exposes carry both
// labels and different names, so the name lookup below excludes them without a rule of its own: a
// labeled sample's name includes its label set and matches nothing here.
func DecodeLeaderCapacity(body []byte) (LeaderCapacity, error) {
	wanted := map[string]**int64{}
	capacity := LeaderCapacity{}
	wanted[metricTotalCapacityBytes] = &capacity.TotalBytes
	wanted[metricAllocatedBytes] = &capacity.AllocatedBytes
	wanted[metricTotalFileCapacityBytes] = &capacity.TotalFileBytes
	wanted[metricAllocatedFileSizeBytes] = &capacity.AllocatedFileBytes

	var sawAnySample bool
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		sawAnySample = true

		target, ok := wanted[name]
		if !ok {
			continue
		}

		// A sample is `name value [timestamp]`, and the timestamp is OPTIONAL — so what Cut left in
		// `value` may be two numbers. Taking the first field rather than the whole remainder is the
		// difference between reading a capacity and refusing an exposition that is perfectly valid.
		value = strings.TrimSpace(value)
		if i := strings.IndexAny(value, " \t"); i >= 0 {
			value = value[:i]
		}

		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			// Only the REASON of strconv's error, because its own Error() quotes the whole input
			// back — so wrapping it whole would restore, unbounded, the very string quoted above
			// with a bound. errors.Unwrap on a *strconv.NumError yields ErrSyntax or ErrRange.
			return LeaderCapacity{}, fmt.Errorf("%w: %s: %s carries %q: %w",
				ErrMalformedBody, adminPathMetrics, name, quoteExternal(value), errors.Unwrap(err))
		}
		// The range is checked before the conversion, not after. ParseFloat accepts NaN and both
		// infinities, and converting a float64 that does not fit an int64 is UNDEFINED in Go — the
		// result is the architecture's, and the two this project builds disagree: measured, the
		// same sample gives the maximum int64 on arm64 and the MINIMUM on amd64. A malformed
		// exposition would therefore publish 8 exabytes on one worker and a negative capacity on
		// another. The body comes over HTTP from an address an external backend chose, so it is
		// not this operator's to trust.
		//
		// The bound is 2^63 and NOT math.MaxInt64, which is the trap this comparison is one of:
		// MaxInt64 is 2^63-1, a value float64 cannot hold, so converting it for this comparison
		// rounds it UP to 2^63 and "greater than" never fires. Comparing against 2^63 itself is
		// exact. It costs the top ~1024 int64 values, all of them above 8 exabytes.
		// A fractional sample is refused rather than truncated: int64(1.9) publishes 1 as a figure
		// the leader never reported, under a condition saying the capacity was observed.
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) ||
			parsed < 0 || parsed >= float64(uint64(1)<<63) ||
			parsed != math.Trunc(parsed) {
			return LeaderCapacity{}, fmt.Errorf("%w: %s: %s carries %q, which is not a byte count",
				ErrMalformedBody, adminPathMetrics, name, quoteExternal(value))
		}
		asInt := int64(parsed)
		*target = &asInt
	}

	// A body carrying no sample at all is not an exposition. A body carrying samples but not OUR
	// families is a legitimate outcome — every figure stays absent, and the caller reports that it
	// observed nothing rather than that it observed zero.
	if !sawAnySample && len(strings.TrimSpace(string(body))) > 0 {
		return LeaderCapacity{}, fmt.Errorf("%w: %s: no sample line found",
			ErrMalformedBody, adminPathMetrics)
	}

	return capacity, nil
}

// SegmentDetail is one entry of the leader's segment listing.
//
// Four fields out of the twelve the listing carries. The allocator byte counts are deliberately left
// behind: nothing in this scope reads a per-member capacity, and a field with no reader is one that
// goes stale without anybody noticing.
type SegmentDetail struct {
	// Name is the segment as the leader knows it.
	Name string
	// State is the leader's own segment state, passed through unchanged. It is NOT mapped to a
	// closed set here: this API publishes what the store reports, so a state a later store version
	// adds travels through rather than being flattened into a value that fits.
	State string
	// Protocol is what the member actually came up on, which is the requested transport only when
	// the request resolved the way the renderer predicted.
	Protocol string
	// TEEndpoint is the member's transfer-engine address, and it is how a listing entry is joined
	// back to the Pod it belongs to.
	TEEndpoint string
}

type segmentListingBody struct {
	TotalSegments uint64 `json:"total_segments"`
	// A POINTER, for the same reason leaderHealthBody.ServiceReady is one: absent is not empty. A
	// nil slice cannot tell "the leader knows of no segment" from a body that is valid JSON and not
	// this document — `{}` from a mistyped external endpoint, or another service answering on the
	// address — and the first is a state this API publishes while the second is a failed read. Read
	// as an empty listing it would clear membership and report NoSegments, which points an operator
	// at the store instead of at the address.
	Segments *[]struct {
		SegmentName string `json:"segment_name"`
		Status      string `json:"status"`
		Protocol    string `json:"protocol"`
		TEEndpoint  string `json:"te_endpoint"`
	} `json:"segments"`
}

// DecodeSegmentListing reads /get_segments_detail.
//
// An empty listing is a legitimate value meaning "the leader knows of no segment", and it is
// returned as an empty slice rather than as an error: a leader that is serving with nothing mounted
// is a real state, and the caller distinguishes it from a failed read by the error being nil.
//
// A listing with no segments FIELD is a different thing and is refused. `{}` is valid JSON and not
// this document — what a mistyped external endpoint or another service on that address returns —
// and read as an empty listing it would clear membership and report NoSegments, pointing an
// operator at the store rather than at the address. An actual empty listing is `"segments": []`.
func DecodeSegmentListing(body []byte) ([]SegmentDetail, error) {
	var wire segmentListingBody
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrMalformedBody, adminPathSegments, err)
	}

	// A segment's name is its IDENTITY, and this is the boundary where that has to hold. status
	// publishes the listing as a list-map keyed on it, so a blank or repeated name makes the whole
	// status update fail schema validation — including the condition the caller would have written
	// to report the trouble. Refusing the body here means the caller reports a listing it could not
	// read, which it can publish, instead of silently losing every field on the status.
	// Absent, so this is not a segment listing at all.
	if wire.Segments == nil {
		return nil, fmt.Errorf("%w: %s: no segments field", ErrMalformedBody, adminPathSegments)
	}

	segments := make([]SegmentDetail, 0, len(*wire.Segments))
	seen := make(map[string]struct{}, len(*wire.Segments))
	for _, s := range *wire.Segments {
		if s.SegmentName == "" {
			return nil, fmt.Errorf("%w: %s: a segment carries no name",
				ErrMalformedBody, adminPathSegments)
		}
		if _, dup := seen[s.SegmentName]; dup {
			return nil, fmt.Errorf("%w: %s: two segments are named %q",
				ErrMalformedBody, adminPathSegments, quoteExternal(s.SegmentName))
		}
		seen[s.SegmentName] = struct{}{}

		segments = append(segments, SegmentDetail{
			Name:       s.SegmentName,
			State:      s.Status,
			Protocol:   s.Protocol,
			TEEndpoint: s.TEEndpoint,
		})
	}
	return segments, nil
}

// AdminClient reads one leader's admin surface.
//
// It is a thin shell over the decoders: the decoders are where the meaning is and they are testable
// against recorded bodies alone, while this turns a transport outcome into one of the failures a
// caller branches on.
type AdminClient struct {
	// Address is the leader's admin endpoint as status publishes it, host:port.
	Address string
	// HTTP is the client to use. A caller supplies one carrying its own timeout, because a scrape
	// that hangs would hold a reconcile open for as long as the transport allows.
	HTTP *http.Client
}

// Health reads /health. It is never gated, so it answers whatever state the leader is in — which is
// why it is the document every readiness decision here rests on.
func (c *AdminClient) Health(ctx context.Context) (LeaderHealth, error) {
	body, err := c.get(ctx, adminPathHealth)
	if err != nil {
		return LeaderHealth{}, err
	}
	return DecodeLeaderHealth(body)
}

// Capacity reads /metrics.
//
// It is not gated either, and that has a consequence a caller must handle rather than this
// method: a leader whose service plane is not up still answers 200 here, with its capacity gauges
// reading zero because no segment has mounted. A successful read is therefore NOT evidence that
// anything was observed, and the caller gates publishing on service_ready.
func (c *AdminClient) Capacity(ctx context.Context) (LeaderCapacity, error) {
	body, err := c.get(ctx, adminPathMetrics)
	if err != nil {
		return LeaderCapacity{}, err
	}
	return DecodeLeaderCapacity(body)
}

// Segments reads /get_segments_detail, which IS gated: it answers ErrServicePlaneInactive until the
// leader is serving.
func (c *AdminClient) Segments(ctx context.Context) ([]SegmentDetail, error) {
	body, err := c.get(ctx, adminPathSegments)
	if err != nil {
		return nil, err
	}
	return DecodeSegmentListing(body)
}

// get performs one request and turns its outcome into a distinguishable failure.
func (c *AdminClient) get(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("http://%s%s", c.Address, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", path, err)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A transport failure travels as itself. It is neither malformed nor inactive: nothing
		// answered, which is a third thing, and flattening it would lose the address and the
		// syscall an operator needs.
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded, because the far end is not necessarily ours. An external backend's address names a
	// process an administrator declared, and a wrong address can name anything at all — including
	// something that streams without end.
	//
	// One byte MORE than the cap is read, so that hitting the cap is distinguishable from the body
	// ending there. Truncation cannot be left to the decoders: the exposition parser is
	// line-oriented, so a body cut mid-stream still yields every family before the cut and would
	// publish a partial scrape as an observed capacity.
	body, err := io.ReadAll(io.LimitReader(resp.Body, adminReadLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(body) > adminReadLimit {
		return nil, fmt.Errorf("%w: %s: the response is larger than %d bytes",
			ErrMalformedBody, path, adminReadLimit)
	}

	if resp.StatusCode == http.StatusServiceUnavailable {
		// The gated routes answer this until the leader's service plane comes up. It is a phase,
		// not a fault, and it must never be collapsed into a 404 — that one would mean this
		// operator asked for a path the leader does not serve, which is a bug here.
		return nil, fmt.Errorf("%w: %s: %s", ErrServicePlaneInactive, path, excerpt(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request %s: unexpected status %d: %s",
			path, resp.StatusCode, excerpt(body))
	}

	return body, nil
}

// adminErrorExcerpt caps how much of a failing response is quoted back.
//
// These errors become a condition message, and a condition message is capped at 32 KiB by the
// schema. A body over that would make every status write fail validation — so the reconciler would
// be unable to report the very fault it was trying to report, for as long as the far end kept
// answering that way. The far end is not necessarily ours, so the size of what it says is not this
// operator's to assume: the excerpt is short enough to leave the rest of the message room.
const adminErrorExcerpt = 512

// excerpt renders a response body for an error message, bounded and on one line.
func excerpt(body []byte) string {
	return quoteExternal(strings.TrimSpace(string(body)))
}

// quoteExternal bounds one string that came off an admin response, on its way into an error that
// becomes a condition message.
//
// The response as a whole is bounded at adminReadLimit, which is 8 MiB and says nothing about how
// that is spread across its fields: one metric sample, one segment name or one body can hold all of
// it. Runes rather than bytes, because a rune is the unit the schema's limit counts in and because
// cutting between them is what keeps a multi-byte character intact.
func quoteExternal(text string) string {
	return stringx.TruncateRunes(text, adminErrorExcerpt, "… (truncated)")
}

// segmentStates maps the store's spelling of a segment state onto this API's.
//
// It is the third place the two vocabularies meet, after the allocation strategy and the transport,
// and it is the only one that translates INWARD — the other two render a request, this one publishes
// a report.
//
// The store's six: UNDEFINED, OK, DRAINING, DRAINED, GRACEFULLY_UNMOUNTING, UNMOUNTING. OK keeps its
// spelling because it is an initialism, which is what this group's enums do with those.
var segmentStates = map[string]string{
	"UNDEFINED":             "Undefined",
	"OK":                    "OK",
	"DRAINING":              "Draining",
	"DRAINED":               "Drained",
	"GRACEFULLY_UNMOUNTING": "GracefullyUnmounting",
	"UNMOUNTING":            "Unmounting",
}

// SegmentState translates a reported state into this API's casing.
//
// A state this version does not know is returned UNCHANGED rather than replaced with a fallback.
// The store owns this vocabulary: a version that adds a state is reporting something real, and
// flattening it into "Unknown" would turn new information into no information. The field carries no
// enum precisely so that this passes through.
func SegmentState(reported string) string {
	if mapped, ok := segmentStates[reported]; ok {
		return mapped
	}
	return reported
}
