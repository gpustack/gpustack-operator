package mooncake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// adminPathTenantQuotas is the tenant ledger's one route. Every operation is that path with a
// different method: listing and querying are GET, a create-or-update is PUT, a removal is DELETE.
//
// It is served on the SAME port as /health, /metrics and /get_segments_detail, which is why these
// methods hang off the client already reading those rather than off one of their own: one connection
// carries one set of rules — one body limit, one excerpt, one redirect policy — and the address being
// dialed comes from a CR a user wrote.
const adminPathTenantQuotas = "/api/v1/tenant_quotas"

// The ledger's distinguishable refusals. They are sentinels for the same reason the read surface's
// are: the caller BRANCHES on them — one keys a Condition an operator must act on, one holds a
// finalizer, and the rest are retried or reported as bugs here.
//
// The master answers a numeric code alongside an HTTP status. The status is what the mapping below
// is keyed on, because it is the part HTTP itself guarantees is there — with one exception, 409,
// which carries THREE unrelated codes and is therefore split further by what the body names.
var (
	// ErrInvalidQuota is the master refusing the quota figure in a PUT body. Measured, it answers
	// -600 / 400 "Tenant quota must be positive" for zero and "Invalid JSON body: Failed to parse
	// number" for a negative one.
	//
	// Reaching it means a BUG or a race here, not a user's input: every rejection path is
	// pre-validated at admission, so a quota the master refuses is one this operator should never
	// have rendered. The master's own words travel in the message so that is diagnosable.
	ErrInvalidQuota = errors.New("kvcache: the master refused the requested quota")

	// ErrInvalidTenantID is the master refusing the tenant_id in the query — measured, -600 / 400
	// "Missing or invalid tenant_id" for both an empty one and one starting with an underscore.
	//
	// It is separate from ErrInvalidQuota although the master answers both with one code, because
	// the two say different things about where the bug is: one is the domain name a webhook admitted,
	// the other is the ceiling it validated.
	ErrInvalidTenantID = errors.New("kvcache: the master refused the tenant id")

	// ErrMultiTenancyDisabled is -1011 / 409 UNAVAILABLE_IN_CURRENT_MODE: the ledger does not exist,
	// because the master was not started with multi-tenancy on.
	//
	// It is the precondition the whole quota feature rests on, so a caller keys a Condition off it
	// rather than retrying: nothing this operator writes will converge until the backend is changed.
	ErrMultiTenancyDisabled = errors.New("kvcache: the master is not running with multi-tenancy")

	// ErrTenantNotEmpty is -1702 / 409 TENANT_NOT_EMPTY: a DELETE of a tenant that still owns
	// objects.
	//
	// It has its own sentinel because it lands on the path that RELEASES a finalizer. Read as
	// anything else, a Binding whose domain still holds cache would be reported as a master with
	// multi-tenancy off — pointing an operator at the backend spec while the actual answer is that
	// the domain has to drain first.
	ErrTenantNotEmpty = errors.New("kvcache: the tenant still owns objects")

	// ErrUnavailableInCurrentStatus is -1010 / 409 UNAVAILABLE_IN_CURRENT_STATUS: the master is
	// refusing because of the state it is in — a standby, or a leader mid-transition.
	//
	// Unlike the other two 409s it is TRANSIENT, which is the whole reason it may not be collapsed
	// into either: a caller retries this one and writes a Condition for the others.
	ErrUnavailableInCurrentStatus = errors.New("kvcache: the master cannot serve this in its current status")

	// ErrQuotaPolicyNotWritable is -1503 / 500 PERSISTENT_FAIL: the master could not write the change
	// to its policy source, and so did not apply it either.
	//
	// NOTHING was accepted. Both admin-API write paths persist to the connector BEFORE applying, so a
	// refused write leaves the ledger exactly as it was — measured on a real master, where the tenant
	// of a failed PUT is absent from the listing that follows. Memory and disk do not disagree, which
	// is worth stating because the opposite reading suggests the quota is in force and merely
	// undurable, and would send an operator to restart something instead of to fix the mount.
	//
	// It has its own sentinel because it is the one failure that is neither the master's fault nor
	// the request's: the connector's file is not writable, so the first quota write fails at the
	// filesystem, where nobody is looking. Reported as an ordinary write failure it reads as a quota
	// that will simply converge later, and it never will.
	ErrQuotaPolicyNotWritable = errors.New("kvcache: the master cannot persist to its quota policy source")
)

