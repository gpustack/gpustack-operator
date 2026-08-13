package exporter

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_Apply(t *testing.T) {
	testCases := []struct {
		name string

		period time.Duration

		wantErr bool
	}{
		{
			name:   "a configured period yields a poller",
			period: 15 * time.Second,
		},
		{
			// waitx polls on the period, so a zero one would spin instead of sampling.
			name:    "a zero period is refused",
			wantErr: true,
		},
		{
			name:    "a negative period is refused",
			period:  -time.Second,
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := (&Config{MonitorPeriod: tc.period}).Apply(context.Background())

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.period, p.period)
			// An unset reader falls back to the controller runtime pkg/manager configured
			// before this runs, so the poller always comes out able to read.
			assert.NotNil(t, p.reader)
		})
	}
}
