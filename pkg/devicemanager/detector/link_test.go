package detector

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

// addRDMAPorts builds an RDMA device's port tree and returns its path relative to the sysfs root.
//
// A port whose attribute map is nil gets its directory and no attributes, which is how "the port is
// there and its state could not be read" is expressed — the case that must never reach `failed`.
func (f *sysfsFixture) addRDMAPorts(rdmaRel string, ports map[string]map[string]string) string {
	f.t.Helper()
	f.mkdir(rdmaRel)
	for port, attrs := range ports {
		portRel := filepath.Join(rdmaRel, "ports", port)
		f.mkdir(portRel)
		for k, v := range attrs {
			f.write(filepath.Join(portRel, k), v)
		}
	}
	return rdmaRel
}

// TestCheckRDMALink pins the three link states and, above all, which reads may reach each one.
//
// `failed` withholds a node label, so the criterion this table exists for is that no unreadable
// file can ever produce it: an inability to ask must not read as an answer of no.
func TestCheckRDMALink(t *testing.T) {
	const (
		activeState = "4: ACTIVE"
		deferState  = "5: ACTIVE_DEFER"
		downState   = "1: DOWN"
		linkUp      = "5: LinkUp"
		disabled    = "3: Disabled"
	)

	testCases := []struct {
		name  string
		ports map[string]map[string]string
		// portsDir is false when the RDMA device has no ports directory at all.
		portsDir     bool
		wantState    workercore.DeviceInterfaceLinkState
		wantContains []string
	}{
		{
			name:      "an active port whose physical link is up verifies",
			ports:     map[string]map[string]string{"1": {"state": activeState, "phys_state": linkUp}},
			portsDir:  true,
			wantState: workercore.DeviceInterfaceLinkStateOK,
		},
		{
			// Both attributes are consulted, not just the first. A port can report the transport
			// layer active while the physical link is administratively off.
			name:         "an active port whose physical link is down fails",
			ports:        map[string]map[string]string{"1": {"state": activeState, "phys_state": disabled}},
			portsDir:     true,
			wantState:    workercore.DeviceInterfaceLinkStateFailed,
			wantContains: []string{activeState, disabled},
		},
		{
			name:         "a down port fails, carrying both values verbatim",
			ports:        map[string]map[string]string{"1": {"state": downState, "phys_state": disabled}},
			portsDir:     true,
			wantState:    workercore.DeviceInterfaceLinkStateFailed,
			wantContains: []string{downState, disabled},
		},
		{
			// A state whose NAME merely starts with a verifying one must not verify. ACTIVE_DEFER
			// is a port that has lost its link and is not carrying user traffic, so accepting it
			// publishes the node's RDMA label over a link nothing can use. It reaches `failed`
			// rather than `unverified` because both files were read and neither carried the link.
			name:         "an ACTIVE_DEFER port does not verify, even with the physical link up",
			ports:        map[string]map[string]string{"1": {"state": deferState, "phys_state": linkUp}},
			portsDir:     true,
			wantState:    workercore.DeviceInterfaceLinkStateFailed,
			wantContains: []string{deferState, linkUp},
		},
		{
			// The converse of the above, so the exact-name comparison cannot be satisfied by
			// refusing everything: the real ACTIVE still verifies.
			name:      "the exact ACTIVE state still verifies",
			ports:     map[string]map[string]string{"1": {"state": activeState, "phys_state": linkUp}},
			portsDir:  true,
			wantState: workercore.DeviceInterfaceLinkStateOK,
		},
		{
			// Every port is consulted. Stopping at the first would report a multi-port HCA as
			// unusable whenever its first port happens to be the unused one.
			name: "a later port carrying the link verifies the device",
			ports: map[string]map[string]string{
				"1": {"state": downState, "phys_state": disabled},
				"2": {"state": activeState, "phys_state": linkUp},
			},
			portsDir:  true,
			wantState: workercore.DeviceInterfaceLinkStateOK,
		},
		{
			name:         "no ports directory is unverified, and names the path",
			portsDir:     false,
			wantState:    workercore.DeviceInterfaceLinkStateUnverified,
			wantContains: []string{"ports"},
		},
		{
			name:         "a port with no readable state is unverified",
			ports:        map[string]map[string]string{"1": nil},
			portsDir:     true,
			wantState:    workercore.DeviceInterfaceLinkStateUnverified,
			wantContains: []string{"ports"},
		},
		{
			// THE criterion. One port is genuinely down, the other cannot be read — so "all ports
			// are down" is not established, and `failed` would withhold a label on the strength of
			// a file that could not be opened.
			name: "a down port beside an unreadable one is unverified, never failed",
			ports: map[string]map[string]string{
				"1": {"state": downState, "phys_state": disabled},
				"2": nil,
			},
			portsDir:     true,
			wantState:    workercore.DeviceInterfaceLinkStateUnverified,
			wantContains: []string{downState, "2"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSysfsFixture(t)
			rdmaRel := "devices/pci0000:00/0000:00:01.0/0000:01:00.0/infiniband/mlx5_0"
			if tc.portsDir {
				f.addRDMAPorts(rdmaRel, tc.ports)
			} else {
				f.mkdir(rdmaRel)
			}

			tree, err := newSysfsTree(f.root)
			if err != nil {
				t.Fatalf("newSysfsTree: %v", err)
			}

			link := tree.checkRDMALink(rdmaRel)
			if link == nil {
				t.Fatal("a bound RDMA device produced no link record; every state, " +
					"including the unverified ones, has to be reported in words")
			}
			if link.State != tc.wantState {
				t.Errorf("state = %q, want %q (reason %q)", link.State, tc.wantState, link.Reason)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(link.Reason, want) {
					t.Errorf("reason %q does not carry %q", link.Reason, want)
				}
			}
			if tc.wantState == workercore.DeviceInterfaceLinkStateOK && link.Reason != "" {
				t.Errorf("a verified link carries a reason %q; only the two states that need "+
					"explaining have words", link.Reason)
			}
			// The first-seen time is assigned by the pass, not by the checker, because it has to
			// come from what was already stored rather than from this read.
			if link.FirstSeenTime != nil {
				t.Errorf("the checker set a first-seen time %v; that is the pass's decision",
					link.FirstSeenTime)
			}
		})
	}
}