// The three refusals that share HTTP 409, by the code the master writes into the body.
//
// Every refusal is `{"success": false, "error_code": …, "error_message": …}`, and the CODE is what
// is read: it is a value of the artifact's public ErrorCode enum, while the message is that enum's
// spelling only because these handlers pass no message of their own. A handler that later passed
// one would leave a message-matching reader unable to tell a tenant that has to drain from a master
// with the ledger switched off — and would do it without a symptom.
//
// Read from the artifact's source at v0.3.12.post1 — types.h for the values, master_admin_service.cpp
// for ErrorCodeToHttpStatus, which maps all three onto one status.
const (
	masterCodeUnavailableInCurrentStatus = -1010
	masterCodeUnavailableInCurrentMode   = -1011
	masterCodeTenantNotEmpty             = -1702

	// masterCodePersistentFail is the master failing to WRITE the policy it just accepted. It maps
	// to 500 only because ErrorCodeToHttpStatus has no case for it, so the status says "the master
	// broke" while the body says which way — which is why it is read from the code.
	masterCodePersistentFail = -1503

	// masterCodeObjectNotFound is the ONE code on the ledger routes that means "no such tenant".
	// ErrorCodeToHttpStatus maps four codes onto 404 — JOB_NOT_FOUND, SEGMENT_NOT_FOUND,
	// OBJECT_NOT_FOUND and TENANT_NOT_REGISTERED — and everything in front of the master answers
	// the same status for a route it does not serve. The status alone therefore cannot say which
	// happened, which is why absence is decided by the code and not by the 404.
	masterCodeObjectNotFound = -704
)

// TenantQuota is one entry of the master's tenant ledger, as the admin API returns it.
//
// The field set is exactly what the measured build (0.3.12.post1) answers.
//
// v0.3.13 answers a DIFFERENT set — it dropped used_bytes, reserved_bytes and both counts in favor
// of one charged_bytes, and added admission_closed. Nothing here is widened for it, because nothing
// here reads the fields it changed: the ledger converges on RequestedBytes and HasExplicitPolicy,
// which both builds send under the same names. The four that go missing decode as zero and are
// never published from this type — the per-tenant METRICS are what status is built from, and that
// decoder reads both generations. Widening this one too would add fields with no reader.
type TenantQuota struct {
	// TenantID is the entry's identity: the reuse domain a Binding declared.
	TenantID string

	// RequestedBytes is what was asked for, and EffectiveBytes what the master granted. The second
	// is LOWER whenever the sum of every tenant's request exceeds allocatable capacity, and it is
	// ZERO for every tenant of a master with no mounted segment.
	RequestedBytes int64
	EffectiveBytes int64

	// UsedBytes is committed consumption and ReservedBytes is in-flight PutStart reservations. They
	// are carried separately because status reports only the first: folding the second in would make
	// a burst of concurrent writes read as consumption that never happened.
	UsedBytes     int64
	ReservedBytes int64

	// CommittedCount and MetadataObjectCount are the entry's object counts.
	CommittedCount      int64
	MetadataObjectCount int64

	// OverQuota is the master's own verdict that this tenant HOLDS more than its effective quota,
	// which a tenant reaches by having its quota recut under it rather than by writing past it — the
	// master refuses a charge that would overshoot instead of recording one.
	OverQuota bool

	// HasExplicitPolicy distinguishes a tenant the ledger carries a policy for from one it is
	// serving under a default. A Binding with no quotaCeiling produces the second, and the two must
	// not look alike: one is a ceiling this operator wrote, the other is the absence of one.
	HasExplicitPolicy bool
}

