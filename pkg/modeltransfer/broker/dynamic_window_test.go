package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func TestComputeDynamicSourceBuffer(t *testing.T) {
	const (
		gib int64 = 1 << 30
		mib int64 = 1 << 20
	)
	cases := []struct {
		name           string
		availRAM       int64
		maxMemoryBytes int64
		minMemoryBytes int64
		wantWindow     int64
		wantAdmitted   bool
	}{
		// Plenty of headroom keeps the configured ceiling.
		{"headroom keeps the ceiling", 20 * gib, 8 * gib, gib, 8 * gib, true},
		// Exactly the quarter headroom is not "more than" it, so the
		// shrink engages; one step down recovers the headroom.
		{"exact quarter headroom shrinks", 10 * gib, 8 * gib, gib, 6 * gib, true},
		// Loaded nodes shrink by a quarter per step until plain fit.
		{"loaded node shrinks to plain fit", 5 * gib, 8 * gib, gib, 4608 * mib, true},
		{"heavily loaded node keeps shrinking", 1200 * mib, 8 * gib, gib, 1092 * mib, true},
		// The floor caps the shrink: above the floor but under every
		// shrink step, the floor is served.
		{"the floor caps the shrink", 1030 * mib, 8 * gib, gib, gib, true},
		// A ceiling under the floor resolves up to the floor first.
		{"a small ceiling resolves up to the floor", 4 * gib, 512 * mib, 512 * mib, gib, true},
		// A floor under 1 GiB resolves up to 1 GiB first.
		{"a small floor resolves up to 1 GiB", 2 * gib, 8 * gib, gib / 1024, 1944 * mib, true},
		// RAM at or under the floor backs no RDMA window at all.
		{"RAM at the floor is refused", gib, 8 * gib, gib, 0, false},
		{"RAM under the floor is refused", 900 * mib, 8 * gib, gib, 0, false},
		{"no RAM is refused", 0, 8 * gib, gib, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			window, ok := ComputeDynamicSourceBuffer(tc.availRAM, tc.maxMemoryBytes, tc.minMemoryBytes)
			assert.Equal(t, tc.wantAdmitted, ok)
			assert.Equal(t, tc.wantWindow, window)
		})
	}
}

// TestComputeDynamicSourceBufferEnormousCeiling pins the overflow guard: a
// ceiling past a quarter of the address space never breaks the arithmetic,
// and the answer stays inside the available RAM.
func TestComputeDynamicSourceBufferEnormousCeiling(t *testing.T) {
	const gib int64 = 1 << 30
	window, ok := ComputeDynamicSourceBuffer(3*gib, int64(1)<<62, gib)
	require.True(t, ok)
	assert.Greater(t, window, gib)
	assert.LessOrEqual(t, window, 3*gib)
}

// TestNodeSessionLimit pins the node-level session cap: it counts every
// generation's live charge, admits nothing past it, moves online in either
// direction, and the sessions already admitted drain naturally.
func TestNodeSessionLimit(t *testing.T) {
	b, _, digest := newBroker(t, defaultLimits())
	assert.Zero(t, b.LimitsSnapshot().MaxSessions,
		"a broker opened without a cap caps nothing")

	generation, secret := "", ""
	admitServe(t, b, digest, &generation, &secret)

	b.SetMaxSessions(1)
	owner := testOwner("tenant-a")
	first, perr := b.AdmitSessionContext(context.Background(), owner, "tenant-a", generation)
	require.Nil(t, perr)

	_, perr = b.AdmitSessionContext(context.Background(), testOwner("tenant-b"), "tenant-a", generation)
	require.NotNil(t, perr, "the node cap bounds the second admission")
	assert.Equal(t, protocol.KindCapacityExhausted, perr.Kind)
	assert.Contains(t, perr.Error(), "the node session limit is reached")

	// A release frees the node charge; a raise admits again; the sessions
	// already admitted under the old cap stay observable.
	require.Nil(t, b.ReleaseSession(owner, first.ID))
	_, perr = b.AdmitSessionContext(context.Background(), owner, "tenant-a", generation)
	require.Nil(t, perr)
	b.SetMaxSessions(2)
	_, perr = b.AdmitSessionContext(context.Background(), testOwner("tenant-b"), "tenant-a", generation)
	require.Nil(t, perr)
	_, perr = b.AdmitSessionContext(context.Background(), testOwner("tenant-c"), "tenant-a", generation)
	require.NotNil(t, perr, "the raised cap bounds the next admission again")
	assert.Equal(t, protocol.KindCapacityExhausted, perr.Kind)

	b.SetMaxSessions(0)
	_, perr = b.AdmitSessionContext(context.Background(), testOwner("tenant-d"), "tenant-a", generation)
	require.Nil(t, perr, "a zero cap disables the node limit")
}

