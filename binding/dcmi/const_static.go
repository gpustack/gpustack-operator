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
