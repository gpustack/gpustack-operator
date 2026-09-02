package mooncake

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The per-tenant series this operator reads. Each carries exactly one label, tenant_id, and the
// tenant IS the reuse domain a Binding declared — so a Binding reads the series bearing its own
// domain name and nothing is ever summed.
//
// TWO generations are read, because the image is whatever a backend's spec names and both are in
// use. 0.3.12.post1 emits eight of these; 0.3.13 removed four and added one. A build emits one
// generation or the other, never both, so every figure stays a pointer and the absent generation is
// simply absent — reading both costs two cases and buys an occupancy figure that would otherwise go
// silently missing on the newer build.
const (
	metricTenantRequestedBytes = "mooncake_tenant_quota_requested_bytes"
	metricTenantEffectiveBytes = "mooncake_tenant_quota_effective_bytes"
	metricTenantOverQuota      = "mooncake_tenant_quota_over_quota"
	metricTenantExplicitPolicy = "mooncake_tenant_quota_explicit_policy"

	// 0.3.12.post1 only. It splits occupancy in two and counts objects.
	metricTenantUsedBytes           = "mooncake_tenant_quota_used_bytes"
	metricTenantReservedBytes       = "mooncake_tenant_quota_reserved_bytes"
	metricTenantCommittedCount      = "mooncake_tenant_quota_committed_count"
	metricTenantMetadataObjectCount = "mooncake_tenant_quota_metadata_object_count"

	// 0.3.13 only. One occupancy figure charged at PutStart, so it already contains what
	// metricTenantReservedBytes used to carry separately, plus a refusal flag that is not over-quota.
	metricTenantChargedBytes    = "mooncake_tenant_quota_charged_bytes"
	metricTenantAdmissionClosed = "mooncake_tenant_quota_admission_closed"
)

// The global gauges, which carry no label at all. They describe the MASTER, not any one tenant, and
// the first of them is the one that explains a whole pool full of zeros: a master with no mounted
// segment has nothing to allocate, so every tenant's effective quota is zero and no write can
// succeed while every object still looks correctly configured.
const (
	metricTenantAllocatableCapacityBytes = "mooncake_tenant_quota_allocatable_capacity_bytes"
	metricTenantRequestedBytesSum        = "mooncake_tenant_quota_requested_bytes_sum"
	metricTenantEffectiveBytesSum        = "mooncake_tenant_quota_effective_bytes_sum"
)

// tenantLabel is the only label the per-tenant families carry.
const tenantLabel = "tenant_id"

// TenantSample is one tenant's figures as one scrape reported them.
//
// Every figure is a POINTER, and the distinction is the whole reason the type exists: the master
// serializes only label combinations it has OBSERVED, so a series missing from a document may be one
// that has never fired rather than one reading zero. Published as zero, "this tenant has written
// nothing yet" and "this tenant's usage was not reported" become the same status — and a fabricated
// zero on a warm cache is worse than no number.
type TenantSample struct {
	// RequestedBytes is what was asked for and EffectiveBytes what the master granted. The second is
	// proportionally lower whenever every tenant's request together exceeds allocatable capacity.
	RequestedBytes *int64
	EffectiveBytes *int64

	// UsedBytes is committed consumption; ReservedBytes is in-flight PutStart reservations. Status
	// reports the first ALONE — folding the second in would make a burst of concurrent writes read
	// as consumption that never happened. 0.3.12.post1 only; see Occupancy.
	UsedBytes     *int64
	ReservedBytes *int64

	// CommittedCount and MetadataObjectCount are this tenant's object counts. 0.3.12.post1 only —
	// 0.3.13 stopped exporting them and nothing replaced them.
	CommittedCount      *int64
	MetadataObjectCount *int64

	// ChargedBytes is 0.3.13's single occupancy figure, replacing the pair above. It is charged at
	// PutStart, so an in-flight reservation is already inside it and there is nothing to subtract.
	ChargedBytes *int64

	// AdmissionClosed is 0.3.13's flag for the master refusing this tenant's new writes, and it is
	// NOT a synonym for OverQuota: the master also closes admission for a tenant whose accounting it
	// has lost confidence in, which no larger quota reopens.
	AdmissionClosed *bool

	// OverQuota is the master's verdict that this tenant HOLDS more than its effective quota, which
	// it reaches only by the quota being recut under it: charged bytes never overshoot the grant,
	// because the master refuses the charge rather than letting the total pass. A tenant writing past
	// its quota therefore leaves this at false, and AdmissionClosed above is not its substitute
	// either — a write past the quota is neither over-quota nor closed admission, it is the master
	// evicting that tenant's own objects and admitting the write.
	OverQuota *bool

	// ExplicitPolicy separates a tenant the ledger holds a written quota for from one running under
	// the master's default. A Binding with no ceiling produces the second, and it is a state to
	// report rather than one to hide.
	ExplicitPolicy *bool
}

