// Copyright 2026 InsightOS
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package llm

import "testing"

// TestEstimateCost 验证按每 1K tokens 单价估算成本。
func TestEstimateCost(t *testing.T) {
	price := Price{Prompt: 0.001, Completion: 0.002}
	usage := Usage{PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500}

	got := EstimateCost(price, usage)
	// 1000/1000*0.001 + 500/1000*0.002 = 0.001 + 0.001 = 0.002
	want := 0.002
	if got < want-1e-12 || got > want+1e-12 {
		t.Errorf("成本估算应为 %v，实际: %v", want, got)
	}
}

// TestEstimateCostZero 验证零用量与零单价时成本为零。
func TestEstimateCostZero(t *testing.T) {
	if got := EstimateCost(Price{}, Usage{PromptTokens: 100, CompletionTokens: 100, TotalTokens: 200}); got != 0 {
		t.Errorf("零单价成本应为 0，实际: %v", got)
	}
	if got := EstimateCost(Price{Prompt: 1, Completion: 1}, Usage{}); got != 0 {
		t.Errorf("零用量成本应为 0，实际: %v", got)
	}
}