// TestCheckRDMALinkReasonIsDeterministic pins the reason a multi-port device produces.
//
// The reason is part of the published record, so two reads of unchanged hardware must produce the
// same bytes. If port order leaked into it, every pass would compare unequal and rewrite the object
// forever with correct data in it the whole time (P8).
func TestCheckRDMALinkReasonIsDeterministic(t *testing.T) {
	f := newSysfsFixture(t)
	rdmaRel := "devices/pci0000:00/0000:00:01.0/0000:01:00.0/infiniband/mlx5_0"
	f.addRDMAPorts(rdmaRel, map[string]map[string]string{
		"2": {"state": "1: DOWN", "phys_state": "3: Disabled"},
		"1": {"state": "1: DOWN", "phys_state": "3: Disabled"},
		"3": {"state": "1: DOWN", "phys_state": "3: Disabled"},
	})

	tree, err := newSysfsTree(f.root)
	if err != nil {
		t.Fatalf("newSysfsTree: %v", err)
	}

	first := tree.checkRDMALink(rdmaRel)
	second := tree.checkRDMALink(rdmaRel)
	if first.Reason != second.Reason {
		t.Fatalf("two reads produced different reasons:\n%q\n%q", first.Reason, second.Reason)
	}
	want := `the RDMA link is not usable: ` +
		`port 1: state="1: DOWN" phys_state="3: Disabled"; ` +
		`port 2: state="1: DOWN" phys_state="3: Disabled"; ` +
		`port 3: state="1: DOWN" phys_state="3: Disabled"`
	if first.Reason != want {
		t.Errorf("reason =\n%q\nwant\n%q", first.Reason, want)
	}
}

// TestEnumerateInterfacesCarriesTheRDMALinkState pins that the pass actually runs the check.
//
// The check being correct in isolation says nothing about whether any interface record carries its
// verdict: an unwired checker is indistinguishable from one that was never written. This is the
// only test that fails if the call site disappears.
func TestEnumerateInterfacesCarriesTheRDMALinkState(t *testing.T) {
	f := newSysfsFixture(t)
	f.physicalNIC("eth0", "0000:01:00.0")
	f.addRDMA("0000:01:00.0", "mlx5_0", map[string]string{
		"state": "4: ACTIVE", "phys_state": "5: LinkUp",
	})
	f.physicalNIC("eth1", "0000:02:00.0")
	f.addRDMA("0000:02:00.0", "mlx5_1", map[string]string{
		"state": "1: DOWN", "phys_state": "3: Disabled",
	})
	// A NIC with no RDMA device at all. `rdma: false` already carries that fact, so a link state
	// here would be a verdict on a check that was never in question.
	f.physicalNIC("eth2", "0000:03:00.0")

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v", err)
	}

	for _, tc := range []struct {
		name      string
		wantState workercore.DeviceInterfaceLinkState
	}{
		{name: "eth0", wantState: workercore.DeviceInterfaceLinkStateOK},
		{name: "eth1", wantState: workercore.DeviceInterfaceLinkStateFailed},
		{name: "eth2", wantState: ""},
	} {
		got := findInterface(ifaces, tc.name)
		if got == nil {
			t.Fatalf("%s missing from %d interfaces", tc.name, len(ifaces))
		}
		switch {
		case tc.wantState == "":
			if got.Link != nil {
				t.Errorf("%s has a link record %+v with no RDMA device bound", tc.name, got.Link)
			}
		case got.Link == nil:
			t.Errorf("%s carries no link record; the check did not reach the inventory", tc.name)
		case got.Link.State != tc.wantState:
			t.Errorf("%s link state = %q, want %q", tc.name, got.Link.State, tc.wantState)
		}
	}
}