// Occupancy is how many bytes this tenant holds, and whether that figure includes writes that have
// not landed yet.
//
// The two generations disagree about what is measurable, and this is the one place that disagreement
// is resolved so no caller has to know which build it is talking to. 0.3.12.post1 reports committed
// bytes apart from reservations, so the first return value excludes them and inflight is false.
// 0.3.13 reports one figure charged from PutStart, so reservations are inside it and inflight is
// true — a caller publishing the number has to say so, because a burst of concurrent writes raises
// it before any of them commit.
//
// The older pair is preferred when both are somehow present: it is the more precise answer, and
// nothing observed so far emits both.
func (s TenantSample) Occupancy() (bytes *int64, inflight bool) {
	if s.UsedBytes != nil {
		return s.UsedBytes, false
	}
	return s.ChargedBytes, s.ChargedBytes != nil
}

// TenantQuotaMetrics is one scrape of the tenant surface: every tenant the document mentioned, plus
// the master's three global gauges.
//
// It is ONE document for the whole pool. A pass reads it once and every Binding takes its own
// tenant's series out of it, which is what keeps the request count independent of the Binding count.
type TenantQuotaMetrics struct {
	// Tenants is keyed by tenant_id. A tenant the document did not mention is NOT a key here, and
	// that absence is the report: see Tenant.
	Tenants map[string]TenantSample

	// AllocatableCapacityBytes is what the master has to divide between its tenants. Zero is the
	// startup-ordering trap F10 exists for — no member has mounted, so every effective quota is zero.
	AllocatableCapacityBytes *int64

	// RequestedBytesSum and EffectiveBytesSum are the master's own totals across every tenant it
	// knows, including tenants belonging to another pool on the same master. Nothing per-Binding is
	// derived from them; they are read so an operator can see that the second is below the first,
	// which is what a pool under pressure looks like.
	RequestedBytesSum *int64
	EffectiveBytesSum *int64
}

// Tenant returns one tenant's figures and whether the document carried that tenant at all.
//
// The bool is the point: a caller must be able to tell "reported, and every figure is zero" from
// "not reported", because the first is a quiet tenant and the second is a tenant the master has
// never heard of — which is what a Binding whose quota has not been written yet looks like.
func (m TenantQuotaMetrics) Tenant(tenantID string) (TenantSample, bool) {
	sample, ok := m.Tenants[tenantID]
	return sample, ok
}

// TenantQuotaMetrics reads /metrics and keeps the tenant surface out of it.
//
// It is the same document AdminClient.Capacity reads, and deliberately a second decode of it rather
// than a second endpoint: the master serves the master-wide families and the per-tenant ones in one
// exposition, and each caller reads the half it has a use for.
func (c *AdminClient) TenantQuotaMetrics(ctx context.Context) (TenantQuotaMetrics, error) {
	body, err := c.get(ctx, adminPathMetrics)
	if err != nil {
		return TenantQuotaMetrics{}, err
	}
	return DecodeTenantQuotaMetrics(body)
}

// DecodeTenantQuotaMetrics reads the per-tenant series of both generations and the three global
// gauges out of one
// Prometheus exposition.
//
// It parses the text itself for the same reason DecodeLeaderCapacity does: the upstream parser
// requires a PROCESS-WIDE metric-name validation mode to be set before first use, and this process
// already hosts the operator's own registry.
//
// A line that is not a well-formed sample is an ERROR, not a line to skip — which is stricter than
// the capacity decoder and is the difference a truncated response makes here. This parser is
// line-oriented, so a body cut mid-stream yields every series before the cut; skipping the partial
// last line would publish that prefix as a complete scrape, and a tenant whose series happened to
// sit after the cut would be reported absent rather than unread.
func DecodeTenantQuotaMetrics(body []byte) (TenantQuotaMetrics, error) {
	metrics := TenantQuotaMetrics{Tenants: map[string]TenantSample{}}

	globals := map[string]**int64{
		metricTenantAllocatableCapacityBytes: &metrics.AllocatableCapacityBytes,
		metricTenantRequestedBytesSum:        &metrics.RequestedBytesSum,
		metricTenantEffectiveBytesSum:        &metrics.EffectiveBytesSum,
	}

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, labels, value, err := splitSample(line)
		if err != nil {
			return TenantQuotaMetrics{}, err
		}

		if target, ok := globals[name]; ok {
			// A global gauge carries no label. One that arrives labeled is a different series that
			// happens to share the name, and reading it as the global figure would report one
			// tenant's total as the master's.
			if labels != "" {
				continue
			}
			parsed, err := parseSampleCount(name, value)
			if err != nil {
				return TenantQuotaMetrics{}, err
			}
			*target = &parsed
			continue
		}

		if !isTenantSeries(name) {
			continue
		}

		tenantID, err := sampleTenantID(name, labels)
		if err != nil {
			return TenantQuotaMetrics{}, err
		}

		sample := metrics.Tenants[tenantID]
		if err := assignTenantSample(&sample, name, value); err != nil {
			return TenantQuotaMetrics{}, err
		}
		metrics.Tenants[tenantID] = sample
	}

	return metrics, nil
}

