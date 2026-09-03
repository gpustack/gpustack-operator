//go:build !linux

package preflight

import (
	"strings"
	"testing"
)

// TestPreflightNetworkCarriesTheEnumerationFailure exercises the real entry point.
//
// It is tagged to the platforms where the inventory pass has no implementation, and that is what
// makes it a test rather than a seam invented to be asserted on: the stub there fails
// unconditionally, so the one composition this function performs — hand the pass's result AND its
// error to the report — is observable without substituting anything.
//
// What it guards is not cosmetic. A dropped error yields an empty interface list, which the report
// then describes as a node with no RDMA hardware — claiming an answer about the hardware when the
// truth is that nothing could be looked at.
func TestPreflightNetworkCarriesTheEnumerationFailure(t *testing.T) {
	report := PreflightNetwork()

	if len(report.Checks) != 0 {
		t.Fatalf("checks = %+v on a platform with no inventory pass", report.Checks)
	}
	if !strings.Contains(report.Note, "could not be enumerated") {
		t.Errorf("note = %q, want the enumeration failure rather than a claim about the hardware",
			report.Note)
	}
	if strings.Contains(report.Note, "has an RDMA device bound") {
		t.Errorf("note = %q reports a node with no RDMA hardware, but nothing was read", report.Note)
	}
}