// The ledger's responses are ENVELOPED: every 200 is `{"success": …, "data": …}`, with data an
// object for a query, an upsert and a delete, and an array for the listing. Read from the artifact's
// source at v0.3.12.post1 — HttpTenantQuotaResponse, HttpTenantQuotaListResponse and
// HttpTenantQuotaDeleteResponse in master_admin_service.cpp, which every handler there writes.
//
// The snapshot ALONE — no envelope — is what the capability experiment's notes show, because the
// payload pasted there is the inner data object rather than the endpoint's answer. There is no flat
// path in the artifact, so a bare snapshot is refused here: accepting both shapes would leave the
// next reader of those notes unable to find out which one the master sends.
//
// data is a POINTER on both. The delete response declares it std::optional, so an entry that was
// already gone answers 200 with data absent or null — which must not read as an entry, and on the
// listing an absent data must not read as an empty ledger.
type tenantQuotaEnvelope struct {
	Data *tenantQuotaBody `json:"data"`
}

type tenantQuotaListEnvelope struct {
	Data *[]tenantQuotaBody `json:"data"`
}

// tenantQuotaBody mirrors one snapshot. It is separate from TenantQuota so the pointer that
// distinguishes "absent" from "empty" stays at the boundary instead of spreading inward.
//
// The snake_case tags are the ARTIFACT's spelling, not a style choice here. Its handlers serialize
// through YLT_REFL, which uses the C++ member names verbatim with no case conversion, so the wire
// keys are whatever master_admin_service.cpp declared. A tag off by one character decodes to a zero
// value in silence — there is no unknown-field error to catch it — which is why this type exists at
// the boundary and nothing above it ever sees these names.
type tenantQuotaBody struct {
	// A POINTER, for the reason leaderHealthBody.ServiceReady is one: this field is the entry's
	// IDENTITY, and a body that is valid JSON and not a snapshot — `{}`, or another service
	// answering on a mistyped address — would otherwise decode into an entry named "" whose every
	// figure reads zero. Published, that is a tenant with no quota and no usage, which is a state
	// the master can genuinely report.
	TenantID            *string `json:"tenant_id"`
	RequestedQuotaBytes int64   `json:"requested_quota_bytes"`
	EffectiveQuotaBytes int64   `json:"effective_quota_bytes"`
	UsedBytes           int64   `json:"used_bytes"`
	ReservedBytes       int64   `json:"reserved_bytes"`
	CommittedCount      int64   `json:"committed_count"`
	MetadataObjectCount int64   `json:"metadata_object_count"`
	OverQuota           bool    `json:"over_quota"`
	HasExplicitPolicy   bool    `json:"has_explicit_policy"`
}

func (b tenantQuotaBody) toTenantQuota() TenantQuota {
	return TenantQuota{
		TenantID:            *b.TenantID,
		RequestedBytes:      b.RequestedQuotaBytes,
		EffectiveBytes:      b.EffectiveQuotaBytes,
		UsedBytes:           b.UsedBytes,
		ReservedBytes:       b.ReservedBytes,
		CommittedCount:      b.CommittedCount,
		MetadataObjectCount: b.MetadataObjectCount,
		OverQuota:           b.OverQuota,
		HasExplicitPolicy:   b.HasExplicitPolicy,
	}
}

// DecodeTenantQuota reads the enveloped answer carrying one ledger entry.
//
// A body with no data, and a snapshot with no tenant_id, are both refused rather than read as an
// empty or unnamed entry: see the two types' own comments for what either would otherwise publish.
func DecodeTenantQuota(body []byte) (TenantQuota, error) {
	var wire tenantQuotaEnvelope
	if err := json.Unmarshal(body, &wire); err != nil {
		return TenantQuota{}, fmt.Errorf("%w: %s: %w", ErrMalformedBody, adminPathTenantQuotas, err)
	}
	if wire.Data == nil {
		return TenantQuota{}, fmt.Errorf("%w: %s: no data in the body",
			ErrMalformedBody, adminPathTenantQuotas)
	}
	if wire.Data.TenantID == nil {
		return TenantQuota{}, fmt.Errorf("%w: %s: no tenant_id",
			ErrMalformedBody, adminPathTenantQuotas)
	}
	return wire.Data.toTenantQuota(), nil
}