func isTenantSeries(name string) bool {
	switch name {
	case metricTenantRequestedBytes, metricTenantEffectiveBytes, metricTenantUsedBytes,
		metricTenantReservedBytes, metricTenantCommittedCount, metricTenantMetadataObjectCount,
		metricTenantChargedBytes, metricTenantAdmissionClosed,
		metricTenantOverQuota, metricTenantExplicitPolicy:
		return true
	default:
		return false
	}
}

// assignTenantSample writes one series' value onto the tenant it belongs to.
func assignTenantSample(sample *TenantSample, name, value string) error {
	switch name {
	case metricTenantOverQuota, metricTenantExplicitPolicy, metricTenantAdmissionClosed:
		flag, err := parseSampleFlag(name, value)
		if err != nil {
			return err
		}
		switch name {
		case metricTenantOverQuota:
			sample.OverQuota = &flag
		case metricTenantExplicitPolicy:
			sample.ExplicitPolicy = &flag
		case metricTenantAdmissionClosed:
			sample.AdmissionClosed = &flag
		}
		return nil
	}

	count, err := parseSampleCount(name, value)
	if err != nil {
		return err
	}
	switch name {
	case metricTenantRequestedBytes:
		sample.RequestedBytes = &count
	case metricTenantEffectiveBytes:
		sample.EffectiveBytes = &count
	case metricTenantUsedBytes:
		sample.UsedBytes = &count
	case metricTenantReservedBytes:
		sample.ReservedBytes = &count
	case metricTenantCommittedCount:
		sample.CommittedCount = &count
	case metricTenantMetadataObjectCount:
		sample.MetadataObjectCount = &count
	case metricTenantChargedBytes:
		sample.ChargedBytes = &count
	}
	return nil
}

// splitSample takes one exposition line apart into its name, its label block and its value.
//
// A sample is `name[{labels}] value [timestamp]`, and the timestamp is OPTIONAL — so the value is
// the first field after the name rather than everything after it. A line that does not have that
// shape is refused: see the decoder's own comment for why a truncated document must not read as a
// short one.
func splitSample(line string) (name, labels, value string, err error) {
	// Cut at the separating space, which is the one OUTSIDE the label block. A tenant id this
	// operator wrote holds no space, but the ledger carries ids created elsewhere, and cutting at the
	// first space would slice `{tenant_id="foo bar"}` in half — a head with no closing brace, refused,
	// and with it the whole scrape and every managed Binding's figures. Same defect as the comma in
	// splitLabels, one stage earlier.
	head, tail, ok := cutOutsideLabels(line)
	if !ok {
		return "", "", "", fmt.Errorf("%w: %s: %q is not a sample line",
			ErrMalformedBody, adminPathMetrics, quoteExternal(line))
	}

	value = strings.TrimSpace(tail)
	if i := strings.IndexAny(value, " \t"); i >= 0 {
		value = value[:i]
	}

	name = head
	if open := strings.IndexByte(head, '{'); open >= 0 {
		if !strings.HasSuffix(head, "}") {
			return "", "", "", fmt.Errorf("%w: %s: %q has no closing brace",
				ErrMalformedBody, adminPathMetrics, quoteExternal(head))
		}
		name, labels = head[:open], head[open+1:len(head)-1]
	}
	return name, labels, value, nil
}

// cutOutsideLabels cuts a sample line at the first space that is not inside its label block, so a
// label VALUE holding a space does not split the line where it is not divided.
func cutOutsideLabels(line string) (head, tail string, found bool) {
	var quoted, escape bool
	for i := 0; i < len(line); i++ {
		switch {
		case escape:
			escape = false
		case line[i] == '\\':
			escape = true
		case line[i] == '"':
			quoted = !quoted
		case (line[i] == ' ' || line[i] == '\t') && !quoted:
			return line[:i], line[i+1:], true
		}
	}
	return line, "", false
}

// splitLabels splits a label block on the commas that SEPARATE labels, leaving the ones inside a
// quoted value alone.
//
// A plain Split on "," is wrong here for a reason this package already documents elsewhere: the
// master's ledger carries entries nobody in this cluster created, and their ids are not DNS-1123
// labels. One comma in one external tenant's name would cut `tenant_id="foo,bar"` into two fragments,
// neither of which parses — and because a series without a usable id is REFUSED rather than skipped,
// that one name would fail the whole scrape and take the figures of every managed Binding on this
// master with it.
func splitLabels(labels string) []string {
	var (
		out            []string
		start          int
		quoted, escape bool
	)
	for i := 0; i < len(labels); i++ {
		switch {
		case escape:
			escape = false
		case labels[i] == '\\':
			escape = true
		case labels[i] == '"':
			quoted = !quoted
		case labels[i] == ',' && !quoted:
			out = append(out, labels[start:i])
			start = i + 1
		}
	}
	return append(out, labels[start:])
}

