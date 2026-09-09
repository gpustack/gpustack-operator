# THead usage region — the layout, as a contract

The file the slicing shim keeps a container's accounting in, and the only place a slice's **compute
limit** can be read at all: `ppu-smi` has no maximum-SM column, exactly as `nvidia-smi` has none.
Written by `csrc/thead/ppu-slicing-shim/common/vppu_ledger.c`, read by
`csrc/thead/ppu-slicing-shim/tools/ppu_monitor.c` — and by whatever scrapes it next, which is why the
layout is written down here rather than left as an internal detail of a C struct.

- Path: `HGGC_LEDGER_PATH`, default `/dev/shm/vppu-ledger`. Empty is the same as unset.
- Created by the first process in the container that is admitted an allocation, mode `0666` (every
  process in the container must be able to map it, and they need not share a user).
- A file that is **not** a region is neither overwritten nor **resized**: the writer judges an existing file's
  magic before it grows anything, so a mistyped `HGGC_LEDGER_PATH` leaves its target byte-for-byte alone.
- Byte order: **the host's**. Writer and readers are processes in one container on one machine.
- Size at layout version 1: **36960 bytes**. A shorter file is not this layout.

## What a reader must do

1. **Check the magic** — `VPPUREGN`, 8 bytes at offset 0, no terminator (so `strings` on the file
   identifies it). Absent or different: not a usage region, do not parse it.
2. **Check the layout version** — `u32` at offset 8, currently `1`. A version you do not know must be
   **refused, not guessed at**: that is what keeps the next field added from silently misparsing in an
   older reader, and it is the whole reason the field exists.
3. **Check the shape** — `header_bytes` (offset 12) is where `devices[]` starts, `device_slots`
   (16) and `process_slots` (20) size the two tables. At version 1 they are `96`, `64` and `32`. A
   writer that disagrees is not writing what you are reading; refuse.
4. **Take no lock.** The write side takes an `fcntl` record lock per card; the read side deliberately
   does not. A figure that is one allocation stale is worth far more than a monitor that can wedge
   behind a vendor allocation that hung.
5. **Map it read-only**, or `pread` the fields you want. Never write: the region is the container's
   accounting, and a reader that wrote to it would be handing out quota.

`tools/ppu-monitor` does all five and exits `0` when it parsed the region, `1` when there is no
region to read (nothing in the container has been sliced yet), `2` when the file exists and fails
step 1, 2 or 3. A scraper needs that distinction: an unsliced container is not a broken ledger.

## Header

| Offset | Bytes | Field | Notes |
| --- | --- | --- | --- |
| 0 | 8 | magic `VPPUREGN` | no terminator |
| 8 | 4 | layout version | `1`; unknown → refuse |
| 12 | 4 | `header_bytes` | `96` — where `devices[]` starts, so a newer header stays skippable |
| 16 | 4 | device slots | `64` |
| 20 | 4 | process slots per card | `32` |
| 24 | 8 | reserved | zero |
| 32 | 64 | lock arena | one byte per card, locked **by offset**, never read as data |
| 96 | 576 × 64 | `devices[]` | card `N` at `96 + 576 * N` |

**The lock arena's position is frozen at version 1 and may never move.** Two processes running
different layout versions must still take the same byte for the same card; if they lock different
offsets they exclude nobody, and the check-then-allocate race the lock exists to close reopens
silently. The same argument freezes the device count at 64: it sizes the arena.

## One card, at `96 + 576 * N`

