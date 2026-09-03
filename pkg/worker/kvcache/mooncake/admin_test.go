package mooncake

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

// fixture reads a recorded payload. Every decoder case runs against one of these rather than against
// a literal built in the test, so what is asserted is a shape the artifact actually produces.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err, "fixture %s must exist", name)
	return body
}

func TestAdminDecodeHealth(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    LeaderHealth
	}{
		{
			name:    "a serving single leader reports no leader view",
			fixture: "health-ready.json",
			want: LeaderHealth{
				Status:       "ok",
				Role:         "leader",
				HAState:      "serving",
				ServiceReady: true,
			},
		},
		{
			name:    "a leader that is up but not serving",
			fixture: "health-not-ready.json",
			want: LeaderHealth{
				Status:       "ok",
				Role:         "leader",
				HAState:      "starting",
				ServiceReady: false,
			},
		},
		{
			name:    "a leader view is carried when the document has one",
			fixture: "health-with-leader-view.json",
			want: LeaderHealth{
				Status:        "ok",
				Role:          "follower",
				HAState:       "serving",
				ServiceReady:  true,
				LeaderAddress: "10.42.0.7:50051",
				ViewVersion:   ptr.To[uint64](42),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DecodeLeaderHealth(fixture(t, c.fixture))
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestAdminDecodeHealth_StatusIsNotAVerdict is the case the F6 correction exists for.
//
// "status": "ok" is assigned unconditionally by the leader before anything else is filled in. A
// reader that took it for a verdict would call a leader ready while its service plane is down. The
// two fixtures below differ ONLY in service_ready, and they both say ok.
func TestAdminDecodeHealth_StatusIsNotAVerdict(t *testing.T) {
	ready, err := DecodeLeaderHealth(fixture(t, "health-ready.json"))
	require.NoError(t, err)
	notReady, err := DecodeLeaderHealth(fixture(t, "health-not-ready.json"))
	require.NoError(t, err)

	assert.Equal(t, "ok", ready.Status)
	assert.Equal(t, "ok", notReady.Status,
		"a leader that is NOT serving still says ok; the field is a constant")
	assert.NotEqual(t, ready.ServiceReady, notReady.ServiceReady,
		"service_ready is the only field in the document that carries a verdict")
}

func TestAdminDecodeHealth_Malformed(t *testing.T) {
	_, err := DecodeLeaderHealth(fixture(t, "health-malformed.json"))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMalformedBody,
		"a body that arrived and did not parse is its own outcome, not a transport failure")
}

// TestAdminDecodeHealth_WithoutServiceReadyIsMalformed covers the body that parses and is not this
// document.
//
// service_ready is the whole readiness verdict, so decoding it into a plain bool would make an
// absent field indistinguishable from an explicit false — and false is a PHASE here, not a fault.
// A `{}`, or something else's health endpoint answering on a mistyped address, would then read as a
// leader that is up and not serving yet, and the backend would wait at Provisioning forever for a
// field nothing is ever going to send.
func TestAdminDecodeHealth_WithoutServiceReadyIsMalformed(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"ok","role":"leader"}`, `{"service_ready":null}`} {
		t.Run(body, func(t *testing.T) {
			_, err := DecodeLeaderHealth([]byte(body))

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrMalformedBody)
			assert.Contains(t, err.Error(), "no service_ready")
		})
	}

	// The field present and false is the phase it has always been, not an error.
	health, err := DecodeLeaderHealth([]byte(`{"status":"ok","role":"leader","service_ready":false}`))
	require.NoError(t, err)
	assert.False(t, health.ServiceReady)
}

func TestAdminDecodeCapacity(t *testing.T) {
	t.Run("the four families are read and the rest ignored", func(t *testing.T) {
		got, err := DecodeLeaderCapacity(fixture(t, "metrics-full.txt"))
		require.NoError(t, err)

		assert.Equal(t, LeaderCapacity{
			TotalBytes:         ptr.To[int64](1082331758592),
			AllocatedBytes:     ptr.To[int64](5476083302),
			TotalFileBytes:     ptr.To[int64](0),
			AllocatedFileBytes: ptr.To[int64](0),
		}, got, "master_key_count and master_active_clients are present in the fixture and must "+
			"not appear here: neither has a reader")
	})

	t.Run("a labelled series of a similar name is not mistaken for a family", func(t *testing.T) {
		got, err := DecodeLeaderCapacity(fixture(t, "metrics-full.txt"))
		require.NoError(t, err)

		require.NotNil(t, got.TotalBytes)
		assert.Equal(t, int64(1082331758592), *got.TotalBytes,
			"the fixture also carries two segment_total_capacity_bytes series; the global gauge wins")
	})

	t.Run("scientific notation is a value, not a parse failure", func(t *testing.T) {
		got, err := DecodeLeaderCapacity(fixture(t, "metrics-scientific.txt"))
		require.NoError(t, err)

		require.NotNil(t, got.TotalBytes)
		assert.Equal(t, int64(1082331758592), *got.TotalBytes)
		require.NotNil(t, got.AllocatedBytes)
		assert.Equal(t, int64(5476083302), *got.AllocatedBytes)
	})
}

// TestAdminDecodeCapacity_MissingFamilyIsAbsentNotZero is the distinction the whole type exists for.
// An exposition that does not carry a family leaves it nil, and nil is not zero: a zero would read
// as an empty cache, which is a real and different state.
func TestAdminDecodeCapacity_MissingFamilyIsAbsentNotZero(t *testing.T) {
	got, err := DecodeLeaderCapacity(fixture(t, "metrics-missing-family.txt"))

	require.NoError(t, err, "a valid exposition without our families is not an error")
	assert.Nil(t, got.TotalBytes, "absent, not zero")
	assert.Nil(t, got.AllocatedBytes)
	assert.Nil(t, got.TotalFileBytes)
	assert.Nil(t, got.AllocatedFileBytes)
}

// TestAdminDecodeCapacity_ZeroIsAValue is the other half of the same distinction, and it is the case
// a starting leader produces: /metrics is not gated, so it answers 200 with its gauges at zero
// before anything has mounted. The decoder reports the zero faithfully — deciding that a zero read
// under service_ready: false must not be published is the CALLER's job, and it can only do that if
// the decoder does not hide the zero here.
func TestAdminDecodeCapacity_ZeroIsAValue(t *testing.T) {
	got, err := DecodeLeaderCapacity(fixture(t, "metrics-zeroed.txt"))

	require.NoError(t, err)
	require.NotNil(t, got.TotalBytes, "a reported zero is present, and distinguishable from absent")
	assert.Equal(t, int64(0), *got.TotalBytes)
}

func TestAdminDecodeCapacity_Malformed(t *testing.T) {
	_, err := DecodeLeaderCapacity(fixture(t, "metrics-malformed.txt"))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMalformedBody)
	assert.Contains(t, err.Error(), "master_total_capacity_bytes",
		"the message names the family that would not parse, so an operator knows where to look")
}

// TestAdminDecodeCapacity_ANumberThatIsNotAByteCountIsMalformed covers the sample that parses as a
// float and is not a capacity.
//
// ParseFloat accepts NaN and both infinities, and converting a float64 that does not fit an int64
// is undefined in Go — on the platforms this runs on it lands on the minimum int64. So an unchecked
// cast turned a malformed exposition into a large NEGATIVE capacity, which then read as an observed
// figure and published as one. The body arrives over HTTP from an address an external backend
// chose, so none of these is a hypothetical.
func TestAdminDecodeCapacity_ANumberThatIsNotAByteCountIsMalformed(t *testing.T) {
	// The last three sit on the boundary the first version of this guard got wrong. It compared
	// against math.MaxInt64, which is 2^63-1 — a value float64 cannot hold — so converting it for
	// the comparison rounded it UP to 2^63 and "greater than" never fired. Everything from 2^63
	// upward was admitted, and int64() on it is undefined in Go: measured, the same sample gives
	// the maximum int64 on arm64 and the MINIMUM on amd64, both of which this project ships.
	for _, value := range []string{
		"NaN", "+Inf", "-Inf", "-1", "-4096", "1e30", "18446744073709551616",
		"9223372036854775808", // exactly 2^63
		"9223372036854775807", // MaxInt64, which ParseFloat rounds up to 2^63
		"9223372036854775296", // fits an int64, and still rounds up to 2^63
		// Fractional. int64() truncates rather than refuses, so each of these used to be published
		// as a figure the leader never reported — under a condition saying capacity was observed.
		"1.9",
		"0.5",
		"4096.0000001",
	} {
		t.Run(value, func(t *testing.T) {
			body := []byte("master_total_capacity_bytes " + value + "\n")

			_, err := DecodeLeaderCapacity(body)

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrMalformedBody)
			assert.Contains(t, err.Error(), "is not a byte count")
			assert.Contains(t, err.Error(), "master_total_capacity_bytes",
				"the message names the family, so an operator knows which gauge to look at")
		})
	}

	// The boundaries stay readable: zero is a value this decoder reports faithfully, and a figure
	// that fits is unaffected by the guard.
	capacity, err := DecodeLeaderCapacity([]byte("master_total_capacity_bytes 0\n"))
	require.NoError(t, err)
	require.NotNil(t, capacity.TotalBytes)
	assert.Equal(t, int64(0), *capacity.TotalBytes)

	// A Prometheus sample is `name value [timestamp]`, and the timestamp is optional — so a
	// perfectly valid exposition can carry two numbers where this parser reads one. Refusing it
	// would make capacity disappear against a leader doing nothing wrong.
	capacity, err = DecodeLeaderCapacity(
		[]byte("master_total_capacity_bytes 1082331758592 1720000000000\n"))
	require.NoError(t, err)
	require.NotNil(t, capacity.TotalBytes)
	assert.Equal(t, int64(1082331758592), *capacity.TotalBytes,
		"the value is read and the timestamp ignored, not concatenated into it")

	// And the guard above must not catch this one. A Prometheus exposition writes gauges through
	// %g, so a figure at cache scale arrives in scientific notation — 500Gi is 5.36870912e+11 —
	// and it is integral however it is spelled. Refusing it would reject the normal case.
	capacity, err = DecodeLeaderCapacity([]byte("master_total_capacity_bytes 5.36870912e+11\n"))
	require.NoError(t, err)
	require.NotNil(t, capacity.TotalBytes)
	assert.Equal(t, int64(536870912000), *capacity.TotalBytes)
}

func TestAdminDecodeSegments(t *testing.T) {
	t.Run("four fields are read and the allocator counts left behind", func(t *testing.T) {
		got, err := DecodeSegmentListing(fixture(t, "segments-detail.json"))
		require.NoError(t, err)

		assert.Equal(t, []SegmentDetail{
			{Name: "n7-dram", State: "OK", Protocol: "tcp", TEEndpoint: "10.42.0.11:15002"},
			{Name: "n8-dram", State: "OK", Protocol: "rdma", TEEndpoint: "10.42.0.12:15002"},
		}, got, "the fixture carries twelve fields per entry; nothing in this scope reads the rest")
	})

	t.Run("a body with no segments field is not an empty listing", func(t *testing.T) {
		// `{}` is valid JSON and not this document — what a mistyped external endpoint, or another
		// service answering on that address, returns. Decoded as an empty listing it would clear
		// membership and report NoSegments, which points an operator at the store rather than at
		// the address they got wrong.
		for _, body := range []string{`{}`, `{"total_segments":0}`, `{"segments":null}`} {
			_, err := DecodeSegmentListing([]byte(body))
			require.Error(t, err, "body %s", body)
			assert.ErrorIs(t, err, ErrMalformedBody)
			assert.Contains(t, err.Error(), "no segments field")
		}

		// The real empty listing still decodes, and to an empty slice rather than an error: a
		// leader serving with nothing mounted is a state this API publishes.
		got, err := DecodeSegmentListing([]byte(`{"total_segments":0,"segments":[]}`))
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("a draining segment is still listed, and says so", func(t *testing.T) {
		got, err := DecodeSegmentListing(fixture(t, "segments-detail-draining.json"))
		require.NoError(t, err)

		require.Len(t, got, 1)
		assert.Equal(t, "DRAINING", got[0].State,
			"a shrink is observable BECAUSE the state travels through; three values could not "+
				"tell draining from gone")
	})

	t.Run("a state this version does not know travels through unchanged", func(t *testing.T) {
		got, err := DecodeSegmentListing(fixture(t, "segments-detail-unknown-state.json"))
		require.NoError(t, err)

		require.Len(t, got, 1)
		assert.Equal(t, "QUIESCING", got[0].State,
			"the store owns this vocabulary; a value invented by a later version is published "+
				"verbatim rather than flattened into one that fits")
	})

	t.Run("an empty listing is a value and not a failure", func(t *testing.T) {
		got, err := DecodeSegmentListing(fixture(t, "segments-detail-empty.json"))

		require.NoError(t, err, "a leader serving with nothing mounted is a real state")
		assert.Empty(t, got)
	})
}

// TestAdminDecodeSegments_ANameThatCannotBeAKeyIsMalformed guards the boundary status depends on.
//
// status.members is a list-map keyed on segmentName, so a blank or repeated name makes the whole
// status update fail schema validation — including the condition the caller would have written to
// report the trouble. Refused here, the caller reports a listing it could not read, which it can
// actually publish.
func TestAdminDecodeSegments_ANameThatCannotBeAKeyIsMalformed(t *testing.T) {
	t.Run("a segment with no name", func(t *testing.T) {
		_, err := DecodeSegmentListing([]byte(
			`{"total_segments":1,"segments":[{"segment_name":"","te_endpoint":"n7:1","status":"OK"}]}`))

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMalformedBody)
		assert.Contains(t, err.Error(), "carries no name")
	})

	t.Run("two segments with one name", func(t *testing.T) {
		_, err := DecodeSegmentListing([]byte(
			`{"total_segments":2,"segments":[` +
				`{"segment_name":"n7:13775","te_endpoint":"n7:15380","status":"OK"},` +
				`{"segment_name":"n7:13775","te_endpoint":"n7:15381","status":"OK"}]}`))

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMalformedBody)
		assert.Contains(t, err.Error(), `two segments are named "n7:13775"`)
	})
}

func TestAdminDecodeSegments_Malformed(t *testing.T) {
	_, err := DecodeSegmentListing(fixture(t, "segments-detail-malformed.json"))

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMalformedBody)
}

// TestAdminClient_BoundsWhatItReadsAndWhatItQuotes covers the two ways a far end's SIZE reaches this
// operator. Neither is hypothetical: an external backend's address names a process an administrator
// declared, and a wrong address can name anything at all.
func TestAdminClient_BoundsWhatItReadsAndWhatItQuotes(t *testing.T) {
	t.Run("a body over the cap is a failure, not a truncated success", func(t *testing.T) {
		// A valid exposition, then enough padding to pass the cap. Truncation cannot be left to
		// the decoder: the parser is line-oriented, so the first family below would parse and
		// publish as an observed capacity from a scrape that never finished.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("master_total_capacity_bytes 1082331758592\n"))
			_, _ = w.Write([]byte(strings.Repeat("# padding\n", (adminReadLimit/10)+1)))
		}))
		defer srv.Close()

		_, err := newTestAdminClient(srv).Capacity(context.Background())

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMalformedBody)
		assert.Contains(t, err.Error(), "larger than")
	})

	t.Run("a failing response is quoted in an excerpt, not whole", func(t *testing.T) {
		// The error becomes a condition message, and the schema caps that at 32 KiB. Quoting a
		// body whole would make every status write fail validation — so the reconciler could not
		// report the fault it was trying to report, for as long as the far end kept answering.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
		}))
		defer srv.Close()

		_, err := newTestAdminClient(srv).Segments(context.Background())

		require.Error(t, err)
		assert.Less(t, len(err.Error()), 2048,
			"the message stays far inside the condition's own 32 KiB cap")
		assert.Contains(t, err.Error(), "(truncated)",
			"and it says it was cut, so nobody reads the excerpt as the whole answer")
	})

	// The excerpt above bounds a body the transport rejected. A body that ARRIVES intact and then
	// fails to parse is quoted by the parser instead, and adminReadLimit says only that the whole
	// response fits 8 MiB — nothing says how that is spread across its fields, so one sample or one
	// segment name can hold all of it.
	t.Run("a parser quoting a field of the body bounds it too", func(t *testing.T) {
		huge := strings.Repeat("9", 1<<20)

		for name, body := range map[string]string{
			"a sample that is not a number": metricTotalCapacityBytes + " " + huge + "x\n",
			"a sample that is not a count":  metricTotalCapacityBytes + " " + huge + ".5\n",
		} {
			_, err := DecodeLeaderCapacity([]byte(body))
			require.Error(t, err, name)
			assert.Less(t, utf8.RuneCountInString(err.Error()), 32768, name)
			assert.Contains(t, err.Error(), "(truncated)", name)
			assert.Contains(t, err.Error(), metricTotalCapacityBytes, name,
				"and the family is still named, which is what says where to look")
		}

		// strconv's own error quotes the whole input back, so the reason has to be carried without
		// it — and carrying nothing at all would lose the difference between a syntax failure and
		// a value out of range.
		_, err := DecodeLeaderCapacity([]byte(metricTotalCapacityBytes + " " + huge + "x\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid syntax",
			"the reason survives; only strconv's copy of the input does not")

		listing := fmt.Sprintf(
			`{"total_segments":2,"segments":[`+
				`{"segment_name":%q,"te_endpoint":"a:1","protocol":"tcp","status":"OK"},`+
				`{"segment_name":%q,"te_endpoint":"a:2","protocol":"tcp","status":"OK"}]}`,
			huge, huge)

		_, err = DecodeSegmentListing([]byte(listing))
		require.Error(t, err)
		assert.Less(t, utf8.RuneCountInString(err.Error()), 32768,
			"a duplicate segment name is quoted back, and the name came off the wire")
		assert.Contains(t, err.Error(), "(truncated)")
	})
}

// TestAdminClient_DistinguishesEveryFailure is the point of the sentinels. The caller branches on
// these — one maps to a phase, one to a condition message, one to a retry — so this asserts that no
// two of them arrive looking the same.
func TestAdminClient_DistinguishesEveryFailure(t *testing.T) {
	t.Run("503 on a gated route is the service plane, not a fault", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(fixture(t, "service-plane-inactive.txt"))
		}))
		defer srv.Close()

		_, err := newTestAdminClient(srv).Segments(context.Background())

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServicePlaneInactive)
		assert.NotErrorIs(t, err, ErrMalformedBody,
			"a leader that is starting is not a leader that is broken")
		assert.Contains(t, err.Error(), "service plane is not active",
			"the leader's own words are carried, so a log says what the leader said")
	})

	t.Run("a body that does not parse is malformed and not inactive", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(fixture(t, "health-malformed.json"))
		}))
		defer srv.Close()

		_, err := newTestAdminClient(srv).Health(context.Background())

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMalformedBody)
		assert.NotErrorIs(t, err, ErrServicePlaneInactive)
	})

	t.Run("nothing answering is neither", func(t *testing.T) {
		// A port nothing listens on: the connection is refused rather than timing out, so the test
		// stays fast and the outcome is the one a down leader actually produces.
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		address := listener.Addr().String()
		require.NoError(t, listener.Close())

		client := &AdminClient{Address: address, HTTP: &http.Client{Timeout: 2 * time.Second}}
		_, err = client.Health(context.Background())

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrMalformedBody,
			"nothing answered, so there is no body to call malformed")
		assert.NotErrorIs(t, err, ErrServicePlaneInactive,
			"nothing answered, so nothing said it was inactive")
		assert.Contains(t, err.Error(), address,
			"the address is in the message: an operator needs to know what was unreachable")
	})

	t.Run("an unexpected status is its own outcome", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		_, err := newTestAdminClient(srv).Segments(context.Background())

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrServicePlaneInactive,
			"404 must never be collapsed into 503: the first means this operator asked for a "+
				"path the leader does not serve, which is a bug here, not a phase")
		assert.Contains(t, err.Error(), "404")
	})
}

// TestAdminClient_ReadsEachRouteFromItsOwnPath pins that the client asks the paths it means to. A
// method pointed at the wrong route would decode a body of the wrong shape and be reported as
// malformed, which would send an operator looking at the leader instead of at this code.
func TestAdminClient_ReadsEachRouteFromItsOwnPath(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write(fixture(t, "health-ready.json"))
		case "/metrics":
			_, _ = w.Write(fixture(t, "metrics-full.txt"))
		case "/get_segments_detail":
			_, _ = w.Write(fixture(t, "segments-detail.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := newTestAdminClient(srv)
	ctx := context.Background()

	health, err := client.Health(ctx)
	require.NoError(t, err)
	assert.True(t, health.ServiceReady)

	capacity, err := client.Capacity(ctx)
	require.NoError(t, err)
	require.NotNil(t, capacity.TotalBytes)

	segments, err := client.Segments(ctx)
	require.NoError(t, err)
	assert.Len(t, segments, 2)

	assert.Equal(t, []string{"/health", "/metrics", "/get_segments_detail"}, asked)
}

func newTestAdminClient(srv *httptest.Server) *AdminClient {
	return &AdminClient{
		Address: srv.Listener.Addr().String(),
		HTTP:    srv.Client(),
	}
}

// TestAdminClient_ErrorsAreComparable pins that the sentinels survive wrapping. The client wraps
// every failure with the path it was reading, so a caller using == instead of errors.Is would see
// none of them.
func TestAdminClient_ErrorsAreComparable(t *testing.T) {
	wrapped := errors.Join(ErrServicePlaneInactive, errors.New("context"))
	assert.ErrorIs(t, wrapped, ErrServicePlaneInactive)
	assert.NotErrorIs(t, wrapped, ErrMalformedBody)
}

func TestAdminSegmentState(t *testing.T) {
	t.Run("the store's six translate into this API's casing", func(t *testing.T) {
		assert.Equal(t, map[string]string{
			"UNDEFINED":             "Undefined",
			"OK":                    "OK",
			"DRAINING":              "Draining",
			"DRAINED":               "Drained",
			"GRACEFULLY_UNMOUNTING": "GracefullyUnmounting",
			"UNMOUNTING":            "Unmounting",
		}, map[string]string{
			"UNDEFINED":             SegmentState("UNDEFINED"),
			"OK":                    SegmentState("OK"),
			"DRAINING":              SegmentState("DRAINING"),
			"DRAINED":               SegmentState("DRAINED"),
			"GRACEFULLY_UNMOUNTING": SegmentState("GRACEFULLY_UNMOUNTING"),
			"UNMOUNTING":            SegmentState("UNMOUNTING"),
		}, "all six, so one added to the store shows up here as a missing case rather than silently")
	})

	t.Run("a state this version does not know passes through", func(t *testing.T) {
		assert.Equal(t, "QUIESCING", SegmentState("QUIESCING"),
			"the store owns this vocabulary; flattening a new state into a fallback would turn "+
				"new information into none")
	})

	t.Run("an empty report stays empty", func(t *testing.T) {
		assert.Empty(t, SegmentState(""),
			"nothing reported is not a state, and must not become one")
	})
}