// sampleTenantID reads the tenant_id label off a per-tenant series.
//
// A per-tenant family that arrives without that label is refused rather than dropped: the label is
// the only thing saying whose figure this is, so a sample without it is either not this master's
// series or a document that was mangled, and both mean the scrape cannot be trusted to say which
// tenant is absent.
func sampleTenantID(name, labels string) (string, error) {
	for _, label := range splitLabels(labels) {
		key, quoted, ok := strings.Cut(strings.TrimSpace(label), "=")
		if !ok || strings.TrimSpace(key) != tenantLabel {
			continue
		}
		quoted = strings.TrimSpace(quoted)
		if len(quoted) < 2 || !strings.HasPrefix(quoted, `"`) || !strings.HasSuffix(quoted, `"`) {
			break
		}
		// The master escapes a backslash, a double quote and a newline on its way out, so they are
		// unescaped on the way in. A tenant id this operator wrote can hold none of them — it is a
		// DNS-1123 label — but the master's ledger also carries entries nobody here created.
		unquoted := strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\n`, "\n").
			Replace(quoted[1 : len(quoted)-1])
		if unquoted == "" {
			break
		}
		return unquoted, nil
	}

	return "", fmt.Errorf("%w: %s: %s carries no usable %s label",
		ErrMalformedBody, adminPathMetrics, name, tenantLabel)
}

// parseSampleCount reads one sample as a non-negative whole number.
//
// It refuses everything an unchecked int64 conversion would turn into a fabricated figure. ParseFloat
// accepts NaN and both infinities, and converting a float64 that does not fit an int64 is UNDEFINED
// in Go — measured, the same sample gives the maximum int64 on arm64 and the minimum on amd64, both
// of which this project ships.
//
// Two paths, two bounds. A plain integer is read exactly and may be anything an int64 holds; a
// float-spelled one is capped at 2^53, above which float64 cannot carry an integer at all. Which
// path a sample takes is decided by its spelling, not by its size.
func parseSampleCount(name, value string) (int64, error) {
	// The exact spelling first, because these samples are byte counts and float64 stops being able
	// to hold one at 2^53: 9007199254740993 comes back from ParseFloat as ...992, which is whole,
	// in range, and wrong — it passes every guard below and gets published as a figure the master
	// never reported. ParseInt takes a plain integer verbatim, and the float path stays for the
	// exposition format's other spellings of the same value (1.5e+09, 12.0).
	if exact, err := strconv.ParseInt(value, 10, 64); err == nil {
		if exact < 0 {
			return 0, fmt.Errorf("%w: %s: %s carries %q, which is not a count",
				ErrMalformedBody, adminPathMetrics, name, quoteExternal(value))
		}
		return exact, nil
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		// Only the REASON of strconv's error: its own Error() quotes the whole input back, which
		// would restore unbounded the very string quoted above with a bound.
		return 0, fmt.Errorf("%w: %s: %s carries %q: %w",
			ErrMalformedBody, adminPathMetrics, name, quoteExternal(value), errors.Unwrap(err))
	}
	// 2^53 here, not 2^63, and the two paths bound differently on purpose. A float-spelled sample
	// only reaches this line because it is NOT a plain integer, so nothing above 2^53 can be trusted:
	// `9007199254740993.0` parses to ...992, which is whole, non-negative and inside an int64, and
	// would be published as a figure the master never reported. The exact path above has no such
	// bound because ParseInt reads the whole int64 range verbatim — the reason the fast path exists.
	// Refusing an unrepresentable float spelling is the safe half of the trade: a master that means a
	// count this large can always write it as digits.
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) ||
		parsed < 0 || parsed >= float64(uint64(1)<<53) ||
		parsed != math.Trunc(parsed) {
		return 0, fmt.Errorf("%w: %s: %s carries %q, which is not a count",
			ErrMalformedBody, adminPathMetrics, name, quoteExternal(value))
	}
	return int64(parsed), nil
}

// parseSampleFlag reads one sample as the boolean a gauge of 0 or 1 stands for. Anything else is
// refused rather than read as truthy: a flag that is neither says the document is not what this
// reader thinks it is, and over_quota decides whether a namespace is told its domain holds more than
// it is now granted.
func parseSampleFlag(name, value string) (bool, error) {
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%w: %s: %s carries %q, which is not 0 or 1",
			ErrMalformedBody, adminPathMetrics, name, quoteExternal(value))
	}
}
