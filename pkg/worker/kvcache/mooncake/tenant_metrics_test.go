package mooncake

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

// TestDecodeTenantQuotaMetrics_AByteCountIsReadExactly pins the values a float64 cannot carry.
//
// These series are byte counts, and float64 stops holding an integer exactly at 2^53. Read through
// one, 2^53+1 comes back as 2^53 — whole, non-negative, in range — so every guard passes and the
// reader publishes a figure the master never reported. The upper end is not a corner either: the
// artifact accepts a quota anywhere in [1, 2^63-1], and 2^63-1 read through a float rounds UP to
// 2^63, which the range guard then refuses outright.
func TestDecodeTenantQuotaMetrics_AByteCountIsReadExactly(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  int64
	}{
		{
			name:  "the first integer float64 cannot represent",
			value: "9007199254740993",
			want:  int64(1)<<53 + 1,
		},
		{
			name:  "the largest quota the artifact accepts",
			value: "9223372036854775807",
			want:  math.MaxInt64,
		},
		{
			name:  "the exposition format's other spelling of a whole number",
			value: "1.5e+09",
			want:  1500000000,
		},
		{
			name:  "a whole number written with a fraction",
			value: "12.0",
			want:  12,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeTenantQuotaMetrics([]byte(fmt.Sprintf(
				"%s{tenant_id=\"team-a\"} %s\n", metricTenantUsedBytes, tc.value)))
			require.NoError(t, err)

			sample, ok := got.Tenant("team-a")
			require.True(t, ok)
			require.NotNil(t, sample.UsedBytes)
			assert.Equal(t, tc.want, *sample.UsedBytes,
				"the reader must publish what the master wrote, not the nearest float64 to it")
		})
	}
}

// TestDecodeTenantQuotaMetrics reads the recorded document. Its eleven series are what the measured
// build serialized, so the figures below are transcribed rather than constructed.
func TestDecodeTenantQuotaMetrics(t *testing.T) {
	t.Run("a tenant's eight series are read as that tenant's own", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics(fixture(t, "tenant-metrics.txt"))
		require.NoError(t, err)

		sample, ok := got.Tenant("team-a")
		require.True(t, ok)
		assert.Equal(t, TenantSample{
			RequestedBytes:      ptr.To[int64](1073741824),
			EffectiveBytes:      ptr.To[int64](0),
			UsedBytes:           ptr.To[int64](0),
			ReservedBytes:       ptr.To[int64](0),
			CommittedCount:      ptr.To[int64](0),
			MetadataObjectCount: ptr.To[int64](0),
			OverQuota:           ptr.To(false),
			ExplicitPolicy:      ptr.To(true),
		}, sample, "the document also carries master_* families and a second tenant; neither reaches "+
			"this sample")
	})

	t.Run("a second tenant in the same document is a second sample, never a sum", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics(fixture(t, "tenant-metrics.txt"))
		require.NoError(t, err)

		teamA, ok := got.Tenant("team-a")
		require.True(t, ok)
		teamB, ok := got.Tenant("team-b")
		require.True(t, ok)

		require.NotNil(t, teamA.RequestedBytes)
		require.NotNil(t, teamB.RequestedBytes)
		assert.Equal(t, int64(1073741824), *teamA.RequestedBytes)
		assert.Equal(t, int64(4294967296), *teamB.RequestedBytes,
			"each Binding reads the series bearing its own domain name; no path adds two together")
	})

	t.Run("the three global gauges come out of the same document", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics(fixture(t, "tenant-metrics.txt"))
		require.NoError(t, err)

		assert.Equal(t, ptr.To[int64](0), got.AllocatableCapacityBytes,
			"zero here is the startup-ordering trap: no member has mounted, so every effective "+
				"quota is zero while every object still looks configured")
		assert.Equal(t, ptr.To[int64](5368709120), got.RequestedBytesSum)
		assert.Equal(t, ptr.To[int64](0), got.EffectiveBytesSum)
		assert.Len(t, got.Tenants, 2,
			"one scrape carries the globals and every tenant, which is why a pass reads it once")
	})
}

