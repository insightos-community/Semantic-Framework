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

package store

import (
	"errors"
	"testing"
)

// TestSettingsKeyCRUD 验证托管密钥的写入（含覆盖）、读取、删除与清单：
// 不存在场景统一返回 ErrNotFound，清单按名称升序且带更新时间。
func TestSettingsKeyCRUD(t *testing.T) {
	st := openTestStore(t)

	// 未写入时读取/删除均为 ErrNotFound。
	if _, err := st.GetKey("deepseek-chat"); !errors.Is(err, ErrNotFound) {
		t.Errorf("读取不存在的密钥应返回 ErrNotFound，实际: %v", err)
	}
	if err := st.DeleteKey("deepseek-chat"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除不存在的密钥应返回 ErrNotFound，实际: %v", err)
	}

	// 写入两条，清单按名称升序。
	if err := st.SetKey("deepseek-chat", "sk-chat-0001"); err != nil {
		t.Fatalf("SetKey 失败: %v", err)
	}
	if err := st.SetKey("deepseek-reasoner", "sk-reasoner-0001"); err != nil {
		t.Fatalf("SetKey 失败: %v", err)
	}

	value, err := st.GetKey("deepseek-chat")
	if err != nil {
		t.Fatalf("GetKey 不应失败: %v", err)
	}
	if value != "sk-chat-0001" {
		t.Errorf("密钥值不符: %q", value)
	}

	keys, err := st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys 不应失败: %v", err)
	}
	if len(keys) != 2 || keys[0].Name != "deepseek-chat" || keys[1].Name != "deepseek-reasoner" {
		t.Fatalf("清单应按名称升序且包含两条记录，实际: %+v", keys)
	}
	if keys[0].Value != "sk-chat-0001" || keys[0].UpdatedAt.IsZero() {
		t.Errorf("清单记录应含值与非零更新时间: %+v", keys[0])
	}

	// 覆盖同名密钥：值更新且 updated_at 不早于首次写入。
	first := keys[0].UpdatedAt
	if err := st.SetKey("deepseek-chat", "sk-chat-0002"); err != nil {
		t.Fatalf("覆盖 SetKey 失败: %v", err)
	}
	keys, err = st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys 不应失败: %v", err)
	}
	if len(keys) != 2 || keys[0].Value != "sk-chat-0002" {
		t.Fatalf("覆盖后清单值不符: %+v", keys)
	}
	if keys[0].UpdatedAt.Before(first) {
		t.Errorf("覆盖后 updated_at 不应回退: %v < %v", keys[0].UpdatedAt, first)
	}

	// 删除后读取回到 ErrNotFound，清单剩一条。
	if err := st.DeleteKey("deepseek-chat"); err != nil {
		t.Fatalf("DeleteKey 失败: %v", err)
	}
	if _, err := st.GetKey("deepseek-chat"); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后读取应返回 ErrNotFound，实际: %v", err)
	}
	keys, err = st.ListKeys()
	if err != nil {
		t.Fatalf("ListKeys 不应失败: %v", err)
	}
	if len(keys) != 1 || keys[0].Name != "deepseek-reasoner" {
		t.Errorf("删除后清单应剩 deepseek-reasoner，实际: %+v", keys)
	}
}

// TestSettingsAudit 验证审计记录的写入与按 id 倒序、限量查询。
func TestSettingsAudit(t *testing.T) {
	st := openTestStore(t)

	for _, e := range []AuditEntry{
		{UserID: "usr-1", Action: "settings.key_set", Detail: "deepseek-chat"},
		{UserID: "usr-1", Action: "settings.patch", Detail: "llm.default"},
		{UserID: "usr-2", Action: "settings.key_delete", Detail: "deepseek-chat"},
	} {
		if err := st.InsertAudit(e); err != nil {
			t.Fatalf("InsertAudit 失败: %v", err)
		}
	}

	entries, err := st.ListAudit(10)
	if err != nil {
		t.Fatalf("ListAudit 不应失败: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("应返回 3 条审计，实际: %d", len(entries))
	}
	// 倒序：最新写入的（key_delete）在最前，且字段完整。
	if entries[0].Action != "settings.key_delete" || entries[0].UserID != "usr-2" {
		t.Errorf("最新审计应在前，实际: %+v", entries[0])
	}
	if entries[2].Action != "settings.key_set" || entries[2].Detail != "deepseek-chat" {
		t.Errorf("最早审计应在末位，实际: %+v", entries[2])
	}
	for i, e := range entries {
		if e.ID == 0 || e.CreatedAt.IsZero() {
			t.Errorf("审计 #%d 应有自增 ID 与非零时间: %+v", i, e)
		}
	}

	// limit 截断：只取最新 1 条。
	entries, err = st.ListAudit(1)
	if err != nil {
		t.Fatalf("ListAudit(1) 不应失败: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "settings.key_delete" {
		t.Errorf("limit=1 应只返回最新一条，实际: %+v", entries)
	}
}