| Offset | Bytes | Field | Notes |
| --- | --- | --- | --- |
| +0 | 8 | memory quota, bytes | **this card's** figure in force — `HGGC_DEVICE_MEMORY_LIMIT_<N>`, or the un-indexed `HGGC_DEVICE_MEMORY_LIMIT` where the card carries none; re-read from the environment on every admission, not frozen at creation |
| +8 | 8 | memory accounted, bytes | what this container is charged for on this card; the sum of the live entries below |
| +16 | 4 | compute limit, percent | **this card's** cap — `HGGC_DEVICE_SM_LIMIT_<N>`, or the un-indexed `HGGC_DEVICE_SM_LIMIT` where the card carries none; the figure no `ppu-smi` field carries |
| +20 | 4 | compute utilisation, percent | what the controller last measured for **this container** |
| +24 | 4 | lock holder pid | `0` when free; names the process a hung allocation is inside |
| +28 | 4 | reserved | zero |
| +32 | 8 | controller: window start, ns | `CLOCK_MONOTONIC`, when the current window opened |
| +40 | 8 | controller: allow, ns | how much of the window admits launches; `0` = never stepped |
| +48 | 8 | controller: last step, ns | `CLOCK_MONOTONIC`, when the loop last stepped |
| +56 | 4 | controller: integral | signed, clamped against windup |
| +60 | 4 | controller: last error | signed |
| +64 | 512 | `processes[32]` | 16 bytes each: pid `i32`, reserved `u32`, bytes `u64` |

**The window is not in the region.** `allow_ns` is a fraction of the controller's period, and the
period is the container's own configuration (`HGGC_SM_CONTROL_PERIOD_MS`, default 100 ms) — so
`allow / period` is the throttle as it stands, but only a reader that can see that environment may
compute it. `ppu-monitor` prints `allow_us` raw for exactly that reason.

**A card the container holds but has never allocated on has no entry.** The region records a card the
first time an admission touches it, so an untouched card is indistinguishable here from a card the
container does not hold. Quota `0` with a non-zero charge cannot happen — both are written under the
card's lock.

## Reading it by hand

```bash
head -c 8 /dev/shm/vppu-ledger                        # VPPUREGN, or it is not ours
od -A d -t u4 -j 8   -N 16 /dev/shm/vppu-ledger       # version, header_bytes, slot counts
Q=$((96 + 576 * 0))                                   # card 0
od -A n -t u8 -j "${Q}"          -N 16 /dev/shm/vppu-ledger   # quota, accounted (bytes)
od -A n -t u4 -j $((Q + 16))     -N 12 /dev/shm/vppu-ledger   # sm limit, sm util, lock holder
od -A n -t u8 -j $((Q + 32))     -N 24 /dev/shm/vppu-ledger   # window start, allow, last step (ns)
od -A n -t d4 -j $((Q + 56))     -N 8  /dev/shm/vppu-ledger   # integral, last error (signed)
od -A n -t d4 -j $((Q + 64))     -N 8  /dev/shm/vppu-ledger   # first charged pid
```

`tools/ppu-monitor` prints the same figures per card, key=value, with the unit in the key:

```
region path=/dev/shm/vppu-ledger version=1 cards=64 procs=32
card=0 mem_quota_mib=4096 mem_used_mib=1024 mem_free_mib=3072 sm_limit_pct=25 sm_util_pct=7 allow_us=18460 lock_pid=0 mem_quota_bytes=4294967296 mem_used_bytes=1073741824
  proc pid=4242 mem_mib=1024 mem_bytes=1073741824
```

## Where this is checked

- `cases/thead-case-1.sh` writes a region **from the table above** with `dd` — never from a header in
  the tree — and requires `ppu-monitor` to read the same figures back, including the `576`-byte
  stride and the process slot at `+64`. It also requires the reader to refuse a bumped layout
  version, and to report an absent region as its own outcome rather than as a corrupt one.
- `cases/thead-case-6.sh` Part B holds a whole quota and then reads it three ways — the shim's own
  `DENIED` line, `ppu-monitor`, and `od` at these offsets — and requires all three to agree. Its Part C is where
  the `96 + 576 * N` stride meets a real region rather than a written one: one container holds two cards with two
  different quotas, and the reader has to show each card at its own figure.
- `common/vppu_test.c` asserts the offsets from the writer's side, and that a region stamped with an
  unknown version is refused rather than misparsed. `common/vppu_ledger.h` carries the same layout as
  `_Static_assert`s, so a field that moves fails the build.

`csrc/thead/ppu-slicing-shim/README.md` carries the same tables for someone working in the shim tree;
this page is the version a reader outside that tree should write against.