// TestDecodeTenantQuotaMetrics_ReadsBothGenerations is about a breaking change upstream made between
// the two builds a backend's spec can name.
//
// 0.3.13 dropped four of the per-tenant series and added one, so a decoder written against either
// build alone reads NOTHING of the other's occupancy — and reads it silently, because an absent
// series is an absent pointer and that is a legitimate answer here. Both fixtures go through the same
// decoder, and Occupancy is what hides the difference from everyone above it.
func TestDecodeTenantQuotaMetrics_ReadsBothGenerations(t *testing.T) {
	t.Run("0.3.12.post1 splits occupancy, so reservations are excluded", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics(fixture(t, "tenant-metrics.txt"))
		require.NoError(t, err)

		sample, ok := got.Tenant("team-a")
		require.True(t, ok)
		bytes, inflight := sample.Occupancy()
		require.NotNil(t, bytes)
		assert.Equal(t, int64(0), *bytes)
		assert.False(t, inflight, "used_bytes counts a write at PutEnd, so nothing in-flight is in it")
		assert.Nil(t, sample.ChargedBytes, "this build emits no such series")
	})

	t.Run("0.3.13 has one occupancy figure, and it includes reservations", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics(fixture(t, "tenant-metrics-0313.txt"))
		require.NoError(t, err)

		sample, ok := got.Tenant("team-a")
		require.True(t, ok)
		bytes, inflight := sample.Occupancy()
		require.NotNil(t, bytes)
		assert.Equal(t, int64(268435456), *bytes)
		assert.True(t, inflight,
			"charged_bytes counts a write from PutStart, and there is no second series to subtract")

		assert.Nil(t, sample.UsedBytes, "the older pair is gone from this build")
		assert.Nil(t, sample.ReservedBytes)
		assert.Nil(t, sample.CommittedCount, "and so are both object counts, with nothing replacing them")
		assert.Nil(t, sample.MetadataObjectCount)
		assert.Equal(t, ptr.To(false), sample.AdmissionClosed,
			"admission_closed is new here, and it is not a synonym for over_quota")
	})

	// No build observed so far emits both, so this pins a TIE-BREAK rather than a case in the wild.
	// It is here because Occupancy's doc claims one, and a claimed order with no test is one a later
	// edit reverses silently — which would start reporting an occupancy that includes reservations
	// while still saying it does not.
	t.Run("a document carrying both prefers the figure that excludes reservations", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics([]byte(
			metricTenantUsedBytes + `{tenant_id="team-a"} 100` + "\n" +
				metricTenantChargedBytes + `{tenant_id="team-a"} 900` + "\n"))
		require.NoError(t, err)

		sample, _ := got.Tenant("team-a")
		bytes, inflight := sample.Occupancy()
		require.NotNil(t, bytes)
		assert.Equal(t, int64(100), *bytes, "the more precise figure wins")
		assert.False(t, inflight)
	})

	t.Run("a tenant with neither series reports no occupancy rather than zero", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics([]byte(
			metricTenantRequestedBytes + `{tenant_id="team-a"} 1073741824` + "\n"))
		require.NoError(t, err)

		sample, _ := got.Tenant("team-a")
		bytes, inflight := sample.Occupancy()
		assert.Nil(t, bytes)
		assert.False(t, inflight, "no figure means no claim about what is in it")
	})
}

// TestDecodeTenantQuotaMetrics_AbsentIsNeverZero is the distinction the pointers exist for.
//
// The master serializes only label combinations it has observed, so a tenant missing from a document
// may be one that has never been written rather than one reading zero. Reported as zero, a Binding
// whose quota has not landed yet would publish a granted quota of nothing and a usage of nothing —
// both of which are figures the master never gave.
func TestDecodeTenantQuotaMetrics_AbsentIsNeverZero(t *testing.T) {
	got, err := DecodeTenantQuotaMetrics(fixture(t, "tenant-metrics.txt"))
	require.NoError(t, err)

	sample, ok := got.Tenant("team-never-written")
	assert.False(t, ok, "the document did not mention this tenant")
	assert.Nil(t, sample.RequestedBytes, "absent, not zero")
	assert.Nil(t, sample.EffectiveBytes)
	assert.Nil(t, sample.UsedBytes)
	assert.Nil(t, sample.OverQuota)

	// And a tenant the document DOES carry with every figure at zero is present, which is the other
	// half of the same distinction: it is a quiet tenant, not an unknown one.
	quiet, ok := got.Tenant("team-b")
	require.True(t, ok)
	require.NotNil(t, quiet.UsedBytes)
	assert.Equal(t, int64(0), *quiet.UsedBytes)
}

// TestDecodeTenantQuotaMetrics_AFamilyThatNeverFiredIsAbsentToo covers the same rule one level down.
// A per-tenant family emits no line at all before its first observation, so a tenant can appear with
// only some of its eight series — and the ones it did not carry stay nil rather than becoming zero.
func TestDecodeTenantQuotaMetrics_AFamilyThatNeverFiredIsAbsentToo(t *testing.T) {
	got, err := DecodeTenantQuotaMetrics([]byte(
		metricTenantRequestedBytes + `{tenant_id="team-a"} 1073741824` + "\n"))
	require.NoError(t, err)

	sample, ok := got.Tenant("team-a")
	require.True(t, ok)
	require.NotNil(t, sample.RequestedBytes)
	assert.Equal(t, int64(1073741824), *sample.RequestedBytes)
	assert.Nil(t, sample.UsedBytes, "a family that emitted no line says nothing, not zero")
	assert.Nil(t, got.AllocatableCapacityBytes, "and neither does a global gauge that was not there")
}

