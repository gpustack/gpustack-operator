package worker

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestModelArtifactProgressTimeout(t *testing.T) {
	// 64 nodes in waves of 16 make 4 waves of the 2s node timeout; the fixed part gets 5s.
	assert.Equal(t, 13*time.Second, modelArtifactProgressTimeout(),
		"one answer's budget covers a full live fan-out, so the last wave is not born canceled")
}
