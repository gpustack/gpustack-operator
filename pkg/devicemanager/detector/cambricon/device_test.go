// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package cambricon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"gpustack.ai/gpustack/binding/cndev"
)

func TestEvaluateCardUnhealthy(t *testing.T) {
	testCases := []struct {
		name  string
		v2    cndev.CardHealthStateV2
		v2Ret cndev.Return
		v1    cndev.CardHealthStateV2
		v1Ret cndev.Return
		want  bool
	}{
		{
			name:  "V2 healthy card",
			v2:    cndev.CardHealthStateV2{Health: 1},
			v2Ret: cndev.SUCCESS,
			want:  false,
		},
		{
			name:  "V2 unhealthy card",
			v2:    cndev.CardHealthStateV2{Health: 0},
			v2Ret: cndev.SUCCESS,
			want:  true,
		},
		{
			name:  "V2 unsupported falls back to V1 healthy",
			v2Ret: cndev.ERROR_NOT_SUPPORTED,
			v1:    cndev.CardHealthStateV2{Health: 1},
			v1Ret: cndev.SUCCESS,
			want:  false,
		},
		{
			name:  "V2 unsupported falls back to V1 unhealthy",
			v2Ret: cndev.ERROR_NOT_SUPPORTED,
			v1:    cndev.CardHealthStateV2{Health: 0},
			v1Ret: cndev.SUCCESS,
			want:  true,
		},
		{
			name:  "both V2 and V1 fail defaults to healthy",
			v2Ret: cndev.ERROR_NOT_SUPPORTED,
			v1Ret: cndev.ERROR_NOT_SUPPORTED,
			want:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateCardUnhealthy(
				func() (cndev.CardHealthStateV2, cndev.Return) { return tc.v2, tc.v2Ret },
				func() (cndev.CardHealthStateV2, cndev.Return) { return tc.v1, tc.v1Ret },
			)
			assert.Equal(t, tc.want, got)
		})
	}
}
