// Copyright 2026 GPUStack Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package elasticprofile

import "testing"

// TestProfileWidthBounds pins the one declaration of the supported profile. Every consumer
// reads these two constants rather than a copy, so this is the single place where widening or
// narrowing the range is a deliberate edit, and the consumer contract tests fail whenever a
// consumer stops agreeing with it.
func TestProfileWidthBounds(t *testing.T) {
	if WidthMin != 2 {
		t.Fatalf("the narrowest elastic collective is 2, the profile declares %d", WidthMin)
	}
	if WidthMax != 64 {
		t.Fatalf("the widest elastic collective is 64, the profile declares %d", WidthMax)
	}
	if WidthMin >= WidthMax {
		t.Fatalf("the profile declares an empty range [%d, %d]", WidthMin, WidthMax)
	}
}
