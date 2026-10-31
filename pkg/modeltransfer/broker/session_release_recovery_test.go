package broker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionReleaseRecoveryPreservesExactCharge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failWrite bool
		want      int32
	}{
		{"released", false, 0},
		{"generation write refused", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, st, digest := newBroker(t, defaultLimits())
			var generation, secret string
			admitServe(t, b, digest, &generation, &secret)
			owner := testOwner("tenant-a")
			session, err := b.AdmitSession(t.Context(), owner, "tenant-a", generation)
			require.Nil(t, err)
			recordPath := filepath.Join(b.root, recordsDir, generation+".json")
			if tc.failWrite {
				require.NoError(t, os.Rename(recordPath, recordPath+".saved"))
				require.NoError(t, os.Mkdir(recordPath, 0o700))
			}
			releaseErr := b.ReleaseSession(t.Context(), owner, session.ID)
			if tc.failWrite {
				require.NotNil(t, releaseErr)
				require.NoError(t, os.Remove(recordPath))
				require.NoError(t, os.Rename(recordPath+".saved", recordPath))
			} else {
				require.Nil(t, releaseErr)
			}
			assert.Equal(t, tc.want, b.records[generation].ActiveSessions)
			require.NoError(t, b.Close())
			reopened, openErr := Open(b.root, testScope, st, b.limits, func() time.Time { return testNow })
			require.NoError(t, openErr)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			assert.Equal(t, tc.want, reopened.records[generation].ActiveSessions)
			assert.Zero(t, reopened.records[generation].UnknownSessions)
		})
	}
}
