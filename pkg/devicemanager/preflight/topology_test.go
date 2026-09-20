package preflight

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
)

// writeKubeletFiles lays one case's kubelet configuration into a fixture host root. Keys are paths
// relative to the root; unreadable names a path where the kubelet's own file belongs but a
// directory stands, which os.ReadFile fails on whatever user runs this test.
func writeKubeletFiles(t *testing.T, root string, files map[string]string, unreadable string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	if unreadable != "" {
		require.NoError(t, os.MkdirAll(filepath.Join(root, unreadable), 0o755))
	}
}

// The policy is a fact about the kubelet, and the three places a kubelet keeps its configuration
// are the only places this report claims to have looked. Each has to answer when it carries the
// key, and none answering has to read as a question left open -- never as the kubelet's default,
// which would be a value nobody read.
func TestTopologyReport(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	testCases := []struct {
		name       string
		files      map[string]string
		unreadable string
		wantPolicy string
		// wantNote is a substring; empty means the report must carry no note.
		wantNote string
	}{
		{
			name: "a kubeadm node's flag file names the policy",
			files: map[string]string{
				"var/lib/kubelet/kubeadm-flags.env": `KUBELET_KUBEADM_ARGS="--topology-manager-policy=single-numa-node --pod-infra-container-image=x"`,
			},
			wantPolicy: "single-numa-node",
		},
		{
			name: "a file-configured kubelet names the policy",
			files: map[string]string{
				"var/lib/kubelet/config.yaml": "apiVersion: kubelet.config.k8s.io/v1beta1\n" +
					"kind: KubeletConfiguration\ntopologyManagerPolicy: restricted\n",
			},
			wantPolicy: "restricted",
		},
		{
			// Measured on a k3s node: neither standard file exists, and the kubelet's configuration
			// is a drop-in under the distribution's own tree. Reading only the two would report
			// unknown on every such node while the policy sits one directory away.
			name: "a drop-in under the distribution's tree names the policy",
			files: map[string]string{
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00-k3s-defaults.conf": "topologyManagerPolicy: single-numa-node\n",
			},
			wantPolicy: "single-numa-node",
		},
		{
			// Files within one drop-in directory are one configuration applied in name order, so
			// the later file's policy is the node's. Taking the first would report a policy the
			// kubelet has already been told to ignore.
			name: "the later drop-in in one directory overrides the earlier",
			files: map[string]string{
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00-defaults.conf": "topologyManagerPolicy: none\n",
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/99-override.conf": "topologyManagerPolicy: restricted\n",
			},
			wantPolicy: "restricted",
		},
		{
			// The standard file is read before the distribution drop-in for the reason the CRI
			// reader reads it first: a kubelet reading the standard path reads it whatever else is
			// on disk, while a drop-in means something only to the distribution that wrote it.
			name: "the standard config file is taken over a distribution drop-in",
			files: map[string]string{
				"var/lib/kubelet/config.yaml":                          "topologyManagerPolicy: restricted\n",
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00.conf": "topologyManagerPolicy: best-effort\n",
			},
			wantPolicy: "restricted",
		},
		{
			// The case the section's vocabulary exists for: a host that sets the policy nowhere is
			// reported unknown, never the kubelet's default none. The policy can also arrive on a
			// command line none of the sources reads, and a default reported as if it had been read
			// publishes a measurement nobody took.
			name: "a host setting the policy nowhere is unknown, and the note says why",
			files: map[string]string{
				"var/lib/kubelet/config.yaml": "apiVersion: kubelet.config.k8s.io/v1beta1\naddress: 0.0.0.0\n",
			},
			wantPolicy: TopologyPolicyUnknown,
			wantNote:   "command line",
		},
		{
			// The bare machine, before any cluster exists: nothing to read at all, which is the
			// same unknown rather than a special one.
			name:       "a host carrying no kubelet configuration at all is unknown too",
			wantPolicy: TopologyPolicyUnknown,
			wantNote:   "command line",
		},
		{
			// A configuration that cannot be read is not a node with no policy: these patterns
			// name the kubelet's own paths, so a match that cannot be read may be the one that
			// decides. Reporting the generic unknown here would claim every file was searched.
			name:       "an unreadable kubelet configuration is not a host with no policy",
			unreadable: "var/lib/kubelet/config.yaml",
			wantPolicy: TopologyPolicyUnknown,
			wantNote:   "could not be read",
		},
		{
			// Two distribution trees are two configurations, and only one of them belongs to the
			// kubelet that is running. Reporting either would publish a policy possibly not in
			// force, silently.
			name: "two distribution trees naming different policies refuse to pick one",
			files: map[string]string{
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00.conf":  "topologyManagerPolicy: restricted\n",
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/00.conf": "topologyManagerPolicy: best-effort\n",
			},
			wantPolicy: TopologyPolicyUnknown,
			wantNote:   "more than one topology manager policy",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeHostRoot(t)
			writeKubeletFiles(t, root, tc.files, tc.unreadable)

			got := topologyReport(root, now)

			assert.True(t, got.Timestamp.Equal(now),
				"timestamp = %v, want %v; a preflight reads mutable host state, so the reading "+
					"is only worth what its time claims", got.Timestamp, now)
			assert.Equal(t, tc.wantPolicy, got.Policy, "policy")
			assert.Equal(t, device.PreflightDepthDeclared, got.Depth,
				"the policy is read out of declared configuration, and nothing is run")
			if tc.wantNote == "" {
				assert.Empty(t, got.Note, "a read policy is its own account")
				return
			}
			assert.Contains(t, got.Note, tc.wantNote,
				"an unknown policy has to say which kind of unknown it is")
		})
	}
}

