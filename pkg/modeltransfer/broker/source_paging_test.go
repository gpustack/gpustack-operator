package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func pagingManifest(t *testing.T, size int64) (protocol.Manifest, string, string) {
	t.Helper()
	manifest := protocol.Manifest{ContractVersion: protocol.ContractVersion, ArtifactUID: "artifact", SourceGeneration: 1}
	for _, name := range []string{"a", "b", "c"} {
		path := name + ".safetensors"
		manifest.Files = append(manifest.Files, protocol.FileDescriptor{Path: path, Size: size})
		manifest.Tensors = append(manifest.Tensors, protocol.TensorDescriptor{
			Name: name, File: path, Dtype: "U8", Shape: []int64{size}, DataEnd: size,
		})
	}
	raw, err := json.Marshal(manifest)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return manifest, string(raw), hex.EncodeToString(sum[:])
}

func TestSourceResidentBudget(t *testing.T) {
	manifest, _, _ := pagingManifest(t, 2<<30)
	for _, tc := range []struct {
		name                          string
		grant, node, configured, want int64
		missing                       bool
	}{
		{name: "full", grant: 8 << 30, node: 8 << 30, want: 6 << 30},
		{name: "grant clamped", grant: reserveBytes(4 << 30), node: 8 << 30, want: 4 << 30},
		{name: "node clamped", grant: 8 << 30, node: reserveBytes(4 << 30), want: 4 << 30},
		{name: "configured", grant: 8 << 30, node: 8 << 30, configured: 2 << 30, want: 2 << 30},
		{name: "shard exceeds budget", grant: 2 << 30, node: 8 << 30},
		{name: "below floor", grant: 1 << 30, node: 8 << 30},
		{name: "missing manifest", grant: 4 << 30, node: 8 << 30, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Broker{limits: Limits{PhysicalBytes: tc.node}, residentBudget: tc.configured}
			req := FillRequest{TotalBytes: 6 << 30, Grant: Grant{Bytes: tc.grant}}
			input := &manifest
			if tc.missing {
				input = nil
			}
			got, err := b.sourceResidentBytes(req, input, "nixl")
			if tc.want == 0 {
				require.NotNil(t, err)
				return
			}
			require.Nil(t, err)
			require.Equal(t, tc.want, got)
			require.LessOrEqual(t, reserveBytes(got), tc.grant)
			require.LessOrEqual(t, reserveBytes(got), tc.node)
		})
	}
}

func TestHixlAdmissionChargesAlignedBacking(t *testing.T) {
	for _, tc := range []struct {
		name             string
		size, allocation int64
	}{
		{name: "single byte", size: 1, allocation: 4 << 20},
		{name: "aligned region", size: 2 << 20, allocation: 4 << 20},
		{name: "unaligned region", size: 2<<20 + 1, allocation: 6 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest, _, _ := pagingManifest(t, tc.size)
			required := tc.allocation * 3
			b := &Broker{limits: Limits{PhysicalBytes: reserveBytes(required)}}
			req := FillRequest{TotalBytes: tc.size * 3, Grant: Grant{Bytes: reserveBytes(required)}}
			got, err := b.sourceResidentBytes(req, &manifest, "hixl")
			require.Nil(t, err)
			require.Equal(t, required, got)
			req.Grant.Bytes--
			_, err = b.sourceResidentBytes(req, &manifest, "hixl")
			require.NotNil(t, err)
		})
	}
}

