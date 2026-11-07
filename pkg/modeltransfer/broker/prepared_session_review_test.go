package broker

import (
	"context"
	"testing"
)

func TestReviewRetryRetiresUnexposedPreparedSession(t *testing.T) {
	f := newPagedSourceFixture(t)
	f.b.mu.Lock()
	session := f.b.sessions[f.pageRequest.SessionID]
	session.Phase = "prepared"
	if err := f.b.persistSessionsLocked(); err != nil {
		f.b.mu.Unlock()
		t.Fatal(err)
	}
	f.b.mu.Unlock()
	if err := f.b.RetrySessionTransactions(context.Background()); err != nil {
		t.Fatalf("retry cannot retire an unexposed prepared session: %v", err)
	}
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	if got := f.b.sessions[f.pageRequest.SessionID].Phase; got != "released" {
		t.Fatalf("prepared session remains charged: %s", got)
	}
}