// The reader runs through the Preflighter's own host root, and a path that merely exists is not
// one. Reading the kubelet's configuration through an unvalidated root would answer out of this
// container's own filesystem -- a policy this node's kubelet never saw -- so the answer degrades
// to unknown and says that nothing was looked for at all.
func TestPreflightTopology(t *testing.T) {
	t.Run("reads the policy through a validated host root", func(t *testing.T) {
		root := fakeHostRoot(t)
		writeKubeletFiles(t, root, map[string]string{
			"var/lib/kubelet/config.yaml": "topologyManagerPolicy: single-numa-node\n",
		}, "")

		p := &Preflighter{host: newHostExec(root)}
		got := p.PreflightTopology()

		assert.Equal(t, "single-numa-node", got.Policy)
		assert.Empty(t, got.Note)
		assert.False(t, got.Timestamp.IsZero(), "a reading is worth what its time claims")
	})

	t.Run("a host root that did not validate is never read through", func(t *testing.T) {
		p := &Preflighter{host: newHostExec(t.TempDir())}

		got := p.PreflightTopology()

		assert.Equal(t, TopologyPolicyUnknown, got.Policy)
		assert.Equal(t, device.PreflightDepthDeclared, got.Depth)
		assert.Contains(t, got.Note, "never looked for",
			"the note has to say the files were not read, not that they were read and held nothing")
	})
}

// There is no test here pinning the topology reader's sources against the CRI reader's. There was
// one while the two kept separate tables, and it could still fail then. Both readings now go
// through readKubeletSetting over the single kubeletConfigSources, so a source added for one is
// added for both and a divergence cannot be written -- which leaves such a test asserting a
// tautology, and a test that cannot fail is worse than none: it reports coverage it does not have.
// What a setting still owns is its two spellings, and those are pinned per source by the fixture
// cases above reading a policy from each of the three.

// The error Report returns is what a script turns into an exit code, and what it answers is
// whether this node can serve the allocation modes its allocators offer. The kubelet's
// TopologyManager policy stops none of them -- it decides which placements the kubelet admits,
// not what this node can hand out -- so a topology row is diagnostic, never a verdict. Were it
// wired into that error, every script gating an install on `preflight` would start refusing
// nodes that allocate perfectly well.
func TestReportDoesNotFailOnAFailingTopologyRow(t *testing.T) {
	testCases := []struct {
		name       string
		unreadable bool
	}{
		{
			// The failing row: a host whose kubelet configuration exists and could not be read,
			// which is the section's worst shape -- not merely absent, but unestablished.
			name:       "a topology row that could not read the kubelet configuration",
			unreadable: true,
		},
		{
			// And the section's unknown shape, which is an honest answer rather than a failure,
			// still changes nothing about the exit code.
			name: "a topology row that found no policy anywhere",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeHostRoot(t)
			if tc.unreadable {
				writeKubeletFiles(t, root, nil, "var/lib/kubelet/config.yaml")
			}
			topology := topologyReport(root, time.Now())

			var buf bytes.Buffer
			err := Report(&buf, device.PreflightGroupList{{
				Manufacturer: "nvidia",
				Detection:    device.PreflightDetection{State: device.PreflightStateOK, Accelerators: 1},
			}}, NetworkReport{}, topology)
			require.NoError(t, err, "a topology row failed the pass")

			// And it is in the document, which is what makes the nil error a decision rather than
			// an omission: a report that dropped the section would pass this too.
			//
			// The policy is asserted through the rendered field and not by searching the document
			// for the word: one of these rows renders a note reading "is unknown rather than
			// defaulted", so a bare search for "unknown" is answered by the note whatever the
			// field holds -- and would pass on a report whose policy had gone empty.
			out := buf.String()
			assert.Contains(t, out, "topology:", "the document carries the topology section")
			assert.Contains(t, out, "policy: "+TopologyPolicyUnknown,
				"and the section's policy field says unknown")
		})
	}
}