func TestPagedIntentReservationAndResidentReports(t *testing.T) {
	for _, backend := range []string{"nixl", "hixl"} {
		t.Run(backend, func(t *testing.T) {
			limits := defaultLimits()
			limits.PhysicalBytes = 8 << 30
			b, _, digest := newBroker(t, limits)
			_, raw, sum := pagingManifest(t, 2<<30)
			intent := Intent{
				Key: "paged", Namespace: "tenant", Digest: digest, TotalBytes: 6 << 30,
				Grant: grant("tenant", reserveBytes(4<<30)), WorkerID: "source",
				NodeName: "node", IntentUID: "prefetch", IntentGeneration: 1,
				BindingUID: "binding-uid-1", ReservationID: "paging-reservation",
				Registrations: 3, ArtifactUID: "artifact", SourceGeneration: 1,
				ManifestJSON: raw, ManifestSHA256: sum, Backend: backend,
			}
			assignment, perr := b.AdmitIntentFill(t.Context(), intent)
			require.Nil(t, perr)
			require.Equal(t, int64(4<<30), assignment.ResidentBudgetBytes)
			rec := b.records[assignment.Generation]
			require.Equal(t, int64(6<<30), rec.TotalBytes)
			require.Equal(t, reserveBytes(4<<30), rec.ReservedBytes)
			authority := b.reservationAuthorities["binding-uid-1"]
			cm, err := authority.maps.Get(context.Background(), AuthorityName("binding-uid-1"), getOneOptions)
			require.NoError(t, err)
			doc, err := authority.decode(cm)
			require.NoError(t, err)
			require.Len(t, doc.Entries, 1)
			require.Equal(t, rec.ReservedBytes, doc.Entries[0].Bytes)
			again, perr := b.AdmitIntentFill(t.Context(), intent)
			require.Nil(t, perr)
			require.Equal(t, assignment, again)

			instance := "worker-instance"
			lease, perr := b.RegisterWorker(rec.Generation, "source", assignment.WorkerSecret, instance)
			require.Nil(t, perr)
			require.Nil(t, b.NativeStart(rec.Generation, "source", lease.ID, assignment.WorkerSecret, instance))
			descriptor := RegistrationDescriptor{Name: "a.safetensors", Addr: 1, Length: 2 << 30, MemoryType: "HOST", Backend: backend, WorkerUID: "source"}
			require.Nil(t, b.FillVerified(t.Context(), rec.Generation, "source", lease.ID, assignment.WorkerSecret, 1, []RegistrationDescriptor{descriptor}, instance))
			require.Equal(t, sourceAllocationBytes(2<<30, backend), rec.ResidentBytes)
			require.Equal(t, int32(3), rec.Registrations)
			require.Equal(t, int32(3), recordAssignment(rec).Registrations)
			// Zero residency does not release the immutable reservation.
			_, perr = b.Heartbeat(rec.Generation, "source", lease.ID, assignment.WorkerSecret, Health{}, instance)
			require.Nil(t, perr)
			require.Zero(t, rec.ResidentBytes)
			require.Equal(t, reserveBytes(4<<30), rec.ReservedBytes)
			_, perr = b.Heartbeat(rec.Generation, "source", lease.ID, assignment.WorkerSecret, Health{ResidentBytes: 5 << 30}, instance)
			require.NotNil(t, perr)

			records, err := b.ledger.records()
			require.NoError(t, err)
			require.Len(t, records, 1)
			require.Equal(t, int64(4<<30), records[0].ResidentBudgetBytes)
			require.Equal(t, int64(6<<30), records[0].TotalBytes)
			require.Equal(t, int32(3), recordAssignment(records[0]).Registrations)
		})
	}
}

func TestPagedDescriptorsRemainManifestBound(t *testing.T) {
	_, raw, sum := pagingManifest(t, 2<<30)
	rec := &GenerationRecord{ManifestJSON: raw, ManifestSHA256: sum, ArtifactUID: "artifact", SourceGeneration: 1, Registrations: 3, Backend: "nixl", ResidentBudgetBytes: 4 << 30, TotalBytes: 6 << 30}
	first := RegistrationDescriptor{Name: "a.safetensors", Addr: 1, Length: 2 << 30, MemoryType: "HOST", Backend: "nixl", WorkerUID: "source"}
	second := first
	second.Name = "b.safetensors"
	third := first
	third.Name = "c.safetensors"
	foreign := first
	foreign.Name = "missing.safetensors"
	partial := first
	partial.Length = 1
	for _, tc := range []struct {
		name        string
		descriptors []RegistrationDescriptor
		valid       bool
	}{
		{"one resident", []RegistrationDescriptor{first}, true},
		{"two resident", []RegistrationDescriptor{first, second}, true},
		{"over budget", []RegistrationDescriptor{first, second, third}, false},
		{"duplicate", []RegistrationDescriptor{first, first}, false},
		{"foreign", []RegistrationDescriptor{foreign}, false},
		{"partial", []RegistrationDescriptor{partial}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &Broker{}
			err := b.validateDescriptorPlan(context.Background(), descriptorPlanOf(rec), "source", tc.descriptors)
			require.Equal(t, tc.valid, err == nil)
		})
	}
}

func TestSourceResidentEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
		valid bool
	}{
		{"", 0, true},
		{"0", 0, true},
		{"2147483648", 2 << 30, true},
		{"-1", 0, false},
		{"typo", 0, false},
		{"1048576", 1 << 30, true},
		{"1073741825", 1 << 30, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(sourceResidentBudgetEnv, tc.value)
			t.Setenv("GPUSTACK_MODEL_TRANSFER_HIXL_RESIDENT_BUDGET_BYTES", "0")
			got, hixl, err := sourceResidentLimits()
			require.Equal(t, tc.valid, err == nil)
			if tc.valid {
				require.Equal(t, tc.want, got)
				require.Equal(t, int64(16<<30), hixl)
			}
		})
	}
}
