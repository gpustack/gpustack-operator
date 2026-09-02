package mooncake

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTenantQuotaDecode runs against the body recorded from the measured build. The fixture is the
// only record of what that build returns, which is why nothing here is built from a literal.
func TestTenantQuotaDecode(t *testing.T) {
	got, err := DecodeTenantQuota(fixture(t, "tenant-quota.json"))
	require.NoError(t, err)

	assert.Equal(t, TenantQuota{
		TenantID:            "team-a",
		RequestedBytes:      1073741824,
		EffectiveBytes:      0,
		UsedBytes:           0,
		ReservedBytes:       0,
		CommittedCount:      0,
		MetadataObjectCount: 0,
		OverQuota:           false,
		HasExplicitPolicy:   true,
	}, got)
}

// TestTenantQuotaDecode_UnmeasuredFieldsAreNeitherRequiredNorInvented covers both halves of the
// upstream/measured disagreement in one place.
//
// Upstream documentation lists charged_bytes and admission_closed; the measured build returns
// neither. Requiring them would refuse the body the build actually sends, and declaring them would
// publish a zero nothing observed — so the decoder accepts the measured field set and ignores the
// two wherever a later build does send them.
func TestTenantQuotaDecode_UnmeasuredFieldsAreNeitherRequiredNorInvented(t *testing.T) {
	measured, err := DecodeTenantQuota(fixture(t, "tenant-quota.json"))
	require.NoError(t, err, "a body without charged_bytes or admission_closed is the measured one")

	withExtras, err := DecodeTenantQuota([]byte(
		`{"success":true,"data":{` +
			`"tenant_id":"team-a","requested_quota_bytes":1073741824,"effective_quota_bytes":0,` +
			`"used_bytes":0,"reserved_bytes":0,"committed_count":0,"metadata_object_count":0,` +
			`"over_quota":false,"has_explicit_policy":true,` +
			`"charged_bytes":4096,"admission_closed":true}}`))
	require.NoError(t, err, "a build that does send them is not an error either")

	assert.Equal(t, measured, withExtras,
		"neither field reaches this type, so neither can be published as an observation")
}

// TestTenantQuotaDecode_NotThisDocumentIsMalformed guards the boundary the pointers exist for. Every
// figure defaults to zero, and a tenant whose quota and usage are zero is a state the master
// genuinely reports — so a body that is valid JSON and not an enveloped ledger entry must not
// become one.
func TestTenantQuotaDecode_NotThisDocumentIsMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
		says string
	}{
		{name: "an empty object", body: `{}`, says: "no data"},
		{name: "a delete answer, which carries no entry", body: `{"success":true,"data":null}`, says: "no data"},
		{
			name: "an envelope whose snapshot has no identity",
			body: `{"success":true,"data":{"requested_quota_bytes":1073741824}}`,
			says: "no tenant_id",
		},
		{
			// The shape the spec's own Notes show. The artifact has no flat path at
			// v0.3.12.post1 — every handler writes an envelope — so what was recorded there is the
			// inner data object, pasted. Accepting it would make this decoder agree with a document
			// no master sends, and the next reader of those notes would have no way to find that out.
			name: "a bare snapshot, with no envelope around it",
			body: `{"tenant_id":"team-a","requested_quota_bytes":1073741824,"effective_quota_bytes":0,` +
				`"used_bytes":0,"reserved_bytes":0,"committed_count":0,"metadata_object_count":0,` +
				`"over_quota":false,"has_explicit_policy":true}`,
			says: "no data",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeTenantQuota([]byte(c.body))

			require.Error(t, err)
			assert.ErrorIs(t, err, ErrMalformedBody)
			assert.Contains(t, err.Error(), c.says)
		})
	}
}

