package exporter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
)

// TestPoller_exporting pins the rule that keeps a two-vendor node from publishing every
// Instance twice: the manufacturer sorting first among the node's Ready device manager pods
// exports, which is a total order needing no shared state and handing over by itself.
func TestPoller_exporting(t *testing.T) {
	const nodeName = "node-role"

	testCases := []struct {
		name string

		// selfManufacturer is the manufacturer of this process's own pod.
		selfManufacturer string
		// peers are the node's other device manager pods.
		peers []*core.Pod

		want bool
	}{
		{
			name:             "the only device manager of a node exports",
			selfManufacturer: "nvidia",
			want:             true,
		},
		{
			name:             "the first-sorting manufacturer exports",
			selfManufacturer: "amd",
			peers: []*core.Pod{
				deviceManagerPodFixture(nodeName, "dm-nvidia", "nvidia"),
			},
			want: true,
		},
		{
			name:             "a later-sorting manufacturer stays quiet",
			selfManufacturer: "nvidia",
			peers: []*core.Pod{
				deviceManagerPodFixture(nodeName, "dm-amd", "amd"),
			},
		},
		{
			name:             "an unready peer does not hold the role",
			selfManufacturer: "nvidia",
			peers: []*core.Pod{
				deviceManagerPodFixture(nodeName, "dm-amd", "amd", notReady()),
			},
			want: true,
		},
		{
			name:             "a terminating peer hands the role over",
			selfManufacturer: "nvidia",
			peers: []*core.Pod{
				deviceManagerPodFixture(nodeName, "dm-amd", "amd", terminating()),
			},
			want: true,
		},
		{
			name:             "a peer on another node is not a peer",
			selfManufacturer: "nvidia",
			peers: []*core.Pod{
				deviceManagerPodFixture("some-other-node", "dm-amd", "amd"),
			},
			want: true,
		},
		{
			name:             "an unready self does not export",
			selfManufacturer: "",
			peers: []*core.Pod{
				deviceManagerPodFixture(nodeName, testSelfPodName, "nvidia", notReady()),
			},
		},
		{
			// An empty manufacturer sorts before every real one, so counting such a peer as
			// the winner would leave the node with no exporter at all: the labeled pods defer
			// to it, and it cannot recognize itself as the winner either. A pod that does not
			// say which manufacturer it serves is not in the running.
			name:             "an unlabeled peer does not take the role away",
			selfManufacturer: "nvidia",
			peers: []*core.Pod{
				deviceManagerPodFixture(nodeName, "dm-unlabeled", ""),
			},
			want: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pods := tc.peers
			if tc.selfManufacturer != "" {
				pods = append(pods,
					deviceManagerPodFixture(nodeName, testSelfPodName, tc.selfManufacturer))
			}

			p := newTestPollerWith(nodeName, pods...)
			got, err := p.exporting(context.Background())

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("reports an unresolvable identity instead of guessing", func(t *testing.T) {
		// Without knowing which pod it is, this process cannot know whether it holds the role;
		// exporting anyway would duplicate every series of the node that does.
		t.Setenv("KUBERNETES_POD_NAME", "")

		p := newTestPoller(nodeName)
		_, err := p.exporting(context.Background())
		require.Error(t, err)
	})

	t.Run("refuses to decide without a namespace to compare within", func(t *testing.T) {
		// An empty namespace does not mean "this pod's own" — it means every namespace, so the
		// election would weigh this pod against the device managers of any other install of the
		// operator running on the node and could defer to one of theirs.
		t.Setenv("KUBERNETES_POD_NAMESPACE", "")

		p := newTestPoller(nodeName)
		_, err := p.exporting(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "KUBERNETES_POD_NAMESPACE")
	})

	t.Run("a device manager the informer has not seen yet stays quiet", func(t *testing.T) {
		p := newTestPollerWith(nodeName, deviceManagerPodFixture(nodeName, "dm-amd", "amd"))

		got, err := p.exporting(context.Background())
		require.NoError(t, err)
		assert.False(t, got)
	})
}
