package ascend

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	klog "k8s.io/klog/v2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

// rankTableV10 is the file found on the 950 lab host 179.67.12.2, captured verbatim. It is a
// well-formed A2-generation table -- eight ranks matching `npu-smi info -m`, status completed --
// which is the point: nothing about its CONTENT is wrong, and a check looking for staleness passes
// it.
const rankTableV10 = `{
  "version": "1.0",
  "server_count": "1",
  "status": "completed",
  "server_list": [
    {
      "server_id": "179.67.12.2",
      "device": [
        {"device_id": "0", "rank_id": "0"},
        {"device_id": "1", "rank_id": "1"}
      ]
    }
  ]
}
`

// rankTableV20 carries the version A5 demands. Only the version field is read, so the rest is the
// shape of the vendor's own v2.0 generator rather than a full fixture.
const rankTableV20 = `{
  "version": "2.0",
  "status": "completed",
  "rank_count": "2",
  "rank_list": [
    {"rank_id": 0, "local_id": 0, "device_id": 0},
    {"rank_id": 1, "local_id": 1, "device_id": 1}
  ]
}
`

// writeRankTable puts content where a test can point the check at it. An empty content means the
// file is never created -- the healthy node, and the case this check inverts relative to its
// neighbor.
func writeRankTable(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "hccl_rootinfo.json")
	if content == "" {
		return path
	}
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// The one question this check answers: would a MULTI-CARD allocation on this node get HCCL up?
//
// The absent case is the load-bearing one. It is the state of every host that never ran
// mindcluster-tools -- both 910B2 lab hosts, and any clean A5 -- so reporting it unavailable would
// fire on correct nodes, and it is the exact opposite of what the neighboring runtime check does
// with a file it cannot read.
func TestCheckRankTable(t *testing.T) {
	cases := []struct {
		name string
		// content is the file's bytes, or empty for a node carrying no ranktable at all.
		content string
		want    device.PreflightState
		// wantVersion, when set, must appear in the row's reason: an operator needs to see which
		// version was found, not merely that one was wrong.
		wantVersion string
	}{
		{
			name: "no file at all is the healthy node",
			want: device.PreflightStateOK,
		},
		{
			name:    "a 2.0 table is the generation A5 demands",
			content: rankTableV20,
			want:    device.PreflightStateOK,
		},
		{
			name:        "the 950 host's own 1.0 table breaks multi-card HCCL",
			content:     rankTableV10,
			want:        device.PreflightStateUnavailable,
			wantVersion: "1.0",
		},
		{
			// 1.2 is the A3 generation. It is not "closer to right" than 1.0 -- anything but 2.0 is
			// refused the same way, which is what keeps the check from growing a version ladder.
			name:        "a 1.2 table is the A3 generation, not this one",
			content:     `{"version": "1.2"}`,
			want:        device.PreflightStateUnavailable,
			wantVersion: "1.2",
		},
		{
			// The runtime mounts the file on presence alone, so being unable to read it is not being
			// safe from it.
			name:    "unparseable is not given the benefit of the doubt",
			content: "this is not json",
			want:    device.PreflightStateUnavailable,
		},
		{
			name:    "a table naming no version",
			content: `{"server_count": "1", "status": "completed"}`,
			want:    device.PreflightStateUnavailable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := checkRankTable(writeRankTable(t, c.content))

			assert.Equal(t, c.want, got.State)
			assert.Equal(t, rankTableCapability, got.Capability)

			// Reason is empty exactly when the state is ok, and the detail says what was established.
			if c.want == device.PreflightStateOK {
				assert.Empty(t, got.Reason)
				assert.NotEmpty(t, got.Detail)
				return
			}
			assert.NotEmpty(t, got.Reason)
			if c.wantVersion != "" {
				assert.Contains(t, got.Reason, c.wantVersion,
					"the reason must name the version that was found")
			}
		})
	}
}