func TestTenantQuotaDecodeList(t *testing.T) {
	t.Run("every entry of the ledger is read", func(t *testing.T) {
		got, err := DecodeTenantQuotaList(fixture(t, "tenant-quotas.json"))
		require.NoError(t, err)

		require.Len(t, got, 2)
		assert.Equal(t, "team-a", got[0].TenantID)
		assert.Equal(t, int64(1073741824), got[0].RequestedBytes)
		assert.Equal(t, "team-b", got[1].TenantID)
		assert.Equal(t, int64(4294967296), got[1].RequestedBytes)
	})

	t.Run("an empty ledger is a value and not a failure", func(t *testing.T) {
		got, err := DecodeTenantQuotaList([]byte(`{"success":true,"data":[]}`))

		require.NoError(t, err, "a multi-tenant master nobody has written a policy to yet")
		assert.Empty(t, got)
	})

	t.Run("a body that is not a ledger is not an empty ledger", func(t *testing.T) {
		// The two lead in opposite directions: an empty ledger says every desired entry has to be
		// written, a failed read says this address is not answering the ledger at all. The bare
		// array is here for the same reason the bare snapshot is above — no handler writes one.
		for _, body := range []string{`{}`, `{"success":true,"data":null}`, `null`, `[]`} {
			_, err := DecodeTenantQuotaList([]byte(body))
			require.Error(t, err, "body %s", body)
			assert.ErrorIs(t, err, ErrMalformedBody)
		}
	})
}

