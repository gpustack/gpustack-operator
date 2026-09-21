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

// The policy is a fact about the kubelet, so the places this report claims to have looked are the
// places that kubelet named. Each has to answer when it carries the key, and none answering has to
// read as a question left open -- never as the kubelet's default, which would be a value nobody
// read.
func TestTopologyReport(t *testing.T) {
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	testCases := []struct {
		name string
		// kubelet is the command line of a kubelet running on the fixture host, empty for a host
		// running none. otherKubelet is a second one, planted at a higher PID that sorts before
		// the first by name.
		kubelet      []string
		otherKubelet []string
		// noProcessTable takes the fixture's process table away, which is the shape of a host root
		// brought into the container without the mounts below it.
		noProcessTable bool
		files          map[string]string
		unreadable     string
		wantPolicy     string
		// wantNote is a substring; empty means the report must carry no note.
		wantNote string
	}{
		{
			// The node this was measured on: its kubelet was started against a configuration file
			// of the distribution's own choosing, and a different file, with different contents,
			// also sat at the standard path. Reading the standard path there reports a policy out
			// of a file this node's kubelet never opened -- so the file the kubelet named is read
			// and the one it did not is not read at all. The name is deliberately one no list of
			// standard paths could hold: a reader that answers this by learning one more path has
			// not learned where to look, only where to look twice.
			name:    "the file the kubelet names is read, and the standard path it does not use is not",
			kubelet: []string{"/usr/bin/kubelet", "--config=/opt/kubelet/config.yaml"},
			files: map[string]string{
				"opt/kubelet/config.yaml":     "topologyManagerPolicy: single-numa-node\n",
				"var/lib/kubelet/config.yaml": "topologyManagerPolicy: none\n",
			},
			wantPolicy: "single-numa-node",
		},
		{
			// The kubelet re-parses its command line after loading its configuration file, so a
			// policy spelled there is the one in force whatever the file says. This is also the
			// spelling with a space rather than an equals sign, which a command line uses and a
			// configuration file has no form of.
			name: "a policy on the kubelet's command line overrides the file it named",
			kubelet: []string{
				"/usr/bin/kubelet", "--config", "/opt/kubelet/config.yaml",
				"--topology-manager-policy=restricted",
			},
			files:      map[string]string{"opt/kubelet/config.yaml": "topologyManagerPolicy: none\n"},
			wantPolicy: "restricted",
		},
		{
			// The kubelet merges its drop-in directory over its configuration file, so the
			// directory's answer is the one in force. Reading the file first would report a policy
			// the kubelet has already been told to ignore.
			name: "the drop-in directory the kubelet names overrides the file it names",
			kubelet: []string{
				"/usr/bin/kubelet", "--config=/opt/kubelet/config.yaml",
				"--config-dir=/opt/kubelet/conf.d",
			},
			files: map[string]string{
				"opt/kubelet/config.yaml":           "topologyManagerPolicy: none\n",
				"opt/kubelet/conf.d/10-numa.conf":   "topologyManagerPolicy: single-numa-node\n",
				"var/lib/kubelet/kubeadm-flags.env": `KUBELET_KUBEADM_ARGS="--topology-manager-policy=best-effort"`,
			},
			wantPolicy: "single-numa-node",
		},
		{
			// A kubelet given no configuration file reads none, so a file at the standard path is a
			// file it never opens. Answering out of it would publish a policy this node's kubelet
			// is demonstrably not running.
			name:       "a kubelet configured by flags alone is not answered out of the standard path",
			kubelet:    []string{"/usr/bin/kubelet", "--kubeconfig=/etc/kubernetes/kubelet.conf"},
			files:      map[string]string{"var/lib/kubelet/config.yaml": "topologyManagerPolicy: restricted\n"},
			wantPolicy: TopologyPolicyUnknown,
			wantNote:   "command line",
		},
		{
			// Without the host's own process table there is no way to tell which kubelet is running
			// or what it was started against, and the standard paths are exactly the guess that
			// reads the wrong file. An unknown that says so is the answer; a policy read from a
			// file nobody established the kubelet uses is not.
			name:           "a host root without the host's process table reports nothing rather than a guess",
			noProcessTable: true,
			files:          map[string]string{"var/lib/kubelet/config.yaml": "topologyManagerPolicy: restricted\n"},
			wantPolicy:     TopologyPolicyUnknown,
			wantNote:       "process",
		},
		{
			// Two kubelets on one host is a host to fix, but the reading still has to be the same
			// on every pass. A directory listing of the process table is in name order, which puts
			// PID 10 before PID 2, so the lowest PID answers rather than the first listed -- the
			// host's own kubelet is started by its init, before anything a workload could add.
			name:         "the lowest PID answers where a host carries more than one kubelet",
			kubelet:      []string{"/usr/bin/kubelet", "--config=/opt/kubelet/first.yaml"},
			otherKubelet: []string{"/usr/bin/kubelet", "--config=/opt/kubelet/second.yaml"},
			files: map[string]string{
				"opt/kubelet/first.yaml":  "topologyManagerPolicy: single-numa-node\n",
				"opt/kubelet/second.yaml": "topologyManagerPolicy: none\n",
			},
			wantPolicy: "single-numa-node",
		},
		{
			// A distribution that embeds the kubelet in its own agent process runs nothing called
			// kubelet, so there is no command line to read and the places such a distribution keeps
			// its configuration are what answers. Refusing to read them would report unknown on
			// every node of the kind this report was fixed for once already.
			name: "a host running no kubelet process still reads where a distribution keeps one",
			files: map[string]string{
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/20-cli-config-dir/10-topology.conf": "topologyManagerPolicy: " +
					"single-numa-node\n",
			},
			wantPolicy: "single-numa-node",
		},
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
			// The shape the distributions actually produce. Their managed tree is regenerated on
			// every start, so an administrator is not offered a file in it to edit but a flag
			// naming a directory of their own, whose contents the distribution copies into a
			// subdirectory here. The setting that decides the node therefore sits one level below
			// the generated defaults -- and a node configured the way its distribution documents
			// is exactly the node this has to answer for.
			name: "a drop-in the distribution copied into a subdirectory names the policy",
			files: map[string]string{
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/00-rke2-defaults.conf": "kind: KubeletConfiguration\n",
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/20-cli-config-dir/10-topology.conf": "topologyManagerPolicy: " +
					"single-numa-node\n",
			},
			wantPolicy: "single-numa-node",
		},
		{
			// One tree is one configuration however deep its files sit: the kubelet merges the
			// whole tree in a single pass, the later file overriding the earlier. Reading the
			// subdirectory as a second configuration would turn this override into a conflict and
			// report a node that is configured exactly as documented as one whose policy cannot be
			// established -- which is why the fix is where the files are grouped and not only
			// which files are matched.
			name: "a subdirectory drop-in overrides the tree's defaults rather than conflicting with them",
			files: map[string]string{
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/00-rke2-defaults.conf": "topologyManagerPolicy: none\n",
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/20-cli-config-dir/10-topology.conf": "topologyManagerPolicy: " +
					"single-numa-node\n",
			},
			wantPolicy: "single-numa-node",
		},
		{
			// Two trees stay two configurations whatever depth their files sit at. Grouping by
			// tree is what keeps this a conflict; grouping by the directory a file happens to sit
			// in would put these two in separate groups for the wrong reason and get the right
			// answer by accident.
			name: "two distribution trees conflict even when one answers from a subdirectory",
			files: map[string]string{
				"var/lib/rancher/k3s/agent/etc/kubelet.conf.d/00.conf":                    "topologyManagerPolicy: restricted\n",
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/20-cli-config-dir/10.conf": "topologyManagerPolicy: best-effort\n",
			},
			wantPolicy: TopologyPolicyUnknown,
			wantNote:   "more than one topology manager policy",
		},
		{
			// The kubelet merges .conf out of this tree and skips everything else, so a policy
			// read from any other file is one it never applied. Walking the tree makes this the
			// reader's own decision rather than a side effect of the pattern it matched on.
			name: "a file the kubelet would not merge is not read",
			files: map[string]string{
				"var/lib/rancher/rke2/agent/etc/kubelet.conf.d/20-cli-config-dir/10-topology.yaml": "topologyManagerPolicy: " +
					"single-numa-node\n",
			},
			wantPolicy: TopologyPolicyUnknown,
			wantNote:   "command line",
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
			if len(tc.kubelet) > 0 {
				writeHostProcess(t, root, 2, tc.kubelet...)
			}
			if len(tc.otherKubelet) > 0 {
				writeHostProcess(t, root, 10, tc.otherKubelet...)
			}
			if tc.noProcessTable {
				require.NoError(t, os.RemoveAll(filepath.Join(root, "proc", "1")))
			}

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
