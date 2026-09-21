# RDMA cases: which host shape answers which reading

> **Purpose** — the RDMA cases (80–84) are gated on the SHAPE of the machine they run on, not on its
> manufacturer. This page says what each shape can answer, what it structurally cannot, and which
> reading no machine in reach answers at all. Read it before deciding a run is worth the time, and
> read it again when a case skips.
>
> [What a machine must carry, as readings](#what-a-machine-must-carry-as-readings) and
> [Three machines to look for](#three-machines-to-look-for) are the tables to use when borrowing or
> buying hardware: every property is a command and the answer it must give, so it stays true across
> a hardware refresh in a way a part number does not.

Run `cases/run-rdma-block.sh --report-only [NS]` against a cluster to get the same matrix computed
from that cluster's own ledger. It touches nothing.

## The rule these groups come from

A reading's host requirement is **whatever that reading reads**. Deriving it from the feature's
subject instead is how a whole fleet gets ruled out for a property no reading depends on: most of
these do not look at the accelerator's manufacturer at all — they read the RDMA side's shape and
counts, or the kubelet's own configuration. The two that genuinely need a particular kind of
hardware need a *partitionable accelerator* and *classic InfiniBand*, and neither is a manufacturer
question either.

So the groups below are **capabilities**, and a machine belongs to as many of them as it satisfies.
Grouping by vendor would put two machines with the same answer in different rows and two machines
with different answers in the same one.

## The five capability groups

### A. Any node running a device manager, with at least one RDMA endpoint

Answers the inventory correspondence and the per-mode counts: that every RDMA device the host's own
subsystem lists is in the published record, and that the node's allocatable counts are the endpoint
counts times each mode's token size, with endpoints whose link verdict is `failed` excluded.

**Structurally cannot answer**: anything about the virtual-function branch, anything about NUMA
alignment, and anything about what InfiniBand needs beyond verbs.

Case 80, and case 81's grant/refusal pair.

### B. Group A, plus SR-IOV virtual functions configured

Answers the other branch of the mode judgment: the partitioned key counts the virtual functions, and
the physical function carrying them counts **zero times** in the whole-function keys.

**Why the gate is hard**: a host with no virtual functions reports the partitioned key at zero for a
reason that has nothing to do with how the count is derived. Zero equals zero, and the check passes
having measured nothing. The gate reads `sriov_numvfs` from the kernel — not from the ledger, which
is the thing under test.

**Two ways a host fails this, and they call for opposite actions.** The case reports which:

| reading of `/sys/class/infiniband/<dev>/device/sriov_numvfs` | what it means | what to do |
|---|---|---|
| a number above zero | virtual functions are configured | this host answers the reading |
| `0` | the capability is present, unconfigured | configure virtual functions on **this** machine |
| the file does not exist | the capability is **not present in this guest** | find another machine |

The third was measured: an adapter passed through to a guest with SR-IOV already consumed on the
other side exposes neither `sriov_numvfs` nor `sriov_totalvfs`, and no command run inside makes it
appear. Folding "absent" into "zero" would send a reader to run a configuration command that cannot
work on that machine.

Case 80's SR-IOV checks; they skip individually on a group-A host, naming which of the two it is.

### C. Accelerators straddling the RDMA endpoints' NUMA nodes, under an enforcing kubelet

Three properties at once, and all three are required:

1. accelerators on **at least two** NUMA nodes;
2. **at least one** accelerator sharing a NUMA node with a usable endpoint, and **at least one** not;
3. a kubelet reporting `single-numa-node` or `restricted` from its own configuration endpoint.

Answers the feature's headline promise in both directions: a request that fits on one NUMA node runs
with the accelerator and the endpoint agreeing, and a request for one accelerator more is refused by
the kubelet with a topology affinity error.

**Why each half of the gate exists**:

- without (2)'s second clause every placement is aligned whatever the kubelet does, so the admitted
  half passes with the alignment mechanism switched off — which is the reading the criterion
  explicitly excludes, and the failure mode a single-socket host produces;
- without (3) the hint is computed and discarded (`none`) or a misaligned container is admitted
  anyway (`best-effort`), so the refusal can never fire and the agreement is a coincidence of
  placement. The policy is read from the kubelet's live endpoint, never from a drop-in file: a
  policy written where a reader does not look produces the same answer as a policy nobody set.

> **"Devices on at least two NUMA nodes" is not enough, and this is the trap worth naming.** The
> requirement is an ACCELERATOR THAT FITS NOWHERE — one whose NUMA node carries no endpoint. A
> machine can spread its devices over two NUMA nodes and still fail it, by spreading them
> *symmetrically*: measured on a host with four accelerators and four adapters on each of two NUMA
> nodes, every accelerator shares a node with some endpoint, so "one accelerator plus one endpoint"
> is satisfiable on either side and **the refusal can never fire**. That host answers nothing here
> even with the policy set correctly, and the policy being the visible knob is what makes the
> topology condition easy to miss. The other host measured — eight accelerators over eight NUMA
> nodes with both adapters on one of them — has only two accelerators beside an endpoint, so the
> boundary sits between asking for two and asking for three, and that boundary IS the reading.
> The `numa-split` requirement's second clause is this condition, and it is what made case 82 skip
> on the symmetric host rather than report a pass.

The boundary is computed from the node's own ledger — the largest number of accelerators any single
endpoint-bearing NUMA node carries — so the two requests differ by exactly one accelerator. A pair
of requests far apart would be refused for a reason the case cannot separate from the one it means.

Case 82.

### D. Group C, plus an accelerator already in a hardware partitioning mode

**No machine this suite has been run on satisfies this**, and it is now the only reading in the set
that none does. See [The one reading no machine answers](#the-one-reading-no-machine-answers).

### E. An endpoint whose port link layer reads `InfiniBand`

Answers whether the injected set carries a real transport rather than only the verbs layer.

**Why the gate is hard**: a RoCE or EFA adapter runs verbs over Ethernet and opens the same
character device, so the whole question — what InfiniBand *additionally* needs — is not being asked.
A pass taken there is the one outcome that would retire the question wrongly. The link layer is read
from the kernel's own port attribute in a probe Pod, never from the ledger.

Case 84. It also needs an image shipping `ib_write_bw`; `ibv_devinfo` succeeding is explicitly not
an answer, because it exercises verbs and says nothing about the rest of the set.

### F. A node running a device manager whose kubelet is on an EXPLICITLY SET policy

Answers whether preflight reports the policy the kubelet is running. Needs no RDMA hardware at all —
but it does need the policy to have been **set by somebody**, and that is the part that is easy to
get wrong.

**Why the instrument matters more than the host here**: judging preflight's answer against the same
configuration files preflight reads asks one source twice. The kubelet's own endpoint is the only
reading that separates "the policy is set somewhere nothing reads" from "the reader looked in the
wrong place" — and the second is what this reading has already caught once, on a node configured
exactly the way its distribution documents.

**What that instrument cannot say.** The endpoint reports the *effective* configuration, defaults
included, so it always names a policy — `none` on a node where nobody ever set one. Preflight
deliberately does not publish a default nobody wrote. So "preflight equals the endpoint" is **not**
the criterion: it fails every node with an unset policy, which is preflight behaving correctly, and
it was measured doing exactly that on a managed node.

The criterion is the one-way inference the endpoint does support: the kubelet's default is `none`,
so an effective value that is **not** `none` came from a configuration source, and preflight is
required to have found it. Where the endpoint says `none`, "nobody set it" and "somebody set it to
`none`" are indistinguishable from outside, so preflight's `unknown` there is recorded rather than
judged — and the one direction still decidable is asserted: preflight must never name a policy the
kubelet is not running, and a policy it does name must carry a depth.

So a node carrying **group C**'s third property — a kubelet on `single-numa-node` or `restricted` —
is also the node that answers this one, and the `topology-enforced` requirement in the capability
matrix is the proxy for "somebody set it".

Case 83. Where the endpoint is unreadable the case skips rather than accepting preflight's own word.
Only the note's *presence* is asserted, never its wording: which sources it names is a property of
the reader and has already moved once, so a case pinning today's phrasing would go red the day the
reader is corrected.

## What a machine must carry, as readings

Every property below is stated as **a command and the answer it must give**, never as a part number.
A part number is not a durable fact and does not survive a hardware refresh; the attribute does, and
the cases' own gates are written against the attribute. This table is the one to hand to whoever is
borrowing or buying a machine.

| property | how to read it | the answer that qualifies |
|---|---|---|
| P1 an RDMA endpoint exists | `ls /sys/class/infiniband/` | at least one entry |
| P2 classic InfiniBand, not RoCE or EFA | `cat /sys/class/infiniband/<dev>/ports/1/link_layer` | `InfiniBand` (`Ethernet` is RoCE and does not qualify) |
| P3 SR-IOV is present and configured | `cat /sys/class/infiniband/<dev>/device/sriov_numvfs` | a number above zero. `0` means configurable here; **no such file** means the capability is not in this guest at all |
| P4 the kubelet enforces alignment | `kubectl get --raw /api/v1/nodes/<n>/proxy/configz \| jq -r .kubeletconfig.topologyManagerPolicy` | `single-numa-node` or `restricted`. This also needs the kubelet to be **ours to configure**, which a managed node is not |
| P5 the NUMA layout is ASYMMETRIC | `cat /sys/class/infiniband/<dev>/device/numa_node` against each accelerator's `/sys/bus/pci/devices/<pci>/numa_node` | at least one accelerator whose NUMA node carries **no** endpoint, and at least one that shares one |
| P6 an accelerator can be hardware-partitioned | the vendor's own mode query, and after enabling it `Devices.spec.groups[].accelerators[].status.physicalSliced.profiles` | a non-empty profile list |

**P5 is the one that gets missed.** It is a topology property, not a configuration one, so it cannot
be fixed on a machine that lacks it and it is invisible from the knob everyone checks (P4). A
machine with devices on two NUMA nodes still fails it when the layout is symmetric — see the note
under group C.

## The one reading no machine answers

**The partition-beside-an-endpoint row.** It needs **P4 and P5 and P6 at once**, and the two
machines measured so far hold complementary halves:

| | eight accelerators over eight NUMA nodes, two adapters on one of them | four accelerators and four adapters on each of two NUMA nodes |
|---|---|---|
| P4 kubelet enforces alignment | yes | **no** — default, and managed, so not ours to change |
| P5 asymmetric NUMA layout | yes | **no** — symmetric, so the refusal can never fire |
| P6 accelerator can be partitioned | **no** | yes |

Neither can answer it, and combining them does not: the row needs one machine holding all three.
With the transport row now answered, **this is the only reading in the set that no machine in reach
can take.**

Case 82 records it as a skip on every run, naming the three properties. It is deliberately **not**
written as a pass-when-observed: a partition token carries no NUMA hint at all, so the kubelet's
merge sees one hint — ours — and admits the container aligned to the RDMA side alone. Nothing fails
and no refusal can ever fire, which means a run that happens to land the partition and the endpoint
on one NUMA node proves nothing. The row exists to make the limit stop being theoretical.

## Three machines to look for

Each row is a shopping list of the properties above. One machine may satisfy several rows.

| to answer | the machine must carry | notes |
|---|---|---|
| the alignment row **and** the partition row | P1 + P4 + P5 + P6 | not a managed node, since P4 needs the kubelet to be configurable. Getting P5 means checking the NUMA layout before committing: an adapter-per-accelerator machine is very likely symmetric and therefore useless for this, however many NUMA nodes it has |
| the virtual-function row | P1 + P3 | bare metal, or a guest whose adapter still exposes SR-IOV — the capability must not have been consumed on the hypervisor side, which a passed-through adapter usually means it was |
| the transport row | P1 + P2 | **already answered** by the second machine measured. Kept here to record what it needed: the link layer, read from the port attribute, not a card model |

The inventory, count and grant rows need only P1 and are answered by any machine with an endpoint.
The preflight-policy row needs P4 alone and no RDMA hardware.

## Machine shapes measured so far

Described by shape and role. A machine's identity is not a durable fact and does not belong in a
repository; its shape is what another machine has to match.

| shape | properties | groups | what it could not answer, and why |
|---|---|---|---|
| aarch64, 8 accelerators over 8 NUMA nodes, 2 RoCE adapters both attached to one of them, 4 SR-IOV virtual functions configured, kubelet on `single-numa-node` | P1 P3 P4 P5 | A, B, C, F | the partition observation — its accelerators have no partitioning mode (no P6); the transport reading — RoCE, not classic InfiniBand (no P2) |
| x86_64, 8 accelerators over 2 NUMA nodes, 8 InfiniBand adapters one per accelerator, partitionable cards, kubelet fixed on `none` and managed | P1 P2 P6 | A, E | the alignment readings — **two independent reasons**, either alone sufficient: the kubelet discards the hint and is not ours to change (no P4), and the NUMA layout is symmetric so every accelerator sits beside an endpoint and the refusal can never fire (no P5); the virtual-function counts — `sriov_numvfs` does not exist, the adapter being passed through with SR-IOV consumed on the other side (no P3, and not fixable here); the preflight-policy reading — its kubelet is on the default, where `unknown` is both the right answer and what a reader that can read nothing produces |

Between them they answer every reading but the partition one, and that one needs a **third** shape
rather than a combination of these two: the first lacks P6 and the second lacks P4 and P5.

The second machine is worth reading twice, because it looks like it should answer the alignment row
and does not. It has two NUMA nodes, multiple accelerators and an adapter per accelerator — every
visible sign of a machine that can. What disqualifies it is that the devices are laid out
*symmetrically*, which is neither a setting nor something a knob exposes.

The second shape's refusal shape is worth recording, because it is the common one and an earlier
revision of the grant case did not recognise it: there, an ungranted container has **no
`/dev/infiniband` directory at all**, so the refusal happens at the mount namespace rather than at
the device cgroup. The first shape refuses at the cgroup, with the node present and its mode bits
reading `crw-rw-rw-`. Both are complete refusals of the endpoint and only the first exercises the
rule an allocation adds, so the grant case accepts either and records which.

## Environment these cases read

| variable | what it is for | without it |
|---|---|---|
| `RDMA_NODE_NAME` | pin the node instead of taking the first that qualifies | the first qualifying node is used |
| `RDMA_TEST_NS` | where the test and probe Pods go (default `default`) | `default`; a `restricted` PodSecurity level there makes the host probe skip |
| `E2E_RDMA_PROBE_IMAGE` | the probe and workload image; needs a shell and nothing else | a small busybox tag |
| `E2E_RDMA_WORKLOAD_IMAGE` | the image the grant and alignment Pods run | the probe image |
| `E2E_RDMA_IMAGE` | an image shipping `ibv_devinfo` | the verbs check skips; the open() checks still run, because a shell's own redirection is an open |
| `E2E_RDMA_PERFTEST_IMAGE` | an image shipping `ib_write_bw` | the transport case skips |

## What the two RDMA-userspace images have to carry

Naming an image is not the same as that image carrying the tool, so both cases **probe the running
container** and skip naming the executable rather than recording its `command not found` as a
failure. An absent tool is a fact about the image; it is not a reading of the operator, and it must
not reach a verdict.

| variable | executable | Debian-family packages |
|---|---|---|
| `E2E_RDMA_IMAGE` | `ibv_devinfo` | `ibverbs-utils`, `rdma-core`, `ibverbs-providers` |
| `E2E_RDMA_PERFTEST_IMAGE` | `ib_write_bw` | `perftest`, `rdma-core`, `libibverbs`, `ibverbs-providers` |

A plain base image carries none of these. Installing them inside the case was considered and not
done: it needs the container to reach a package mirror, which makes a run's outcome depend on the
cluster's egress, and a failure there would arrive as a failure of the reading.

**`ibverbs-providers` is not optional, and leaving it out fails silently.** Measured on real
hardware: an image with no provider library makes a collective communication library **fall back to
TCP and complete successfully**, over Ethernet, with no error anywhere. The rule that follows is
about any future version of the transport case — a reading built on such a library must assert
**which transport was selected**, never that the job succeeded, because success is exactly what both
outcomes have in common. The transport case uses `perftest` today for that reason: it has no
fallback, so its completing *is* the reading.
| `E2E_RDMA_SHARED_POOL_SIZE` | the shared family's token count per endpoint | the shipped ceiling; a mismatch is a finding, not a configuration difference |
| `E2E_RDMA_POD_TIMEOUT` | how long a Pod gets to reach its phase | 180s |

## Where these readings come from

The readings these cases implement are the verification table of
`specs/2026-09-20-rdma-extended-resource.md`, rows `R0`–`R7`, together with the `Readings` section
recording what each one read on real hardware. This is the **only** place those labels appear: a
case names the behaviour it asserts and carries the criterion in its own words, because a label
needs a second document to mean anything and outlives the document that defined it.

| reading | case | the check's own name |
|---|---|---|
| `R0` | 80 | the published inventory names the host's RDMA devices |
| `R1` | 80 | the whole-function / shared / partitioned key counts |
| `R2` | 81 | the granted device opens …; the same Pod without the request is refused the same device |
| `R3` | 82 | a request that fits on one NUMA node is admitted; one accelerator more is refused for topology |
| `R3b` | 82 | a hardware partition beside an endpoint is aligned by nothing (recorded, never passed) |
| `R4` | 80 | a failed link keeps its tokens in capacity and loses them from allocatable |
| `R5` | 80 | the partitioned key counts the host's configured virtual functions; a physical function counts zero times |
| `R6` | 83 | preflight names the policy the kubelet is running |
| `R7` | 84 | the transport installs and runs from the injected set alone |

One drift is worth stating because the table and the code describe the same thing at different
times: the verification table names **four** keys, including a `.sliced` one. That key was retired —
two keys over one pool set no ceiling at all, since one endpoint then admitted a full complement of
holders under each name — and the shared key was moved to RDMA's own ceiling rather than the
accelerator sharing constant it had borrowed. Case 80 asserts the three served keys and asserts that
the retired one is absent or zero: an extended resource that has entered a node's status is not
removed when the plugin stops serving it, so a node that once ran the older build keeps it at zero
forever, and only a non-zero value is a finding.
