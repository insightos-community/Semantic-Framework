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

package kernel

import (
	"context"

	"insightos.cn/semantic-framework/internal/store"
)

// checkPointStore 是 adk CheckPointStore 接口的实现：断点字节流后端
// internal/store，随 run_sessions 存（checkpoint 列）。
// 为什么断点随 run 存而不是独立表：断点的生命周期与 run 严格一致
// （run 结束断点即失去意义），随行存取避免孤儿数据与额外的清理逻辑。
type checkPointStore struct {
	// st 元数据存储。
	st *store.Store
}

// newCheckPointStore 创建断点存储。
func newCheckPointStore(st *store.Store) *checkPointStore {
	return &checkPointStore{st: st}
}

// Get 按断点 ID（= run ID）读取断点；不存在时第二个返回值为 false。
func (s *checkPointStore) Get(_ context.Context, checkPointID string) ([]byte, bool, error) {
	return s.st.GetRunCheckpoint(checkPointID)
}

// Set 写入断点（覆盖同一 run 的旧断点：一次 run 可能多次中断，
// 只保留最新断点，恢复永远从最新暂停点继续）。
func (s *checkPointStore) Set(_ context.Context, checkPointID string, checkPoint []byte) error {
	return s.st.SaveRunCheckpoint(checkPointID, checkPoint)
}