// TestTenantQuotaClient_RefusalsAreDistinguishable is the point of the sentinels: the reconciler
// branches on them, and two of the three arrive under ONE master error code, so this asserts that no
// two of them look the same to a caller.
func TestTenantQuotaClient_RefusalsAreDistinguishable(t *testing.T) {
	// Every sentinel the ledger can raise. Each case asserts its own AND every other, because the
	// failure this guards against is two of them arriving alike — and three of them share a status.
	sentinels := []error{
		ErrInvalidQuota, ErrInvalidTenantID,
		ErrMultiTenancyDisabled, ErrTenantNotEmpty, ErrUnavailableInCurrentStatus,
		ErrQuotaPolicyNotWritable,
	}

	// The write this operator issues. A refusal is asserted through the operation that actually
	// provokes it: TENANT_NOT_EMPTY is answered on DELETE and nowhere else, and that is the path a
	// finalizer waits on.
	put := func(c *AdminClient) error {
		return c.PutTenantQuota(context.Background(), "team-a", 1073741824)
	}
	del := func(c *AdminClient) error {
		return c.DeleteTenantQuota(context.Background(), "team-a")
	}

	cases := []struct {
		name    string
		status  int
		fixture string
		op      func(*AdminClient) error
		want    error
		says    string
	}{
		{
			name:    "a quota the master calls not positive",
			status:  http.StatusBadRequest,
			fixture: "tenant-quota-rejected-quota.json",
			op:      put,
			want:    ErrInvalidQuota,
			says:    "Tenant quota must be positive",
		},
		{
			name:    "a quota the master cannot parse as a number",
			status:  http.StatusBadRequest,
			fixture: "tenant-quota-rejected-quota-number.json",
			op:      put,
			want:    ErrInvalidQuota,
			says:    "Failed to parse number",
		},
		{
			name:    "a tenant id the master refuses",
			status:  http.StatusBadRequest,
			fixture: "tenant-quota-rejected-tenant-id.json",
			op:      put,
			want:    ErrInvalidTenantID,
			says:    "Missing or invalid tenant_id",
		},
		{
			name:    "the whole API answering that the mode is wrong",
			status:  http.StatusConflict,
			fixture: "tenant-quota-multi-tenancy-off.json",
			op:      put,
			want:    ErrMultiTenancyDisabled,
			says:    "UNAVAILABLE_IN_CURRENT_MODE",
		},
		{
			// The one that made the 409 worth splitting: it is answered on the delete a finalizer
			// waits on, so mapped to the multi-tenancy sentinel it would point an operator at the
			// backend spec while the real answer is that the domain has to drain first.
			name:    "a delete of a tenant that still owns objects",
			status:  http.StatusConflict,
			fixture: "tenant-quota-tenant-not-empty.json",
			op:      del,
			want:    ErrTenantNotEmpty,
			says:    "TENANT_NOT_EMPTY",
		},
		{
			name:    "a master refusing because of the state it is in",
			status:  http.StatusConflict,
			fixture: "tenant-quota-unavailable-in-current-status.json",
			op:      put,
			want:    ErrUnavailableInCurrentStatus,
			says:    "UNAVAILABLE_IN_CURRENT_STATUS",
		},
		{
			// The master ACCEPTED the change and could not write it down. It arrives under 500,
			// which ErrorCodeToHttpStatus also gives to everything it has no case for — so the code
			// in the body is what separates it, and the separation matters: read as an ordinary
			// write failure it looks like a quota that will converge on the next pass, and it never
			// will until somebody makes the connector's file writable.
			name:    "a policy source the master cannot rewrite",
			status:  http.StatusInternalServerError,
			fixture: "tenant-quota-persistent-fail.json",
			op:      put,
			want:    ErrQuotaPolicyNotWritable,
			says:    "PERSISTENT_FAIL",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write(fixture(t, c.fixture))
			}))
			defer srv.Close()

			err := c.op(newTestAdminClient(srv))

			require.Error(t, err)
			assert.ErrorIs(t, err, c.want)
			for _, other := range sentinels {
				if errors.Is(other, c.want) {
					continue
				}
				assert.NotErrorIs(t, err, other,
					"the reconciler branches on these; two must never arrive alike")
			}
			assert.Contains(t, err.Error(), c.says,
				"the master's own words travel, so a Condition says what the master said")
		})
	}

	// A 409 naming none of the three codes is none of the three refusals. Guessing would put a
	// Condition on an object for a reason the master never gave.
	t.Run("a conflict this reader does not recognise is its own outcome", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"success":false,"error_code":-9999,"error_message":"SOMETHING_ELSE"}`))
		}))
		defer srv.Close()

		err := newTestAdminClient(srv).PutTenantQuota(context.Background(), "team-a", 1073741824)

		require.Error(t, err)
		for _, sentinel := range sentinels {
			assert.NotErrorIs(t, err, sentinel)
		}
		assert.Contains(t, err.Error(), "SOMETHING_ELSE",
			"and the master's own words still travel, which is what makes it diagnosable")
	})
}

// TestTenantQuotaClient_ARefusalIsNamedByItsCodeNotItsMessage is why the reader asks for error_code
// rather than for the words next to it.
//
// Every ledger 409 today renders the enum's own spelling as its message, and that is a fact about
// those handlers rather than about the protocol: each of them passes no message, so the master fills
// one in. A handler that later passes its own — the shape below — would leave a message-matching
// reader unable to tell a tenant that has to drain from a master with the ledger switched off, and
// the finalizer waiting on the first would be told to go and reconfigure the second.
func TestTenantQuotaClient_ARefusalIsNamedByItsCodeNotItsMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(
			`{"success":false,"error_code":-1702,"error_message":"tenant team-a still owns 3 objects"}`))
	}))
	defer srv.Close()

	err := newTestAdminClient(srv).DeleteTenantQuota(context.Background(), "team-a")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTenantNotEmpty)
	assert.NotErrorIs(t, err, ErrMultiTenancyDisabled,
		"the code says which refusal this is; the message only says it in words")
}

// TestTenantQuotaClient_InvalidTenantIDOnEitherShape covers the second half of the case above: the
// master answers the same -600 / 400 for an empty tenant_id and for one starting with an underscore,
// and both are the tenant-id refusal rather than the quota one.
func TestTenantQuotaClient_InvalidTenantIDOnEitherShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(fixture(t, "tenant-quota-rejected-tenant-id.json"))
	}))
	defer srv.Close()

	_, err := newTestAdminClient(srv).GetTenantQuota(context.Background(), "_bad")

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTenantID)
	assert.NotErrorIs(t, err, ErrInvalidQuota)
}

// TestTenantQuotaClient_UnknownTenantIsAbsentNotAFailure is the 404 the spec models as absence.
//
// A tenant the master has never heard of is the ordinary state between a Binding being admitted and
// the next pass writing its quota, and it is also the state a delete is asking for — so reporting it
// as a failure would make convergence report an error against itself.
func TestTenantQuotaClient_UnknownTenantIsAbsentNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(fixture(t, "tenant-quota-unknown-tenant.json"))
	}))
	defer srv.Close()

	client := newTestAdminClient(srv)

	quota, err := client.GetTenantQuota(context.Background(), "team-a")
	require.NoError(t, err, "unknown is a state, not a fault")
	assert.Nil(t, quota, "and it is reported as no entry, which is not an entry reading zero")

	require.NoError(t, client.DeleteTenantQuota(context.Background(), "team-a"),
		"an entry that is already gone is what the delete was asking for")

	// The upsert is the one operation for which 404 cannot mean "absent": there is no tenant whose
	// absence it could describe, so it says the request reached nothing that writes. Reported as
	// success it would let a pass call the ledger converged with the quota never written.
	err = client.PutTenantQuota(context.Background(), "team-a", 1<<30)
	require.Error(t, err, "a 404 on an upsert is not an absent tenant, it is a write that did not land")
	assert.Contains(t, err.Error(), "was not written")
}

// TestTenantQuotaClient_A404WithoutTheCodeIsNotAnAbsentTenant is the other half of the test above:
// the status is the same, and only the body says whether anything answered the question.
//
// ErrorCodeToHttpStatus maps four of the artifact's codes onto 404, and everything in front of the
// master — a proxy, a wrong address, a build without the ledger routes — answers it too. Read as
// absence, the last group is the one that costs something: DeleteTenantQuota is what a Binding's
// finalizer waits on, so a 404 from a route that does not exist would release the finalizer over a
// ledger entry that is still there, and nothing would ever look again.
func TestTenantQuotaClient_A404WithoutTheCodeIsNotAnAbsentTenant(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "an empty body, which is what something in front of the master answers",
			body: "",
		},
		{
			name: "an error page, from a router that does not serve this path at all",
			body: "<html><head><title>404 Not Found</title></head><body>nginx</body></html>",
		},
		{
			name: "the master's own code for a tenant with no policy, which is a different state",
			body: `{"success":false,"error_code":-1701,"error_message":"TENANT_NOT_REGISTERED"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := newTestAdminClient(srv)

			_, err := client.GetTenantQuota(context.Background(), "team-a")
			require.Error(t, err,
				"a question nothing answered must not be reported as the answer 'no such tenant'")

			require.Error(t, client.DeleteTenantQuota(context.Background(), "team-a"),
				"and least of all here, where reporting success releases the Binding's finalizer")

			require.Error(t, client.PutTenantQuota(context.Background(), "team-a", 1<<30),
				"the upsert refuses every 404, and now names this one for what it is")
		})
	}
}