func failedLink(reason string, firstSeen *meta.Time) *workercore.DeviceInterfaceLink {
	return &workercore.DeviceInterfaceLink{
		State:         workercore.DeviceInterfaceLinkStateFailed,
		Reason:        reason,
		FirstSeenTime: firstSeen,
	}
}

// TestCarryLinkFirstSeen pins where a failure's first-seen time comes from.
//
// Two of this spec's own requirements collide here: the time must be stable while the failure
// persists, and an unchanged pass must compare equal to what is stored. Taking the current instant
// on every pass satisfies the first requirement's letter and breaks the second — with correct data
// in the object throughout, and write volume as the only symptom.
func TestCarryLinkFirstSeen(t *testing.T) {
	var (
		earlier = meta.NewTime(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
		now     = meta.NewTime(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	)

	testCases := []struct {
		name             string
		stored, detected []workercore.DeviceInterface
		wantFirstSeen    *meta.Time
	}{
		{
			name:          "a failure this pass first saw takes this pass's instant",
			stored:        []workercore.DeviceInterface{{Name: "eth0"}},
			detected:      []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", nil)}},
			wantFirstSeen: &now,
		},
		{
			// The create path, where there is nothing stored to carry from. It has its own call
			// site because an object created carrying a failure with no first-seen time would
			// publish an outage with no beginning until a later pass filled it in.
			name:          "with no stored inventory every failure is seen for the first time",
			stored:        nil,
			detected:      []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", nil)}},
			wantFirstSeen: &now,
		},
		{
			name: "a persisting failure keeps the stored instant",
			stored: []workercore.DeviceInterface{
				{Name: "eth0", Link: failedLink("down", &earlier)},
			},
			detected:      []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", nil)}},
			wantFirstSeen: &earlier,
		},
		{
			// The clock tracks the failure, not its wording. A second port going down changes the
			// reason and is still the same outage, which is the question an operator asks about.
			name: "a failure whose reason changed keeps the stored instant",
			stored: []workercore.DeviceInterface{
				{Name: "eth0", Link: failedLink("port 1 down", &earlier)},
			},
			detected: []workercore.DeviceInterface{
				{Name: "eth0", Link: failedLink("port 1 down; port 2 down", nil)},
			},
			wantFirstSeen: &earlier,
		},
		{
			// A different state is a different fault, so the clock restarts rather than reporting
			// this failure as having been seen while the link was merely unverifiable.
			name: "a state that changed takes this pass's instant",
			stored: []workercore.DeviceInterface{{Name: "eth0", Link: &workercore.DeviceInterfaceLink{
				State: workercore.DeviceInterfaceLinkStateUnverified, FirstSeenTime: &earlier,
			}}},
			detected:      []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", nil)}},
			wantFirstSeen: &now,
		},
		{
			// An object written before this field existed converges instead of staying blank
			// forever, which is what carrying the stored value unconditionally would do.
			name: "a stored failure with no instant is stamped",
			stored: []workercore.DeviceInterface{
				{Name: "eth0", Link: failedLink("down", nil)},
			},
			detected:      []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", nil)}},
			wantFirstSeen: &now,
		},
		{
			name:     "an interface absent from the stored inventory takes this pass's instant",
			stored:   []workercore.DeviceInterface{{Name: "eth1", Link: failedLink("down", &earlier)}},
			detected: []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", nil)}},
			// eth1's instant belongs to eth1; matching by position rather than by name would hand
			// it to a different interface.
			wantFirstSeen: &now,
		},
		{
			// Only a failure has a first-seen time. An unverified link is not an outage, so a time
			// there would answer "how long has this been broken?" about something that is not.
			//
			// The detected link arrives WITH a time so this asserts the clearing rather than the
			// checker's habit of not setting one. Were it nil here, the case would hold whatever
			// this function did, and the guarantee the comparison depends on would be untested.
			name:   "an unverified link carries no instant",
			stored: []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", &earlier)}},
			detected: []workercore.DeviceInterface{{Name: "eth0", Link: &workercore.DeviceInterfaceLink{
				State: workercore.DeviceInterfaceLinkStateUnverified, Reason: "no ports",
				FirstSeenTime: &earlier,
			}}},
			wantFirstSeen: nil,
		},
		{
			name:   "a cleared failure drops the instant",
			stored: []workercore.DeviceInterface{{Name: "eth0", Link: failedLink("down", &earlier)}},
			detected: []workercore.DeviceInterface{{Name: "eth0", Link: &workercore.DeviceInterfaceLink{
				State: workercore.DeviceInterfaceLinkStateOK, FirstSeenTime: &earlier,
			}}},
			wantFirstSeen: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			carryLinkFirstSeen(tc.stored, tc.detected, now)

			got := tc.detected[0].Link.FirstSeenTime
			switch {
			case tc.wantFirstSeen == nil && got != nil:
				t.Fatalf("first-seen = %v, want none", got)
			case tc.wantFirstSeen != nil && got == nil:
				t.Fatalf("first-seen = none, want %v", tc.wantFirstSeen)
			case tc.wantFirstSeen != nil && !got.Equal(tc.wantFirstSeen):
				t.Fatalf("first-seen = %v, want %v", got, tc.wantFirstSeen)
			}
		})
	}
}

// countingDevicesClient stands in for the Devices client and counts the writes made through it.
//
// Counting is the point. The defect this guards is a pass that recomputes a correct inventory and
// writes it every time, so the object's CONTENT is right in every failing case — no assertion on
// the stored value can see it. The number of writes is the only observable that moves.
type countingDevicesClient struct {
	stored  *workercore.Devices
	creates int
	updates int
}

func (c *countingDevicesClient) Get(
	_ context.Context, name string, _ meta.GetOptions,
) (*workercore.Devices, error) {
	if c.stored == nil {
		return nil, kerrors.NewNotFound(
			schema.GroupResource{Group: workercore.GroupVersion.Group, Resource: "devices"}, name)
	}
	return c.stored.DeepCopy(), nil
}

func (c *countingDevicesClient) Create(
	_ context.Context, obj *workercore.Devices, _ meta.CreateOptions,
) (*workercore.Devices, error) {
	c.creates++
	c.stored = obj.DeepCopy()
	return c.stored.DeepCopy(), nil
}

func (c *countingDevicesClient) Update(
	_ context.Context, obj *workercore.Devices, _ meta.UpdateOptions,
) (*workercore.Devices, error) {
	c.updates++
	c.stored = obj.DeepCopy()
	return c.stored.DeepCopy(), nil
}

// TestDetectPassWritesOnlyOnChange drives the real write path repeatedly and counts the writes.
//
// This is the acceptance criterion for the first-seen merge. Every pass here produces the same
// failing link at a different instant, which is exactly what a detector on a node with a broken
// link does — and if the instant reached the comparison, the writes would never stop.
//
// Only Update is counted because this path makes no other write: the align function is consulted
// first and the helper returns before touching the client when nothing changed, and the Devices
// status is written by the worker's reconciler rather than by this pass.
func TestDetectPassWritesOnlyOnChange(t *testing.T) {
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-1", UID: types.UID("node-1-uid")}}
	cli := &countingDevicesClient{}

	// pass runs one detect pass at the given instant and returns the writes it made.
	pass := func(t *testing.T, at meta.Time, detected []workercore.DeviceInterface) (creates, updates int) {
		t.Helper()

		beforeCreates, beforeUpdates := cli.creates, cli.updates
		expected := &workercore.Devices{
			ObjectMeta: meta.ObjectMeta{Name: node.Name},
			Spec:       workercore.DevicesSpec{Interfaces: detected},
		}
		kubemeta.ControlOnWithoutBlock(expected, node, core.SchemeGroupVersion.WithKind("Node"))
		// The create path stamps the instant too. Without it a first pass would store a failure
		// with no first-seen time, and every later pass would carry that absence forward.
		carryLinkFirstSeen(nil, expected.Spec.Interfaces, at)

		alignment := devicesAlignment{
			expected:       expected,
			node:           node,
			manufacturers:  sets.New[string](),
			syncInterfaces: true,
			now:            at,
		}
		if _, err := kubeclientset.Create(context.Background(), cli, expected,
			kubeclientset.WithUpdateIfExisted(alignment.apply)); err != nil {
			t.Fatalf("sync Devices: %v", err)
		}
		return cli.creates - beforeCreates, cli.updates - beforeUpdates
	}

	broken := func() []workercore.DeviceInterface {
		return []workercore.DeviceInterface{{
			Name: "eth0", RDMA: true, RDMADevice: "mlx5_0",
			Link: failedLink(`port 1: state="1: DOWN" phys_state="3: Disabled"`, nil),
		}}
	}

	firstPass := meta.NewTime(time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))
	if creates, updates := pass(t, firstPass, broken()); creates != 1 || updates != 0 {
		t.Fatalf("the first pass made %d creates and %d updates, want 1 and 0", creates, updates)
	}

	for i, at := range []meta.Time{
		meta.NewTime(firstPass.Add(time.Hour)),
		meta.NewTime(firstPass.Add(2 * time.Hour)),
	} {
		_, updates := pass(t, at, broken())
		if updates != 0 {
			t.Fatalf("pass %d issued %d writes with nothing changed; a failing link must not "+
				"rewrite the object on every pass", i+2, updates)
		}
	}

	stored := cli.stored.Spec.Interfaces[0].Link
	if stored.FirstSeenTime == nil || !stored.FirstSeenTime.Equal(&firstPass) {
		t.Fatalf("first-seen = %v after three passes, want %v — the operator's question is how "+
			"long the link has been broken", stored.FirstSeenTime, firstPass)
	}

	// A real change still writes, which is what makes the three zero-write passes above evidence
	// of a working comparison rather than of a comparison that never fires.
	repaired := []workercore.DeviceInterface{{
		Name: "eth0", RDMA: true, RDMADevice: "mlx5_0",
		Link: &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
	}}
	if _, updates := pass(t, meta.NewTime(firstPass.Add(3*time.Hour)), repaired); updates != 1 {
		t.Fatalf("a repaired link produced %d writes, want 1", updates)
	}
	if got := cli.stored.Spec.Interfaces[0].Link; got.FirstSeenTime != nil {
		t.Errorf("a repaired link kept its first-seen time %v", got.FirstSeenTime)
	}
}