// TestDecodeTenantQuotaMetrics_MalformedIsAnErrorNotAPartialSampleSet is why this parser is stricter
// than the capacity one. It is line-oriented, so a body cut mid-stream yields every series before
// the cut — and publishing that prefix would report a tenant that sat after the cut as ABSENT, which
// is the one thing this reader's absences are supposed to mean.
func TestDecodeTenantQuotaMetrics_MalformedIsAnErrorNotAPartialSampleSet(t *testing.T) {
	cases := []struct {
		name string
		body string
		says string
	}{
		{
			name: "a sample whose value is not a number",
			body: metricTenantUsedBytes + `{tenant_id="team-a"} not-a-number` + "\n",
			says: metricTenantUsedBytes,
		},
		{
			name: "a sample that is not a byte count",
			body: metricTenantUsedBytes + `{tenant_id="team-a"} -1` + "\n",
			says: "is not a count",
		},
		{
			// The exact path takes this value verbatim when it is spelled as digits. Spelled as a
			// float it cannot be carried at all — 9007199254740993.0 parses to ...992 — so it is
			// refused rather than published as a figure one bit off what the master wrote.
			name: "a float spelling of a count past what float64 can carry",
			body: metricTenantUsedBytes + `{tenant_id="team-a"} 9007199254740993.0` + "\n",
			says: "is not a count",
		},
		{
			name: "a float spelling in exponent form, past the same bound",
			body: metricTenantUsedBytes + `{tenant_id="team-a"} 1.8014398509481984e+16` + "\n",
			says: "is not a count",
		},
		{
			name: "a flag that is neither 0 nor 1",
			body: metricTenantOverQuota + `{tenant_id="team-a"} 2` + "\n",
			says: "is not 0 or 1",
		},
		{
			name: "a per-tenant series with no tenant_id label",
			body: metricTenantUsedBytes + " 4096\n",
			says: "carries no usable tenant_id label",
		},
		{
			name: "a per-tenant series whose tenant_id is empty",
			body: metricTenantUsedBytes + `{tenant_id=""} 4096` + "\n",
			says: "carries no usable tenant_id label",
		},
		{
			name: "a line that is a name and no value",
			body: metricTenantUsedBytes + `{tenant_id="team-a"}` + "\n",
			says: "is not a sample line",
		},
		{
			// An UNCLOSED QUOTE, so there is no separating space outside the label block and the line
			// has no sample shape at all. It is refused for that rather than for the missing brace:
			// once a value can legitimately contain a space, "the text before the first space" stops
			// being a name-and-labels, and this line stops having one to complain about.
			name: "a label block that was cut open",
			body: metricTenantUsedBytes + `{tenant_id="team-a 4096` + "\n",
			says: "is not a sample line",
		},
		{
			// The brace complaint still has its own case: quotes balanced, separating space present,
			// closing brace absent.
			name: "a label block missing only its closing brace",
			body: metricTenantUsedBytes + `{tenant_id="team-a" 4096` + "\n",
			says: "has no closing brace",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeTenantQuotaMetrics([]byte(c.body))

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrMalformedBody)
			assert.Contains(t, err.Error(), c.says)
		})
	}

	t.Run("a truncated document", func(t *testing.T) {
		_, err := DecodeTenantQuotaMetrics(fixture(t, "tenant-metrics-truncated.txt"))

		require.Error(t, err,
			"the series before the cut parsed; reporting them would publish a partial scrape")
		assert.ErrorIs(t, err, ErrMalformedBody)
	})
}