// TestTenantQuotaClient_DeleteAcceptsAnAnswerCarryingNoEntry covers the shape the artifact declares
// optional: a delete answers 200 with its data absent or null. Nothing here decodes that body — the
// snapshot describes what was just removed and has no reader — so the absence must not be mistaken
// for a body that failed to parse.
func TestTenantQuotaClient_DeleteAcceptsAnAnswerCarryingNoEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture(t, "tenant-quota-deleted-absent.json"))
	}))
	defer srv.Close()

	require.NoError(t, newTestAdminClient(srv).DeleteTenantQuota(context.Background(), "team-a"))
}

// TestTenantQuotaClient_MakesTheRequestEachOperationNames pins method, path, query and body for all
// four operations. A misdirected one would be answered with a body of the wrong shape and reported
// as malformed, which sends an operator looking at the master instead of at this code.
func TestTenantQuotaClient_MakesTheRequestEachOperationNames(t *testing.T) {
	type observed struct {
		method string
		path   string
		query  string
		body   string
	}
	var asked []observed

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = append(asked, observed{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.RawQuery,
			body:   string(body),
		})
		if r.Method == http.MethodGet && r.URL.RawQuery == "" {
			_, _ = w.Write(fixture(t, "tenant-quotas.json"))
			return
		}
		_, _ = w.Write(fixture(t, "tenant-quota.json"))
	}))
	defer srv.Close()

	client := newTestAdminClient(srv)
	ctx := context.Background()

	entries, err := client.ListTenantQuotas(ctx)
	require.NoError(t, err)
	assert.Len(t, entries, 2)

	quota, err := client.GetTenantQuota(ctx, "team-a")
	require.NoError(t, err)
	require.NotNil(t, quota)

	require.NoError(t, client.PutTenantQuota(ctx, "team-a", 1073741824))
	require.NoError(t, client.DeleteTenantQuota(ctx, "team-a"))

	assert.Equal(t, []observed{
		{method: http.MethodGet, path: "/api/v1/tenant_quotas"},
		{method: http.MethodGet, path: "/api/v1/tenant_quotas", query: "tenant_id=team-a"},
		{
			method: http.MethodPut,
			path:   "/api/v1/tenant_quotas",
			query:  "tenant_id=team-a",
			body:   `{"requested_quota_bytes":1073741824}`,
		},
		{method: http.MethodDelete, path: "/api/v1/tenant_quotas", query: "tenant_id=team-a"},
	}, asked)
}