// DecodeTenantQuotaList reads the whole ledger.
//
// An empty ledger is a legitimate value — a multi-tenant master nobody has written a policy to yet —
// and is returned as an empty slice. A body that is not a list of entries is an error rather than an
// empty ledger, because the two lead the caller in opposite directions: the first says every desired
// entry is missing and must be written, the second says this address is not answering the ledger.
func DecodeTenantQuotaList(body []byte) ([]TenantQuota, error) {
	var wire tenantQuotaListEnvelope
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrMalformedBody, adminPathTenantQuotas, err)
	}
	// Absent or null data is not an empty ledger. A JSON null decodes into a nil WITHOUT an error,
	// so this pointer is the only thing separating "the master sent no list" from "the master sent
	// an empty list", and only the second is a ledger this operator may converge against.
	if wire.Data == nil {
		return nil, fmt.Errorf("%w: %s: no data in the body",
			ErrMalformedBody, adminPathTenantQuotas)
	}

	entries := make([]TenantQuota, 0, len(*wire.Data))
	for _, entry := range *wire.Data {
		if entry.TenantID == nil {
			return nil, fmt.Errorf("%w: %s: an entry carries no tenant_id",
				ErrMalformedBody, adminPathTenantQuotas)
		}
		entries = append(entries, entry.toTenantQuota())
	}
	return entries, nil
}

// ListTenantQuotas reads every entry of the master's ledger.
//
// It is how convergence observes: the desired set is computed from the Bindings, and an entry the
// master holds that no Binding claims is what a DELETE is issued for. Listing once per pass rather
// than querying per tenant is what keeps that a single request whatever the Binding count.
func (c *AdminClient) ListTenantQuotas(ctx context.Context) ([]TenantQuota, error) {
	body, _, err := c.tenantQuotaDo(ctx, http.MethodGet, "", nil)
	if err != nil {
		return nil, err
	}
	return DecodeTenantQuotaList(body)
}

// GetTenantQuota reads one entry, and reports an unknown tenant as ABSENT: it returns a nil entry
// and a nil error.
//
// That is not leniency. A tenant the master does not know is the ordinary state between a Binding
// being admitted and the next pass writing its quota, so calling it a failure would make convergence
// report an error against itself on every create. It is the master's OBJECT_NOT_FOUND that says so
// and not the 404 carrying it — see tenantQuotaDo for what else answers that status.
func (c *AdminClient) GetTenantQuota(ctx context.Context, tenantID string) (*TenantQuota, error) {
	body, found, err := c.tenantQuotaRequest(ctx, http.MethodGet, tenantID, nil)
	if err != nil || !found {
		return nil, err
	}

	quota, err := DecodeTenantQuota(body)
	if err != nil {
		return nil, err
	}
	return &quota, nil
}

// PutTenantQuota creates or updates one entry, writing requestedBytes as that tenant's
// requested_quota_bytes.
//
// It is unconditional by design: deciding that an entry already carries this figure and needs no
// write belongs to the caller, which has the observed ledger in hand. A client that read before
// every write would double the request count of a pass that changes nothing.
//
// An OBJECT_NOT_FOUND is an ERROR here, and only here. tenantQuotaDo reports one as
// absent-not-failed because that is what it means to a GET (a tenant between admission and its first
// write) and to a DELETE (an entry already gone) — but this route is an upsert, so there is no tenant
// whose absence it could be describing. It says the request never reached a handler that writes: a
// wrong address, a proxy in front of the master, or a build without these routes. Reporting that as
// success would let the pass call the ledger converged and the pool Ready with the quota never
// written. A 404 that carries any other code is refused one level down, for every method.
func (c *AdminClient) PutTenantQuota(ctx context.Context, tenantID string, requestedBytes int64) error {
	payload, err := json.Marshal(map[string]int64{"requested_quota_bytes": requestedBytes})
	if err != nil {
		return fmt.Errorf("build quota body for %q: %w", tenantID, err)
	}

	_, found, err := c.tenantQuotaRequest(ctx, http.MethodPut, tenantID, payload)
	if err != nil {
		return err
	}
	if !found {
		// Deliberately none of the sentinels: it is neither of the two named preconditions, so the
		// reconciler reports it on the ledger axis as an unreachable ledger, which is what it is.
		return fmt.Errorf("PUT %s: the master answered 404, so the quota of %q was not written",
			adminPathTenantQuotas, tenantID)
	}
	return nil
}

