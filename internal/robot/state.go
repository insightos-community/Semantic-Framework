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
	"context"
	"time"
)

// StateObservation is the small read-only part of an existing Runtime
// observation needed by conversation queries. It is not an execution command
// or a substitute for the Skill's independent tool-load verification.
type StateObservation struct {
	RobotID       string
	ObservedAt    time.Time
	Generation    int64
	HoldingObject *string
	InHold        bool
}

// StateReader reads one robot's current Runtime observation.
type StateReader func(context.Context, string, string, string) (StateObservation, error)

// StateReading is the conversation-facing snapshot of that observation.
type StateReading struct {
	Status        string     `json:"status"`
	Source        string     `json:"source"`
	ObservedAt    *time.Time `json:"observed_at"`
	Generation    *int64     `json:"generation,omitempty"`
	HoldingObject *string    `json:"holding_object"`
	InHold        *bool      `json:"in_hold"`
	Reason        string     `json:"reason,omitempty"`
	Limitation    string     `json:"limitation"`
}

const robotStateMaxAge = 10 * time.Second

// SetRobotStateReader wires an existing read-only Runtime API at bootstrap.
// Robot execution and GetRobot's complete persisted catalog remain unchanged.
func (s *Service) SetRobotStateReader(source string, reader StateReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stateSource, s.stateReader = source, reader
}

func (s *Service) ReadRobotState(ctx context.Context, projectID, robotID string) StateReading {
	s.mu.RLock()
	source, reader := s.stateSource, s.stateReader
	s.mu.RUnlock()
	result := StateReading{Status: "unavailable", Source: source,
		Limitation: "holding_object 仅表示此接口报告的携物对象；null 不能证明两个工具都空载。此接口不提供双工具负载验证。"}
	if reader == nil {
		result.Source, result.Reason = "unconfigured", "state_reader_not_configured"
		return result
	}
	instance, err := s.st.GetLatestRuntimeByRobot(ctx, robotID)
	if err != nil || instance.SceneInstanceID == "" {
		result.Reason = "runtime_state_unavailable"
		return result
	}
	if projectID == "" || (instance.ProjectID != "" && instance.ProjectID != projectID) {
		result.Reason = "robot_outside_project"
		return result
	}
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	observation, err := reader(readCtx, projectID, instance.SceneInstanceID, robotID)
	if err != nil {
		// Transport errors can contain Runtime URLs. Expose the availability
		// outcome without leaking endpoints or treating failure as empty hands.
		result.Reason = "state_read_failed"
		return result
	}
	if observation.RobotID != robotID {
		result.Reason = "state_robot_mismatch"
		return result
	}
	result.Status = "observed"
	result.HoldingObject, result.InHold = observation.HoldingObject, &observation.InHold
	result.Generation = &observation.Generation
	if observation.ObservedAt.IsZero() {
		result.Status, result.Reason = "stale", "observation_timestamp_missing"
		return result
	}
	result.ObservedAt = &observation.ObservedAt
	age := s.now().Sub(observation.ObservedAt)
	if age > robotStateMaxAge || age < -robotStateMaxAge {
		result.Status, result.Reason = "stale", "observation_timestamp_out_of_range"
	}
	return result
}