// TestTenantQuotaClient_AnEmptyTenantIDNeverReachesTheMaster covers the one input this client
// refuses itself.
//
// On a GET an absent tenant_id is not an error at the master at all — it is the LIST request. A
// caller asking for one tenant under an empty name would be handed the whole ledger and told the
// master's body was malformed, which points at the master for a mistake made here.
func TestTenantQuotaClient_AnEmptyTenantIDNeverReachesTheMaster(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		_, _ = w.Write(fixture(t, "tenant-quotas.json"))
	}))
	defer srv.Close()

	client := newTestAdminClient(srv)
	ctx := context.Background()

	_, err := client.GetTenantQuota(ctx, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTenantID)

	assert.ErrorIs(t, client.PutTenantQuota(ctx, "", 1073741824), ErrInvalidTenantID)
	assert.ErrorIs(t, client.DeleteTenantQuota(ctx, ""), ErrInvalidTenantID)

	assert.Zero(t, asked, "none of the three was sent")
}

// TestTenantQuotaClient_BoundsWhatItReadsAndWhatItQuotes asserts the write paths obey the SAME two
// bounds the read paths do. They share a port and an address that came out of a CR, so a second set
// of rules for one connection is the thing this task exists to avoid.
func TestTenantQuotaClient_BoundsWhatItReadsAndWhatItQuotes(t *testing.T) {
	t.Run("a body over the cap is a failure, not a truncated success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("["))
			_, _ = w.Write([]byte(strings.Repeat(" ", adminReadLimit+1)))
		}))
		defer srv.Close()

		_, err := newTestAdminClient(srv).ListTenantQuotas(context.Background())

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMalformedBody)
		assert.Contains(t, err.Error(), "larger than")
	})

	t.Run("a failing response is quoted in an excerpt, not whole", func(t *testing.T) {
		// The error becomes a condition message, capped at 32 KiB by the schema. Quoting a body
		// whole would make every status write fail validation — so the reconciler could not report
		// the fault it was trying to report, for as long as the master kept answering that way.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"success":false,"error_code":-1011,"error_message":"`))
			_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
			_, _ = w.Write([]byte(`"}`))
		}))
		defer srv.Close()

		err := newTestAdminClient(srv).PutTenantQuota(context.Background(), "team-a", 1073741824)

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMultiTenancyDisabled)
		assert.Less(t, len(err.Error()), 2048,
			"the write paths truncate exactly as the read paths do")
		assert.Contains(t, err.Error(), "(truncated)",
			"and say they cut, so nobody reads the excerpt as the whole answer")
	})
}

// TestTenantQuotaClient_ADistinguishableFailureForEveryOtherOutcome covers the outcomes the ledger
// shares with the read routes: the routes are gated on the service plane, and an unexpected status
// is neither a refusal nor a phase.
func TestTenantQuotaClient_ADistinguishableFailureForEveryOtherOutcome(t *testing.T) {
	t.Run("503 is the service plane, not a refusal", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(fixture(t, "service-plane-inactive.txt"))
		}))
		defer srv.Close()

		_, err := newTestAdminClient(srv).ListTenantQuotas(context.Background())

		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServicePlaneInactive)
		assert.NotErrorIs(t, err, ErrMultiTenancyDisabled,
			"a master that is starting is not a master with multi-tenancy off")
	})

	t.Run("an unexpected status is its own outcome", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		err := newTestAdminClient(srv).DeleteTenantQuota(context.Background(), "team-a")

		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrInvalidQuota)
		assert.NotErrorIs(t, err, ErrInvalidTenantID)
		assert.NotErrorIs(t, err, ErrMultiTenancyDisabled)
		assert.Contains(t, err.Error(), "500")
	})
}