// DeleteTenantQuota removes one entry, and an entry that is already gone is SUCCESS.
//
// The master answers OBJECT_NOT_FOUND for a tenant it does not know, and that is the state a delete
// is asking for. Reporting it as a failure would leave a pass that converged perfectly reporting an
// error, and would do so forever: the entry never comes back. A 404 that does NOT carry that code
// travels as an error instead, because this is the caller whose finalizer releases on success.
//
// The removed entry comes back in the answer's data, which is why nothing here decodes it: the body
// describes what was just deleted, and this operator has no reader for a snapshot of that. The
// artifact declares it optional for the same reason it can be absent.
//
// A tenant that still owns objects is refused with ErrTenantNotEmpty rather than deleted. That is
// the master's own precondition, and it is a caller's cue to wait rather than to retry harder.
func (c *AdminClient) DeleteTenantQuota(ctx context.Context, tenantID string) error {
	_, _, err := c.tenantQuotaRequest(ctx, http.MethodDelete, tenantID, nil)
	return err
}

// tenantQuotaRequest addresses one entry of the ledger.
//
// An empty tenantID is refused HERE rather than sent. On the write methods the master would answer
// 400 anyway, but on a GET the query would simply be absent and the master would answer the WHOLE
// LEDGER — so a caller asking for one tenant under an empty name would be handed a list to decode as
// an entry, and told the master's body was malformed.
func (c *AdminClient) tenantQuotaRequest(
	ctx context.Context, method, tenantID string, payload []byte,
) ([]byte, bool, error) {
	if tenantID == "" {
		return nil, false, fmt.Errorf("%w: %s: no tenant id was given",
			ErrInvalidTenantID, adminPathTenantQuotas)
	}
	return c.tenantQuotaDo(ctx, method, url.Values{"tenant_id": []string{tenantID}}.Encode(), payload)
}

