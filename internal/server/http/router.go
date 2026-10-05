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

package http

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"insightos.cn/semantic-framework/internal/server/auth"
	"insightos.cn/semantic-framework/internal/server/http/handlers"
	"insightos.cn/semantic-framework/pkg/log"
	"insightos.cn/semantic-framework/pkg/version"
)

// NewRouter 装配 HTTP 网关路由表。
// 中间件链顺序：RequestID（注入 trace_id）→ Recovery（兜底 panic）→
// Logging（访问日志，含 401 也应可见）→ auth.Middleware（白名单外鉴权）。
// 路由分两组：公开组（登录、系统探活，由 auth 白名单放行）与受保护组
// （其余全部端点：系统 ping、对话域 /chat/*、设置域 /settings/*、
// Agent 目录 /agents、技能库 /skills/*、工具目录 /tools、
// 观测域 /traces|/metering|/interactions）。
func NewRouter(logger *log.Logger, authSvc *auth.Service, chatH *handlers.ChatHandler,
	projectsH *handlers.ProjectsHandler, settingsH *handlers.SettingsHandler,
	agentsH *handlers.AgentsHandler,
	skillsH *handlers.SkillsHandler, toolsH *handlers.ToolsHandler,
	tracesH *handlers.TracesHandler, meteringH *handlers.MeteringHandler,
	interactionsH *handlers.InteractionsHandler,
	robotsH *handlers.RobotsHandler,
	simulationH *handlers.SimulationHandler) http.Handler {
	authHandlers := auth.NewHandlers(authSvc)

	r := chi.NewRouter()
	r.Use(RequestID())
	r.Use(Recovery(logger))
	r.Use(Logging(logger))
	r.Use(authSvc.Middleware)

	// Pilot 从 Semantic Server 的 HTTP 地址流式下载 Robot Skill 包和
	// Artifact；WebSocket 端口只承载控制事件。把传输入口放在统一 HTTP
	// 网关后，单机双端口部署和反向代理部署都会得到相同地址语义。
	r.HandleFunc("/pilot/v1/transfers/*", robotsH.HandlePilotTransfer)

	r.Route("/api/v1", func(r chi.Router) {
		// 公开组：路径命中 auth 白名单，中间件直接放行。
		r.Post("/auth/login", authHandlers.HandleLogin)
		r.Get("/system/healthz", healthzHandler(logger))
		r.Get("/system/version", versionHandler())
		r.Post("/pilot-enrollments/claim", robotsH.HandleClaimPilotEnrollment)

		// 受保护组：必须经过 Bearer 鉴权。
		r.Post("/auth/refresh", authHandlers.HandleRefresh)
		r.Post("/auth/logout", authHandlers.HandleLogout)
		r.Get("/system/ping", pingHandler())
		r.Post("/pilot-enrollments", robotsH.HandleCreatePilotEnrollment)
		r.Delete("/pilot-enrollments/{id}", robotsH.HandleRevokePilotEnrollment)

		// Agent 目录：Team 成员清单与实时状态（roster 只读视图）。
		r.Get("/agents", agentsH.HandleListAgents)
		r.Put("/agents/{id}/models", agentsH.HandleUpdateAgentModels)

		// 技能库：SKILL.md 清单与详情（skill store 只读视图）。
		r.Route("/skills", func(r chi.Router) {
			r.Get("/", skillsH.HandleListSkills)
			r.Get("/{name}/resources/*", skillsH.HandleGetSkillResource)
			r.Get("/{name}", skillsH.HandleGetSkill)
		})

		// 工具目录：按来源分组的只读视图（内置 + MCP，含健康状态）。
		r.Get("/tools", toolsH.HandleListTools)

		// 观测域（架构文档 13 §8，只读）：链路追踪 / 计量 / 交互记录。
		// interactions 的 status=pending 过滤供前端审批卡刷新后恢复。
		r.Get("/traces", tracesH.HandleListTraces)
		r.Get("/traces/{trace_id}/spans", tracesH.HandleGetSpans)
		r.Get("/traces/{trace_id}/spans/{span_id}/io", tracesH.HandleGetSpanIO)
		r.Get("/metering/summary", meteringH.HandleSummary)
		r.Get("/metering/traces/{id}", meteringH.HandleTraceMetering)
		r.Get("/interactions", interactionsH.HandleListInteractions)

		// Robot Skill 包与设备中心。浏览器只访问 Server，不直连 Pilot。
		r.Route("/robot-skills", func(r chi.Router) {
			r.Get("/", robotsH.HandleListRobotSkills)
			r.Post("/", robotsH.HandlePublishRobotSkill)
			r.Get("/{name}/{version}", robotsH.HandleGetRobotSkill)
			r.Get("/{name}/{version}/resources/*", robotsH.HandleGetRobotSkillResource)
		})
		r.Route("/devices", func(r chi.Router) {
			r.Get("/", robotsH.HandleListDevices)
			r.Get("/snapshot", robotsH.HandleDeviceSnapshot)
			r.Get("/{robot_id}", robotsH.HandleGetDevice)
			r.Get("/{robot_id}/executions", robotsH.HandleListExecutions)
			r.Post("/{robot_id}/stop", robotsH.HandleStopDevice)
			r.Post("/{robot_id}/skills/{name}/{version}/install", robotsH.HandleInstallRobotSkill)
			r.Post("/{robot_id}/skills/{name}/{version}/{action}", robotsH.HandleRobotSkillAction)
			r.Delete("/{robot_id}/skills/{name}/{version}", robotsH.HandleDeleteRobotSkill)
			r.Post("/{robot_id}/abilities/debug", robotsH.HandleStartAbilityDebug)
			r.Post("/{robot_id}/abilities/debug/{debug_id}/stop", robotsH.HandleStopAbilityDebug)
		})
		r.Get("/robot-executions/{execution_id}", robotsH.HandleGetExecution)
		r.Post("/robot-executions/{execution_id}/stop", robotsH.HandleStopExecution)
		r.Post("/robot-executions/{execution_id}/agent-reply", robotsH.HandleAgentReply)
		r.Post("/projects/{id}/robots/{robot_id}/skill-executions", robotsH.HandleStartSkillDebug)

		if simulationH != nil {
			r.Route("/simulation", func(r chi.Router) {
				r.Get("/runtime-installations", simulationH.HandleRuntimeInstallations)
				r.Delete("/runtime-installations/{installation_id}", simulationH.HandleUninstallRuntime)
				r.Post("/runtime-installations/reload", simulationH.HandleReloadResources)
				r.Get("/scene-catalog", simulationH.HandleSceneCatalog)
				r.Get("/scene-previews/{key}/{file}", simulationH.HandleScenePreviewImage)
				r.Get("/scene-preview-assets/{scene_id}", simulationH.HandleBundledScenePreviewImage)
				r.Post("/runtime-installations/{installation_id}/probe", simulationH.HandleProbeRuntimeInstallation)
				r.Post("/runtime-installations/{installation_id}/start-test", simulationH.HandleTestRuntimeInstallation)
				r.Post("/runtime-installations/{installation_id}/stop", simulationH.HandleStopRuntimeInstallation)
				r.Put("/runtime-installations/{installation_id}/enabled", simulationH.HandleSetRuntimeInstallationEnabled)
			})
		}

		// Project 工作台：v0.2 的 Project、Conversation、Memory、Run 和
		// Studio Snapshot。消息发送与增量订阅走 /ws/studio。
		r.Route("/projects", func(r chi.Router) {
			r.Get("/", projectsH.HandleListProjects)
			r.Post("/", projectsH.HandleCreateProject)
			r.Get("/{id}", projectsH.HandleGetProject)
			r.Get("/{id}/imports", projectsH.HandleListImports)
			if simulationH != nil {
				r.Post("/{id}/scene-previews/{scene_id}", simulationH.HandlePrepareScenePreviews)
				r.Post("/{id}/scene-previews/{scene_id}/cancel", simulationH.HandleCancelScenePreviews)
			}
			r.Post("/{id}/imports", projectsH.HandleUploadImport)
			r.Post("/{id}/imports/scan", projectsH.HandleScanImports)
			r.Post("/{id}/imports/{import_id}/retry", projectsH.HandleRetryImport)
			r.Post("/{id}/imports/{import_id}/install", projectsH.HandleInstallImport)
			r.Post("/{id}/imports/{import_id}/cancel", projectsH.HandleCancelInstallation)
			r.Get("/{id}/components", projectsH.HandleComponentVersions)
			r.Post("/{id}/components/bind", projectsH.HandleBindComponents)
			r.Post("/{id}/components/apply", projectsH.HandleApplyComponents)
			r.Delete("/{id}/components/{component_id}", projectsH.HandleRemoveComponent)
			r.Post("/{id}/components/rollback", projectsH.HandleRollbackComponents)
			r.Patch("/{id}", projectsH.HandleUpdateProject)
			r.Delete("/{id}", projectsH.HandleArchiveProject)
			r.Post("/{id}/activate", projectsH.HandleActivateProject)
			r.Get("/{id}/bindings", projectsH.HandleGetProjectBindings)
			r.Put("/{id}/bindings", projectsH.HandleReplaceProjectBindings)
			r.Get("/{id}/memory", projectsH.HandleGetMemory)
			r.Put("/{id}/memory", projectsH.HandleSaveMemory)
			r.Get("/{id}/conversations", projectsH.HandleListConversations)
			r.Post("/{id}/conversations", projectsH.HandleCreateConversation)
			r.Delete("/{id}/conversations/{conversation_id}",
				projectsH.HandleArchiveConversation)
			r.Get("/{id}/runs", projectsH.HandleListRuns)
			r.Get("/{id}/studio/snapshot", projectsH.HandleStudioSnapshotV030)
			r.Get("/{id}/robot-executions", robotsH.HandleListProjectExecutions)
			if simulationH != nil {
				r.Route("/{id}/simulation", func(r chi.Router) {
					r.Post("/runtime/ensure", simulationH.HandleEnsureRuntime)
					r.Post("/runtime/recover-interrupted", simulationH.HandleRecoverInterruptedRuntime)
					r.Get("/runtime-preference", simulationH.HandleProjectRuntimePreference)
					r.Put("/runtime-preference", simulationH.HandleSetProjectRuntimePreference)
					r.Post("/runtime/release", simulationH.HandleReleaseProjectSimulation)
					r.Get("/project-scenes", simulationH.HandleListProjectScenes)
					r.Post("/project-scenes", simulationH.HandleAddProjectScene)
					r.Delete("/project-scenes/{project_scene_id}", simulationH.HandleRemoveProjectScene)
					r.Post("/project-scenes/{project_scene_id}/layout-drafts",
						simulationH.HandleCreateProjectLayoutDraft)
					r.Post("/project-scenes/{project_scene_id}/instances",
						simulationH.HandleStartProjectScene)
					r.Post("/instances/{instance_id}/switch-variant",
						simulationH.HandleSwitchProjectSceneVariant)

					r.Get("/runtime-profiles", simulationH.HandleRuntimeProfiles)
					r.Get("/snapshot", simulationH.HandleSnapshot)
					r.Get("/scenes", simulationH.HandleListScenes)
					r.Post("/scenes/{scene_key}/instances", simulationH.HandleStartScene)
					r.Get("/instances/{instance_id}", simulationH.HandleScene)
					r.Get("/instances/{instance_id}/snapshot", simulationH.HandleSceneSnapshot)
					r.Get("/instances/{instance_id}/viewer-scene", simulationH.HandleViewerScene)
					r.Get("/instances/{instance_id}/viewer-scene/content", simulationH.HandleViewerSceneContent)
					r.Post("/instances/{instance_id}/sync-map", simulationH.HandleSyncSceneMap)
					r.Get("/instances/{instance_id}/source-links", simulationH.HandleSourceLink)
					r.Get("/instances/{instance_id}/evaluation", simulationH.HandleSceneEvaluation)
					r.Get("/instances/{instance_id}/robots", simulationH.HandleRobots)
					r.Post("/instances/{instance_id}/{operation:pause|resume|step|reset|stop}",
						simulationH.HandleSceneOperation)
					r.Get("/instances/{instance_id}/robots/{robot_id}/state",
						simulationH.HandleRobotState)
					r.Get("/instances/{instance_id}/robots/{robot_id}/sensors",
						simulationH.HandleRobotSensors)
					r.Post("/instances/{instance_id}/robots/{robot_id}/commands",
						simulationH.HandleRobotCommand)
					r.Get("/instances/{instance_id}/robots/{robot_id}/commands/{command_id}",
						simulationH.HandleRobotCommandState)
					r.Post("/instances/{instance_id}/robots/{robot_id}/commands/{command_id}/stop",
						simulationH.HandleRobotCommandStop)
					r.Post("/instances/{instance_id}/robots/{robot_id}/hold",
						simulationH.HandleRobotHold)
					r.Get("/scene-assets", simulationH.HandleListSceneAssets)
					r.Get("/visual-assets/{visual_id}/{version}.glb",
						simulationH.HandleVisualAsset)
					r.Get("/scene-documents", simulationH.HandleListDocuments)
					r.Post("/scene-documents", simulationH.HandleCreateDocument)
					r.Get("/scene-documents/{document_id}", simulationH.HandleGetDocument)
					r.Put("/scene-documents/{document_id}", simulationH.HandleUpdateDocument)
					r.Post("/scene-documents/{document_id}/operations",
						simulationH.HandleDocumentOperations)
					r.Post("/scene-documents/{document_id}/validate",
						simulationH.HandleValidateDocument)
					r.Post("/scene-documents/{document_id}/build",
						simulationH.HandleBuildDocument)
					r.Post("/scene-documents/{document_id}/publish",
						simulationH.HandlePublishDocument)
					r.Post("/scene-documents/{document_id}/fork",
						simulationH.HandleForkDocument)
					r.Get("/scene-documents/{document_id}/layouts",
						simulationH.HandleListDocumentLayouts)
					r.Post("/scene-documents/{document_id}/layouts",
						simulationH.HandleCreateDocumentLayout)
					r.Patch("/scene-documents/{document_id}/layout",
						simulationH.HandleRenameDocumentLayout)
					r.Delete("/scene-documents/{document_id}/layout",
						simulationH.HandleDeleteDocumentLayout)
					r.Get("/scenes/{scene_id}/package", simulationH.HandleExportScenePackage)
					r.Post("/scene-packages/import", simulationH.HandleImportScenePackage)
				})
			}
			r.Get("/{id}/plan-proposals/active", projectsH.HandleGetActivePlanProposal)
			r.Get("/{id}/plan-proposals/{proposal_id}", projectsH.HandleGetPlanProposal)
			r.Post("/{id}/plan-proposals/{proposal_id}/{action:approve|discard}",
				projectsH.HandlePlanProposalAction)
			r.Get("/{id}/workflows", projectsH.HandleListWorkflows)
			r.Get("/{id}/workflows/active", projectsH.HandleGetActiveWorkflow)
			r.Get("/{id}/workflows/{workflow_id}/view", projectsH.HandleGetWorkflowView)
			// 用户批准前的修订只发生在 Conversation + Plan Proposal；Workflow
			// 创建后只暴露运行控制，旧的直接 PATCH/feedback/confirm 入口下线。
			r.Post("/{id}/workflows/{workflow_id}/{action:pause|resume|stop|retry-decision|confirm-stop}",
				projectsH.HandleWorkflowAction)
			r.Get("/{id}/maps/{map_id}", projectsH.HandleGetSemanticMap)
			r.Post("/{id}/maps/{map_id}/query", projectsH.HandleQuerySemanticMap)
			r.Post("/{id}/maps/{map_id}/generations", projectsH.HandleCreateMapGeneration)
			r.Post("/{id}/maps/{map_id}/updates", projectsH.HandleUpdateSemanticMap)
		})
		r.Get("/runs/{id}", projectsH.HandleGetRun)
		r.Get("/runs/{id}/events", projectsH.HandleRunEvents)
		r.Post("/runs/{id}/cancel", projectsH.HandleCancelRun)

		// 对话域：保留旧 CLI 与迁移期页面使用。
		r.Route("/chat", func(r chi.Router) {
			r.Post("/attachments", chatH.HandleUploadAttachment)
			r.Get("/attachments/{id}", chatH.HandleGetAttachment)
			r.Post("/artifacts/register", chatH.HandleRegisterWorkspaceArtifact)
			r.Get("/artifacts", chatH.HandleListArtifacts)
			r.Get("/artifacts/{id}", chatH.HandleGetAttachment)
			r.Delete("/artifacts/{id}", chatH.HandleDeleteArtifact)
			r.Post("/sessions", chatH.HandleCreateSession)
			r.Get("/sessions", chatH.HandleListSessions)
			r.Get("/sessions/{id}/agents", agentsH.HandleListSessionAgents)
			r.Get("/sessions/{id}/agents/{agent_id}/tools", agentsH.HandleListSessionAgentTools)
			r.Put("/sessions/{id}/agents/{agent_id}/model", agentsH.HandleUpdateSessionAgentModel)
			r.Get("/sessions/{id}/messages", chatH.HandleListMessages)
			r.Get("/sessions/{id}/host-execution", chatH.HandleGetHostExecution)
			r.Put("/sessions/{id}/host-execution", chatH.HandleUpdateHostExecution)
			r.Delete("/sessions/{id}", chatH.HandleDeleteSession)
		})

		// 设置域：生效配置快照（掩码）/ 配置 PATCH（乐观锁 + 热应用）/
		// 托管密钥管理。审计只记变更键清单，绝不记值。
		r.Route("/settings", func(r chi.Router) {
			r.Get("/", settingsH.HandleGetSettings)
			r.Patch("/", settingsH.HandlePatchSettings)
			r.Get("/keys", settingsH.HandleListKeys)
			r.Put("/keys/{name}", settingsH.HandlePutKey)
			r.Delete("/keys/{name}", settingsH.HandleDeleteKey)
		})
	})

	logger.Debug("HTTP 路由注册完成", "prefix", "/api/v1")
	return r
}

// healthzHandler 返回健康检查处理器：服务进程存活即返回 200。
// TODO(B3)：接入真实依赖探活（store ping 等）。
func healthzHandler(logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logger.Debug("健康检查请求", "remote", r.RemoteAddr)
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// versionHandler 返回版本查询处理器，版本号由 pkg/version 统一提供。
func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"version": version.String()})
	}
}

// pingHandler 返回鉴权链路验证处理器：回显 pong 与 context 中的 user_id，
// 证明 Bearer token 已通过中间件校验并正确注入。
func pingHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]any{
			"pong":    true,
			"user_id": auth.UserIDFromContext(r.Context()),
		})
	}
}
