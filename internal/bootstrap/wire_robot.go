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

package bootstrap

import (
	"insightos.cn/semantic-framework/internal/event"
	"insightos.cn/semantic-framework/internal/server/ws"
)

type robotEventPublisher struct{ bus *event.Bus }

func (p robotEventPublisher) PublishRobotEvent(projectID, resourceType, resourceID, eventType string, revision int64, payload any) {
	envelope := ws.NewEnvelope("", ws.ChannelDialogue, eventType, ws.ImportanceNormal, payload)
	envelope.ProjectID = projectID
	envelope.ResourceType = resourceType
	envelope.ResourceID = resourceID
	envelope.Revision = revision
	p.bus.Publish(event.TopicAgentEvents, envelope)
}
