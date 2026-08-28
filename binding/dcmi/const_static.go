// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dcmi

// ERROR_LIST_TRUNCATED reports that a list read could not be completed because the device held
// more entries than the read accepts.
//
// It is deliberately outside the driver's own -80xx code space: the driver never returns it, and a
// log line carrying it must not read as something the driver said. It exists because one of the
// library's list calls takes its count as an output only — it cannot be told how large the caller's
// buffer is — so "the list was longer than we can accept" is a condition only this side can detect
// and therefore only this side can name.
const ERROR_LIST_TRUNCATED Return = -9001

// DDIE is the die type whose id serves as the chip's uuid on the A5 generation.
//
// It is declared here rather than coming from the generated const.go because the public V2 header
// does not enumerate it: that header declares no type of its own, and the die-type enum it reuses is
// the V1 one, which stops at VDIE. The vendor's own binding goes further — its constants declare
// `DDIE DieType = 2` as "DDie ID, it can be the uuid of A5 chip", and the set of die types it accepts
// for the V2 die query is {VDIE, NDIE, DDIE}.
//
// Only the V2 die query is asked for it. A V1 driver is never sent this type.
const DDIE DieType = 2
