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
