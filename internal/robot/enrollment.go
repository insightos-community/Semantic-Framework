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

package robot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"insightos.cn/semantic-framework/internal/store"
)

var ErrPilotCredentialInvalid = errors.New("Pilot credential 无效或已撤销")

func (s *Service) CreatePilotEnrollment(createdBy string) (store.PilotEnrollment, error) {
	if strings.TrimSpace(createdBy) == "" {
		return store.PilotEnrollment{}, errors.New("created_by 必填")
	}
	limit := big.NewInt(1000000)
	value, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return store.PilotEnrollment{}, err
	}
	now := s.now().UTC()
	item := store.PilotEnrollment{ID: "pen-" + uuid.NewString(),
		Code: fmt.Sprintf("%06d", value.Int64()), Status: "pending", CreatedBy: createdBy,
		CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute)}
	return item, s.st.SavePilotEnrollment(item)
}

func (s *Service) ClaimPilotEnrollment(code, pilotID string) (store.PilotEnrollment, string, error) {
	code, pilotID = strings.TrimSpace(code), strings.TrimSpace(pilotID)
	if code == "" || pilotID == "" {
		return store.PilotEnrollment{}, "", errors.New("join_code 和 pilot_id 必填")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return store.PilotEnrollment{}, "", err
	}
	credential := "pilot_" + hex.EncodeToString(secret)
	item, err := s.st.ClaimPilotEnrollment(code, pilotID, credential, s.now().UTC())
	return item, credential, err
}

func (s *Service) RevokePilotEnrollment(id string) error {
	return s.st.RevokePilotEnrollment(strings.TrimSpace(id))
}

// ValidatePilotCredential 返回凭据绑定的 Pilot ID。Gateway 随后还会把它与
// register 消息中的 ID 比较，防止复制另一台设备的 connection.yaml 后冒充。
func (s *Service) ValidatePilotCredential(value string) (string, error) {
	item, err := s.st.GetPilotCredential(strings.TrimSpace(value))
	if err != nil || item.RevokedAt != nil {
		return "", ErrPilotCredentialInvalid
	}
	return item.PilotInstanceID, nil
}

func (s *Service) SetDesiredSkill(robotID, name, version string, enabled bool) (store.RobotDesiredSkill, error) {
	if strings.TrimSpace(robotID) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" {
		return store.RobotDesiredSkill{}, errors.New("robot_id、name 和 version 必填")
	}
	item := store.RobotDesiredSkill{RobotID: robotID, Name: name, Version: version,
		Enabled: enabled, UpdatedAt: s.now().UTC()}
	if err := s.st.SaveRobotDesiredSkill(item); err != nil {
		return item, err
	}
	err := s.ReconcileDesiredSkills(robotID)
	if errors.Is(err, ErrPilotOffline) || errors.Is(err, store.ErrNotFound) {
		return item, nil
	}
	return item, err
}

func (s *Service) RemoveDesiredSkill(robotID, name string) error {
	if err := s.st.DeleteRobotDesiredSkill(robotID, name); err != nil {
		return err
	}
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if err != nil || !s.IsOnline(pilot.PilotInstanceID) {
		return err
	}
	for _, actual := range mustListPilotSkills(s.st, pilot.PilotInstanceID) {
		if actual.Name == name {
			return s.UninstallSkill(pilot.PilotInstanceID, actual.Name, actual.Version)
		}
	}
	return nil
}

func mustListPilotSkills(st *store.Store, pilotID string) []store.RobotPilotSkill {
	items, err := st.ListRobotPilotSkills(pilotID)
	if err != nil {
		return nil
	}
	return items
}

// ReconcileDesiredSkills 只比较 Server 期望与 Pilot 实际目录并下发最小命令。
// 安装过程仍由 Pilot 负责；命令失败会回报 actual=failed，Server 不在同一次
// 连接中无限重试，避免错误配置形成安装循环。
func (s *Service) ReconcileDesiredSkills(robotID string) error {
	pilot, err := s.st.GetActiveRobotPilot(robotID)
	if err != nil {
		return err
	}
	if !s.IsOnline(pilot.PilotInstanceID) {
		return ErrPilotOffline
	}
	desired, err := s.st.ListRobotDesiredSkills(robotID)
	if err != nil {
		return err
	}
	actualItems, err := s.st.ListRobotPilotSkills(pilot.PilotInstanceID)
	if err != nil {
		return err
	}
	actual := make(map[string]store.RobotPilotSkill, len(actualItems))
	for _, item := range actualItems {
		actual[item.Name+"\x00"+item.Version] = item
	}
	for _, target := range desired {
		key := target.Name + "\x00" + target.Version
		item, exists := actual[key]
		// uninstalled 是 Pilot 已确认本地包不存在的稳定状态。设备页再次把
		// desired 改为启用时必须重新发送 install；若把所有非 installed
		// 状态都跳过，HTTP 会返回 accepted，但 desired/actual 永远无法
		// 收敛。failed 则保留错误、等待用户明确重试，避免损坏的包形成
		// 自动安装循环。
		if !exists || item.Status == "uninstalled" {
			if err := s.InstallSkill(pilot.PilotInstanceID, target.Name, target.Version); err != nil {
				return err
			}
			continue
		}
		if item.Status != "installed" {
			continue
		}
		if item.Enabled != target.Enabled {
			if err := s.SetSkillEnabled(pilot.PilotInstanceID, target.Name, target.Version, target.Enabled); err != nil {
				return err
			}
		}
	}
	return nil
}