// TestInterfacesChanged pins what takes the detect loop round again.
//
// The accelerator device key carries nothing about the network, so before this comparison existed
// the monitor loop kept spinning on unchanged accelerator keys while a link that went down was
// never re-read: `rdma.capable` stayed published on a broken link until an accelerator changed or
// the pod restarted. A gate that needs an unrelated event to fire is not a gate.
//
// The last case is the other half, and it is the one with no wrong value to observe: if first-seen
// times took part in this comparison, every tick would differ from the last and the object would be
// rewritten forever with correct data in it the whole time.
func TestInterfacesChanged(t *testing.T) {
	iface := func(name string, state workercore.DeviceInterfaceLinkState,
		firstSeen *meta.Time,
	) workercore.DeviceInterface {
		out := workercore.DeviceInterface{Name: name, Bus: "pci", RDMA: true, RDMADevice: "mlx5_0"}
		if state != "" {
			out.Link = &workercore.DeviceInterfaceLink{State: state, FirstSeenTime: firstSeen}
		}
		return out
	}
	stamp := meta.NewTime(meta.Now().Add(-time.Hour))

	// noBaseline rather than its complement, so the zero value is "there is a baseline" — the state
	// every case below but two is in. Spelling it the other way round would let a case that forgot
	// the field still pass whenever it wants true, taking the no-baseline shortcut instead of the
	// comparison it was written to exercise.
	testCases := []struct {
		name               string
		reported, detected []workercore.DeviceInterface
		noBaseline         bool
		detectedErr        error
		want               bool
	}{
		{
			name:     "an unchanged record is not a change",
			reported: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil)},
			detected: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil)},
			want:     false,
		},
		{
			// THE criterion. Nothing about this changes an accelerator's device key.
			name:     "a link going down is a change",
			reported: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil)},
			detected: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateFailed, nil)},
			want:     true,
		},
		{
			name:     "a link recovering is a change too",
			reported: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateFailed, nil)},
			detected: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil)},
			want:     true,
		},
		{
			name:     "an interface appearing is a change",
			reported: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil)},
			detected: []workercore.DeviceInterface{
				iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil),
				iface("eth1", workercore.DeviceInterfaceLinkStateOK, nil),
			},
			want: true,
		},
		{
			// The recovery case, and the one with nothing wrong to observe in the object: after a
			// first pass whose enumeration failed there is no baseline, and on a host whose whole
			// triggering subset is empty -- every interface virtual, which is what the device
			// manager sees in a Pod network namespace -- "never enumerated" and "enumerated,
			// found nothing gate-relevant" are the same empty list. Comparing them answers
			// "unchanged" and the loop never re-enters the report path, so `spec.interfaces`
			// stays absent for as long as the accelerator keys hold still.
			name:       "a first successful enumeration is a change even when nothing in it triggers",
			reported:   nil,
			noBaseline: true,
			detected: []workercore.DeviceInterface{
				{Name: "lo", Bus: "virtual", Virtual: true, MTU: 65536, Up: true},
				{Name: "eth0", Bus: "virtual", Virtual: true, MTU: 1500, Up: true},
			},
			want: true,
		},
		{
			// The failure branch outranks the missing baseline: recovery is driven by the first
			// enumeration that SUCCEEDS, so a permanently unreadable sysfs cannot spin the loop.
			name:        "no baseline and a failed enumeration is still not a change",
			reported:    nil,
			noBaseline:  true,
			detected:    nil,
			detectedErr: errors.New("the network interface inventory is only available on linux"),
			want:        false,
		},
		{
			// A failed read establishes nothing, so it must not take the loop round: the report
			// path refuses to publish an empty inventory after one, and re-reporting on it would
			// spin the loop on no evidence.
			name:        "a failed enumeration is not a change",
			reported:    []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil)},
			detected:    nil,
			detectedErr: errors.New("the network interface inventory is only available on linux"),
			want:        false,
		},
		{
			// The other cost-class half, and the one that made a Pod event a hardware event: a CNI
			// veth end appearing under /sys/devices/virtual/net is not a change worth a detect
			// round. Comparing the whole list made every Pod start rerun the manufacturer's driver
			// detection and rewrite the cluster-scoped object. Unlike the timestamp case below,
			// the data really did change -- so nothing in the published record looks wrong, and
			// only the write volume shows it.
			name:     "an ephemeral virtual interface appearing is not a change",
			reported: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil)},
			detected: []workercore.DeviceInterface{
				iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil),
				{Name: "veth1a2b3c", Bus: "virtual", Virtual: true, MTU: 1500, Up: true},
			},
			want: false,
		},
		{
			// And its churn, which is what a busy node actually produces: the same veth with a
			// different MTU is still nothing this comparison is for.
			name: "a virtual interface's own fields changing is not a change",
			reported: []workercore.DeviceInterface{
				iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil),
				{Name: "veth1a2b3c", Bus: "virtual", Virtual: true, MTU: 1500, Up: true},
			},
			detected: []workercore.DeviceInterface{
				iface("eth0", workercore.DeviceInterfaceLinkStateOK, nil),
				{Name: "veth1a2b3c", Bus: "virtual", Virtual: true, MTU: 9000, Up: false},
			},
			want: false,
		},
		{
			// The control that keeps the filter honest: it drops what cannot affect the gate, not
			// everything virtual. An RDMA verdict on a virtual interface still takes the round.
			name: "a virtual interface carrying an RDMA verdict is not exempt",
			reported: []workercore.DeviceInterface{{
				Name: "bond0", Bus: "virtual", Virtual: true, RDMA: true, RDMADevice: "mlx5_bond_0",
				Link: &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK},
			}},
			detected: []workercore.DeviceInterface{{
				Name: "bond0", Bus: "virtual", Virtual: true, RDMA: true, RDMADevice: "mlx5_bond_0",
				Link: &workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateFailed},
			}},
			want: true,
		},
		{
			// The cost-class half: a first-seen time on one side only must NOT read as a change.
			// Nothing about the published record would be wrong if it did — the object would simply
			// be rewritten on every monitor tick for as long as the failure lasted.
			name:     "a first-seen time present on one side only is not a change",
			reported: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateFailed, nil)},
			detected: []workercore.DeviceInterface{iface("eth0", workercore.DeviceInterfaceLinkStateFailed, &stamp)},
			want:     false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := interfacesChanged(
				tc.reported, !tc.noBaseline, tc.detected, tc.detectedErr); got != tc.want {
				t.Errorf("interfacesChanged = %v, want %v", got, tc.want)
			}
		})
	}
}