// An unmounted host root reaches the same os.ErrNotExist as a clean node, and only one of them is
// healthy.
//
// No case in the table above can tell the two apart: every one points at a directory t.TempDir()
// made, so all of them observe a host /etc that is there. The runner does not stop on a host root it
// could not validate -- it downgrades the pass to a dry run and carries on -- so this branch is
// reached on real nodes, and reporting ok there would clear a host whose 1.0 ranktable was never
// read.
//
// The two halves differ in one directory and nothing else, which is what makes the assertion about
// the guard rather than about tempdir layout.
func TestCheckRankTable_UnmountedHostRootIsNotAHealthyNode(t *testing.T) {
	root := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(root, "mounted", "etc"), 0o750))
	mounted := checkRankTable(filepath.Join(root, "mounted", hcclRootInfoPath))
	require.Equal(t, device.PreflightStateOK, mounted.State,
		"a host root that IS mounted and carries no ranktable is still the healthy node")

	unmounted := checkRankTable(filepath.Join(root, "unmounted", hcclRootInfoPath))

	assert.Equal(t, device.PreflightStateUnavailable, unmounted.State,
		"a host root with no etc was never looked in, so a missing ranktable says nothing about it")
	assert.Contains(t, unmounted.Reason, "host root",
		"the reason must send the operator to the mount rather than to their ranktable")
	assert.Empty(t, unmounted.Detail)
}

// The path this check reads must resolve to the HOST's file, not this container's.
//
// No table case above can establish that: each injects a path of its own, so every one of them
// would pass just as well against a check hard-wired to the container's /etc -- which is exactly the
// defect this asserts against. The failure it guards is silent and total: /etc is bind-mounted
// nowhere, so an unjoined read finds nothing, takes the absent-is-ok branch, and reports a healthy
// node on every host including one whose v1.0 ranktable is breaking every multi-card job on it.
//
// installInfo is asserted beside it as the contrast: /usr/local/Ascend IS mounted at its own name,
// so that one is correct unjoined, and the pair records which paths need the join and why.
func TestNewPreflighter_ReadsTheHostsRankTable(t *testing.T) {
	p, ok := NewPreflighter(device.PreflighterOptions{
		Logger:   klog.Background(),
		HostRoot: "/host",
	}).(*preflighter)
	require.True(t, ok, "NewPreflighter returns the concrete preflighter")

	assert.Equal(t, "/host"+hcclRootInfoPath, p.rootInfo,
		"the ranktable is read from the host root, since /etc is bind-mounted nowhere")
	assert.Equal(t, dockerRuntimeInstallInfo, p.installInfo,
		"the runtime's install info is NOT joined: /usr/local/Ascend is mounted at its own name")
}

// The ranktable is a precondition for A5 and for nothing else.
//
// A node carrying both generations is the discriminating fixture, for the reason the runtime row's
// own test gives: on a single-family node, "emitted where an A5 is present" and "emitted on every
// group" produce identical output, so the family test could be deleted and the test would pass.
func TestPreflightAccelerator_RankTableRowIsA5Only(t *testing.T) {
	p := &preflighter{
		logger:      klog.Background(),
		share:       &fakeShareDriver{enabled: map[[2]int32]bool{}},
		installInfo: writeInstallInfo(t, installInfoV2610),
		rootInfo:    writeRankTable(t, rankTableV10),
	}
	devs := ascendDevicesFixture()
	a5 := *devs.Spec.Groups[0].DeepCopy()
	a5.ID, a5.Family = "950", family950
	a5.Accelerators = []workercore.Accelerator{{ID: "a5-0", Index: 0, PhysicalIndexes: []uint32{7, 3, 0}}}
	devs.Spec.Groups = append(devs.Spec.Groups, a5)

	grp := p.PreflightAccelerator(devs.Spec.Groups)

	var carried []string
	for _, check := range grp.Checks {
		if check.Capability != rankTableCapability {
			continue
		}
		assert.Equal(t, device.PreflightStateUnavailable, check.State)
		carried = append(carried, check.Accelerator+"/"+check.Mode)
	}
	sort.Strings(carried)
	assert.Equal(t, []string{
		"a5-0/exclusive", "a5-0/shared", "a5-0/sliced", "a5-0/visibility",
	}, carried, "the ranktable row is filed on the A5 accelerator alone, once per mode it is a "+
		"precondition for, so a report filtered by either still carries it")
}