// tenantQuotaDo performs one ledger request and turns its outcome into a distinguishable failure.
// The bool it returns is whether the tenant EXISTS: false with a nil error is the master's 404.
func (c *AdminClient) tenantQuotaDo(
	ctx context.Context, method, query string, payload []byte,
) ([]byte, bool, error) {
	target := fmt.Sprintf("http://%s%s", c.Address, adminPathTenantQuotas)
	if query != "" {
		target += "?" + query
	}

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, false, fmt.Errorf("build %s request for %s: %w", method, adminPathTenantQuotas, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A transport failure travels as itself, exactly as it does on the read routes: nothing
		// answered, which is neither a malformed body nor a refusal.
		return nil, false, fmt.Errorf("request %s %s: %w", method, adminPathTenantQuotas, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The same bound and the same one-byte overshoot as the read routes, because it is the same port
	// and the same reason: the address names whatever an administrator wrote into a CR.
	body, err := io.ReadAll(io.LimitReader(resp.Body, adminReadLimit+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s %s: %w", method, adminPathTenantQuotas, err)
	}
	if len(body) > adminReadLimit {
		return nil, false, fmt.Errorf("%w: %s: the response is larger than %d bytes",
			ErrMalformedBody, adminPathTenantQuotas, adminReadLimit)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return body, true, nil
	case http.StatusNotFound:
		// Absent, not failed — but ONLY when the master says so in the body. A 404 reaches here
		// from four of the artifact's own codes and from anything in front of it that does not
		// serve this route at all: a proxy, a wrong address, a build without the ledger routes.
		// Read as absence, that last group is the dangerous one, because every caller of this
		// function treats absence as a settled answer rather than as a question that failed —
		// GetTenantQuota reports the tenant unknown, and DeleteTenantQuota reports the entry
		// already gone, which lets a Binding's finalizer release over a ledger entry that is
		// still there.
		if !objectNotFound(body) {
			return nil, false, fmt.Errorf(
				"request %s %s: the master answered 404 without OBJECT_NOT_FOUND, so this is a "+
					"route that does not exist rather than a tenant that does not: %s",
				method, adminPathTenantQuotas, excerpt(body))
		}
		return nil, false, nil
	case http.StatusServiceUnavailable:
		// The ledger routes are gated on the service plane like /get_segments_detail is, so a
		// starting leader answers this. It is a phase, and the caller retries.
		//
		// It carries error_code -1011, the SAME code a 409 means multi-tenancy-is-off by, because
		// the master writes both through one helper. That is why the status is asked before the
		// code and not the other way round: a starting leader read by code alone would be reported
		// as a backend somebody has to go and reconfigure.
		return nil, false, fmt.Errorf("%w: %s: %s",
			ErrServicePlaneInactive, adminPathTenantQuotas, excerpt(body))
	case http.StatusBadRequest:
		// -600, which the master answers for two unrelated inputs under ONE code: at v0.3.12.post1
		// both a refused quota and a refused tenant_id come back as ErrorCode::INVALID_PARAMS —
		// ParseAdminTenantId returns it for an empty id and for an invalid one, and the zero-quota
		// branch of HandleUpsertTenantQuota returns it too. There is no code-level discriminator to
		// switch to, so the message is not a shortcut here, it is the only signal: measured, every
		// tenant-id refusal names the field and no quota refusal does.
		refusal := ErrInvalidQuota
		if strings.Contains(strings.ToLower(string(body)), "tenant_id") {
			refusal = ErrInvalidTenantID
		}
		return nil, false, fmt.Errorf("%w: %s %s: %s",
			refusal, method, adminPathTenantQuotas, excerpt(body))
	case http.StatusConflict:
		// THREE unrelated codes share this status — -1011, -1010 and -1702 — and one of them lands
		// on the delete path a finalizer waits on, so the status alone may not decide.
		return nil, false, conflictError(method, body)
	case http.StatusInternalServerError:
		// ErrorCodeToHttpStatus maps everything it has no case for onto this status, so the status
		// alone means only "the master broke". One of those codes is the mount fault this operator
		// has to report differently, and the body is what separates it from the rest.
		if persistentFail(body) {
			return nil, false, fmt.Errorf("%w: %s %s: %s",
				ErrQuotaPolicyNotWritable, method, adminPathTenantQuotas, excerpt(body))
		}
		return nil, false, fmt.Errorf("request %s %s: unexpected status %d: %s",
			method, adminPathTenantQuotas, resp.StatusCode, excerpt(body))
	default:
		return nil, false, fmt.Errorf("request %s %s: unexpected status %d: %s",
			method, adminPathTenantQuotas, resp.StatusCode, excerpt(body))
	}
}

// objectNotFound reports whether a 404 is the master's own "no such tenant".
//
// A body that does not parse fails the test on purpose: the master envelopes every refusal, so an
// answer without one did not come from a ledger handler. Absence is the one 404 this client acts
// on, and acting on it wrongly is silent — hence the burden of proof sits on the body.
func objectNotFound(body []byte) bool {
	var wire struct {
		ErrorCode int32 `json:"error_code"`
	}
	return json.Unmarshal(body, &wire) == nil && wire.ErrorCode == masterCodeObjectNotFound
}

// persistentFail reports whether a 500 is the master failing to write what it accepted.
func persistentFail(body []byte) bool {
	var wire struct {
		ErrorCode int32 `json:"error_code"`
	}
	return json.Unmarshal(body, &wire) == nil && wire.ErrorCode == masterCodePersistentFail
}

// conflictError names which of the three refusals a 409 is, by the error_code in its body.
//
// A body that does not parse, or that carries a code none of the three names, is deliberately NONE
// of the sentinels: guessing would put a Condition on an object for a reason the master never gave.
// The master's own words still travel in the message, which is what leaves it diagnosable.
func conflictError(method string, body []byte) error {
	var wire struct {
		ErrorCode int32 `json:"error_code"`
	}
	if err := json.Unmarshal(body, &wire); err == nil {
		var refusal error
		switch wire.ErrorCode {
		case masterCodeUnavailableInCurrentMode:
			refusal = ErrMultiTenancyDisabled
		case masterCodeTenantNotEmpty:
			refusal = ErrTenantNotEmpty
		case masterCodeUnavailableInCurrentStatus:
			refusal = ErrUnavailableInCurrentStatus
		}
		if refusal != nil {
			return fmt.Errorf("%w: %s %s: %s", refusal, method, adminPathTenantQuotas, excerpt(body))
		}
	}

	return fmt.Errorf("request %s %s: unexpected conflict: %s",
		method, adminPathTenantQuotas, excerpt(body))
}