// TestDecodeTenantQuotaMetrics_ReadsWhatAValidDocumentSpells covers the shapes a legitimate
// exposition uses and that this parser must not refuse.
func TestDecodeTenantQuotaMetrics_ReadsWhatAValidDocumentSpells(t *testing.T) {
	t.Run("an optional timestamp is ignored, not concatenated into the value", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics([]byte(
			metricTenantRequestedBytes + `{tenant_id="team-a"} 1073741824 1720000000000` + "\n"))
		require.NoError(t, err)

		sample, ok := got.Tenant("team-a")
		require.True(t, ok)
		require.NotNil(t, sample.RequestedBytes)
		assert.Equal(t, int64(1073741824), *sample.RequestedBytes)
	})

	t.Run("scientific notation is a value", func(t *testing.T) {
		// A Prometheus exposition writes gauges through %g, so a figure at cache scale arrives this
		// way — 500Gi is 5.36870912e+11 — and it is a whole number however it is spelled.
		got, err := DecodeTenantQuotaMetrics([]byte(
			metricTenantRequestedBytesSum + " 5.36870912e+11\n"))
		require.NoError(t, err)

		assert.Equal(t, ptr.To[int64](536870912000), got.RequestedBytesSum)
	})

	t.Run("a family this reader does not use is skipped, not refused", func(t *testing.T) {
		// The master serves the master-wide families and the per-tenant ones in ONE document, and a
		// later build adds families of its own. Neither is this reader's business.
		got, err := DecodeTenantQuotaMetrics(fixture(t, "metrics-full.txt"))

		require.NoError(t, err)
		assert.Empty(t, got.Tenants, "that document carries no tenant series at all")
		assert.Nil(t, got.AllocatableCapacityBytes)
	})

	t.Run("an empty document is not a malformed one", func(t *testing.T) {
		got, err := DecodeTenantQuotaMetrics(nil)

		require.NoError(t, err)
		assert.Empty(t, got.Tenants)
	})
}

// TestTenantQuotaMetricsClient_ReadsTheMetricsRoute pins that the tenant surface is taken off the
// same exposition the capacity read uses, and in one request: a pool with many Bindings scrapes once
// and every Binding takes its own tenant out of the result.
func TestTenantQuotaMetricsClient_ReadsTheMetricsRoute(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		_, _ = w.Write(fixture(t, "tenant-metrics.txt"))
	}))
	defer srv.Close()

	got, err := newTestAdminClient(srv).TenantQuotaMetrics(context.Background())

	require.NoError(t, err)
	assert.Equal(t, []string{"/metrics"}, asked)
	assert.Len(t, got.Tenants, 2)
}

// TestSampleTenantID_ACommaInsideAnExternalTenantName covers an id this operator did not write.
//
// A managed reuse domain is a DNS-1123 label and can hold no comma. The master's ledger carries
// entries created elsewhere, and one of those splitting a label block is what would fail the scrape
// for every Binding on the master rather than for that entry alone.
func TestSampleTenantID_ACommaInsideAnExternalTenantName(t *testing.T) {
	testCases := []struct {
		name     string
		labels   string
		expected string
	}{
		{
			name:     "an ordinary id",
			labels:   `tenant_id="team-a-chat"`,
			expected: "team-a-chat",
		},
		{
			name:     "a comma inside the value",
			labels:   `tenant_id="foo,bar"`,
			expected: "foo,bar",
		},
		{
			name:     "a comma inside the value, with a sibling label after it",
			labels:   `tenant_id="foo,bar",role="leader"`,
			expected: "foo,bar",
		},
		{
			name:     "a sibling label before it",
			labels:   `role="leader",tenant_id="foo,bar"`,
			expected: "foo,bar",
		},
		{
			name:     "an escaped quote next to a comma",
			labels:   `tenant_id="foo\",bar"`,
			expected: `foo",bar`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sampleTenantID("mooncake_tenant_quota_requested_bytes", tc.labels)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestSplitSample_ASpaceInsideAnExternalTenantName is the comma defect one stage earlier.
//
// splitLabels made a comma inside a quoted id survive; the line splitter still cut at the first
// space, so an id holding one was sliced before the label block was ever reached — and one malformed
// line fails the whole scrape for every Binding on that master.
func TestSplitSample_ASpaceInsideAnExternalTenantName(t *testing.T) {
	testCases := []struct {
		name     string
		line     string
		expected string
	}{
		{
			name:     "no labels",
			line:     "mooncake_tenant_quota_allocatable_capacity_bytes 4096",
			expected: "",
		},
		{
			name:     "an ordinary id",
			line:     `mooncake_tenant_quota_used_bytes{tenant_id="team-a"} 4096`,
			expected: "team-a",
		},
		{
			name:     "a space inside the value",
			line:     `mooncake_tenant_quota_used_bytes{tenant_id="foo bar"} 4096`,
			expected: "foo bar",
		},
		{
			name:     "a space and a comma inside the value",
			line:     `mooncake_tenant_quota_used_bytes{tenant_id="foo, bar"} 4096`,
			expected: "foo, bar",
		},
		{
			name:     "a trailing timestamp after the value",
			line:     `mooncake_tenant_quota_used_bytes{tenant_id="foo bar"} 4096 1700000000000`,
			expected: "foo bar",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			name, labels, value, err := splitSample(tc.line)
			require.NoError(t, err)
			assert.Equal(t, "4096", value, "the value is the field after the labels, not the timestamp")
			if tc.expected == "" {
				assert.Empty(t, labels)
				return
			}
			got, err := sampleTenantID(name, labels)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, got)
		})
	}
}