// TestSourceWindowBytes pins the session answer's window: it is the dynamic
// buffer over the node's available RAM, absent when the RAM cannot be read
// or cannot back the floor.
func TestSourceWindowBytes(t *testing.T) {
	limits := defaultLimits()
	limits.PhysicalBytes = 8 << 30
	limits.MinMemoryBytes = 1 << 30
	b, _, _ := newBroker(t, limits)

	const gib int64 = 1 << 30
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	write := func(content string) {
		require.NoError(t, os.WriteFile(meminfo, []byte(content), 0o644))
	}
	write("MemTotal:       32768 kB\nMemAvailable:   20971520 kB\n")
	b.SetMemInfoPath(meminfo)

	window, ok := b.SourceWindowBytes()
	require.True(t, ok)
	assert.Equal(t, 8*gib, window, "plenty of headroom keeps the configured ceiling")

	write("MemAvailable:   9437184 kB\n")
	window, ok = b.SourceWindowBytes()
	require.True(t, ok)
	assert.Equal(t, 6*gib, window, "9 GiB of RAM clears plain fit one shrink down")

	write("MemAvailable:    524288 kB\n")
	_, ok = b.SourceWindowBytes()
	assert.False(t, ok, "512 MiB cannot back the 1 GiB floor")

	write("garbage\n")
	_, ok = b.SourceWindowBytes()
	assert.False(t, ok, "unreadable memory figures answer no window")
}

// TestSessionRetryAfterAndSourceWindowOverHTTP pins what a full node serves
// an engine over the wire: a conflict that names its wait and records no
// admission, and — once the cap moves — an open answer carrying the window
// the node can back right now.
func TestSessionRetryAfterAndSourceWindowOverHTTP(t *testing.T) {
	b, generation, owner, _ := gateFixture(t)
	digest := b.records[generation].Digest
	handler := NewSessionHandler(b, gateAuthorizer{digest: digest})

	meminfo := filepath.Join(t.TempDir(), "meminfo")
	require.NoError(t, os.WriteFile(meminfo, []byte("MemAvailable:   20971520 kB\n"), 0o644))
	b.SetMemInfoPath(meminfo)

	// One admitted session fills the node's cap of one: the next open is a
	// capacity refusal that names its wait.
	b.SetMaxSessions(1)
	_, perr := b.AdmitSessionContext(context.Background(), owner, "tenant", generation)
	require.Nil(t, perr)

	body := gateSessionRequest(t, digest, strings.Repeat("d", 32), "weight")
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	assert.Equal(t, "5", recorder.Header().Get("Retry-After"))
	var refusal map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &refusal))
	assert.Equal(t, "not-created", refusal["admission"])
	assert.Equal(t, string(protocol.KindCapacityExhausted), refusal["kind"])
	assert.Equal(t, float64(sessionRetryAfterSeconds), refusal["retryAfter"])

	// A different request identity under a raised cap admits, and the
	// answer carries the node's window for the client's min negotiation.
	b.SetMaxSessions(2)
	fresh := gateSessionRequest(t, digest, strings.Repeat("e", 32), "weight")
	code, answer, raw := gateSessionPost(t, handler, fresh)
	require.Equal(t, http.StatusOK, code, raw)
	assert.InEpsilon(t, float64(1<<30), answer["sourceWindowBytes"], 0,
		"the open answer carries the dynamic window over a 20 GiB available RAM")
}
