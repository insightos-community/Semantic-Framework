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

package pilot

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// JSONLineEventSink 把 Skill 与 Pilot 事件写成一行一个 JSON 对象。
// stdout 留给 Worker 协议和最终结果，因此调用方应把该日志写到 stderr 或文件。
type JSONLineEventSink struct {
	writer io.Writer
	mu     sync.Mutex
	now    func() time.Time
}

func NewJSONLineEventSink(writer io.Writer) *JSONLineEventSink {
	return &JSONLineEventSink{writer: writer, now: time.Now}
}

func (s *JSONLineEventSink) Report(execution SkillExecution, event string, fields map[string]any) {
	s.write(execution, "event", "", event, fields)
}

func (s *JSONLineEventSink) Log(execution SkillExecution, level, message string, fields map[string]any) {
	s.write(execution, "log", level, message, fields)
}

func (s *JSONLineEventSink) write(
	execution SkillExecution,
	kind string,
	level string,
	message string,
	fields map[string]any,
) {
	if s == nil || s.writer == nil {
		return
	}
	record := map[string]any{
		"timestamp":          s.now().UTC().Format(time.RFC3339Nano),
		"kind":               kind,
		"level":              level,
		"message":            message,
		"robot_id":           execution.RobotID,
		"project_id":         execution.ProjectID,
		"task_id":            execution.TaskID,
		"subtask_id":         execution.SubtaskID,
		"skill_execution_id": execution.ID,
		"skill_name":         execution.SkillName,
		"skill_version":      execution.SkillVersion,
		"fields":             cloneMap(fields),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.writer.Write(append(encoded, '\n'))
}
