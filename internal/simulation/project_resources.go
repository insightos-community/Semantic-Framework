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

package simulation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProjectRuntimePreferenceResolver 只暴露 Project 的可移植 Profile 和本机
// Installation 偏好。实际 Scene 启动后使用的 Installation 仍写入 RuntimeState。
type ProjectRuntimePreferenceResolver interface {
	RuntimePreference(projectID string) (profileID, preferredInstallationID string, err error)
	RememberRuntimePreference(projectID, profileID, installationID string) error
}

// ConfigureProjectResources 接入管理员安装清单、离线场景目录和 Project 运行偏好。
// 测试使用旧构造器时可以不配置，此时保留单 Profile 兼容行为。
func (s *Service) ConfigureProjectResources(
	installations *RuntimeInstallationCatalog,
	catalog *SceneCatalogService,
	resolver ProjectRuntimePreferenceResolver,
) {
	s.installations = installations
	s.sceneCatalog = catalog
	s.projectBindings = resolver
}

func (s *Service) RuntimeInstallations() []RuntimeInstallationView {
	if s.installations == nil {
		return nil
	}
	return s.installations.Views()
}

func (s *Service) SceneCatalog(profileID string) []SceneCatalogEntry {
	if s.sceneCatalog == nil {
		return nil
	}
	return s.decorateScenePreviews(s.sceneCatalog.List(profileID))
}

func (s *Service) SceneCatalogVersionID() string {
	if s.sceneCatalog == nil {
		return ""
	}
	return s.sceneCatalog.CatalogVersion()
}

func (s *Service) runtimeProfile(profileID string) (RuntimeProfile, error) {
	for _, profile := range s.registry.Profiles() {
		if profile.RuntimeProfileID == profileID {
			return profile, nil
		}
	}
	return RuntimeProfile{}, fmt.Errorf("%w: Runtime Profile %s 不存在", ErrNotFound, profileID)
}

func (s *Service) ProjectRuntimeProfile(projectID string) (RuntimeProfile, error) {
	profileID, _, err := s.projectRuntimePreference(projectID)
	if err != nil {
		return RuntimeProfile{}, err
	}
	if profileID == "" {
		return RuntimeProfile{}, fmt.Errorf("%w: Project 尚未选择 Runtime Profile", ErrConflict)
	}
	return s.runtimeProfile(profileID)
}

func profileSupportsSceneKind(profile RuntimeProfile, sceneKind string) bool {
	if sceneKind == "" {
		return true
	}
	for _, candidate := range profile.SceneKinds {
		if candidate == sceneKind {
			return true
		}
	}
	return false
}

func (s *Service) projectRuntimeViews(
	projectID string,
) ([]string, *RuntimeInstallationView, error) {
	if s.installations == nil || s.projectBindings == nil {
		return s.registry.BindingIDs(), nil, nil
	}
	_, installationID, err := s.projectBindings.RuntimePreference(projectID)
	if err != nil {
		return nil, nil, err
	}
	if installationID == "" {
		return nil, nil, nil
	}
	installation, err := s.installations.Get(installationID)
	if err != nil {
		view := RuntimeInstallationView{
			InstallationID: installationID, Enabled: false, Status: "failed",
			Diagnostic: "Project 偏好的 Runtime Installation 不存在，请重新选择兼容安装",
		}
		return nil, &view, nil
	}
	view, err := s.installations.View(installation.InstallationID)
	if err != nil {
		return nil, nil, err
	}
	return []string{installation.InstallationID}, &view, nil
}

func (s *Service) RuntimeInstallation(installationID string) (RuntimeInstallation, error) {
	if s.installations == nil {
		return RuntimeInstallation{}, errors.New("RuntimeInstallation 尚未配置")
	}
	return s.installations.Get(installationID)
}

func (s *Service) ValidateRuntimePreference(profileID, installationID string) error {
	profileID = strings.TrimSpace(profileID)
	installationID = strings.TrimSpace(installationID)
	if profileID == "" {
		return errors.New("runtime_profile_id 不能为空")
	}
	if _, err := s.runtimeProfile(profileID); err != nil {
		return err
	}
	if installationID == "" {
		return nil
	}
	installation, err := s.RuntimeInstallation(installationID)
	if err != nil {
		return err
	}
	if installation.Profile.RuntimeProfileID != profileID {
		return fmt.Errorf("%w: Runtime Installation %s 不属于 Profile %s",
			ErrConflict, installationID, profileID)
	}
	return validateInstallationAvailable(installation)
}

func (s *Service) ProjectRuntimePreference(
	projectID string,
) (profileID, preferredInstallationID string, candidates []RuntimeInstallationView, err error) {
	profileID, preferredInstallationID, err = s.projectRuntimePreference(projectID)
	if err != nil || profileID == "" {
		return profileID, preferredInstallationID, nil, err
	}
	for _, candidate := range s.installations.Views() {
		if candidate.ProfileID != profileID || !candidate.Enabled {
			continue
		}
		installation, getErr := s.installations.Get(candidate.InstallationID)
		if getErr == nil && validateInstallationAvailable(installation) == nil {
			// 返回 View 保留最近一次动态探测状态，但候选资格只由安装清单的
			// 静态有效性决定。受管 Runtime 的 offline 正是启动时可恢复的状态。
			candidates = append(candidates, candidate)
		}
	}
	return profileID, preferredInstallationID, candidates, nil
}

func validateInstallationAvailable(installation RuntimeInstallation) error {
	if installation.Enabled && installation.Diagnostic == "" {
		return nil
	}
	reason := installation.Diagnostic
	if reason == "" {
		reason = "安装已停用"
	}
	return fmt.Errorf("%w: Runtime %s: %s", ErrRuntimeUnavailable,
		installation.InstallationID, reason)
}

func (s *Service) EnsureRuntimeInstallation(
	ctx context.Context, installationID string,
) (RuntimeInfo, error) {
	installation, err := s.RuntimeInstallation(installationID)
	if err != nil {
		return RuntimeInfo{}, err
	}
	if err := validateInstallationAvailable(installation); err != nil {
		return RuntimeInfo{}, err
	}
	s.installations.ObserveStatus(installationID, "starting", "")
	info, err := s.supervisor.Ensure(ctx, installation.InstallationID)
	if err != nil {
		s.installations.ObserveStatus(installationID, "failed", err.Error())
		return RuntimeInfo{}, err
	}
	s.installations.ObserveStatus(installationID, "ready", "")
	return info, nil
}

func (s *Service) StopRuntimeInstallation(
	ctx context.Context, installationID string,
) (bool, error) {
	if _, err := s.RuntimeInstallation(installationID); err != nil {
		return false, err
	}
	s.installations.ObserveStatus(installationID, "stopping", "")
	stopped, err := s.supervisor.StopManaged(ctx, installationID)
	if err != nil {
		s.installations.ObserveStatus(installationID, "failed", err.Error())
		return stopped, err
	}
	s.installations.ObserveStatus(installationID, "offline", "")
	return stopped, nil
}

func (s *Service) ProbeRuntimeInstallation(
	ctx context.Context, installationID string,
) (RuntimeInfo, error) {
	if _, err := s.RuntimeInstallation(installationID); err != nil {
		return RuntimeInfo{}, err
	}
	info, err := s.supervisor.Probe(ctx, installationID)
	if err != nil {
		s.installations.ObserveStatus(installationID, "offline", err.Error())
		return RuntimeInfo{}, err
	}
	s.installations.ObserveStatus(installationID, "ready", "")
	return info, nil
}

// SetRuntimeInstallationEnabled 原子写回固定安装清单。Registry 与 Supervisor
// 使用 Server 启动时快照，因此返回 restartRequired=true，避免热替换进程边界。
func (s *Service) SetRuntimeInstallationEnabled(
	ctx context.Context, installationID string, enabled bool,
) (view RuntimeInstallationView, restartRequired bool, err error) {
	if s.installations == nil {
		return RuntimeInstallationView{}, false, errors.New("RuntimeInstallation 尚未配置")
	}
	current, err := s.installations.Get(installationID)
	if err != nil {
		return RuntimeInstallationView{}, false, err
	}
	if current.Enabled == enabled {
		return current.Public(), false, nil
	}
	if current.SourceFile == "" {
		return RuntimeInstallationView{}, false, errors.New(
			"内置 Runtime 清单是只读模板；请先运行 semantic init 后修改安装目录中的清单",
		)
	}
	if !enabled && current.Enabled {
		// 停用安装只管理运行环境；活动场景先走场景停止流程，保留 Robot 的
		// 安全停止与状态对账，避免设置开关直接切断正在工作的进程。
		info, probeErr := s.supervisor.Probe(ctx, installationID)
		if probeErr == nil && info.ActiveInstanceID != "" {
			return RuntimeInstallationView{}, false, fmt.Errorf("%w: 请先停止 Runtime 中的活动场景", ErrConflict)
		}
		if _, err := s.supervisor.StopManaged(ctx, installationID); err != nil {
			return RuntimeInstallationView{}, false, err
		}
	}
	view, err = s.installations.SetEnabled(installationID, enabled)
	if err != nil {
		return RuntimeInstallationView{}, false, err
	}
	return view, true, nil
}

func (s *Service) projectRuntimePreference(projectID string) (string, string, error) {
	if s.installations == nil || s.projectBindings == nil {
		return "", "", errors.New("Project Runtime 资源尚未配置")
	}
	profileID, preferredID, err := s.projectBindings.RuntimePreference(projectID)
	if err != nil || profileID != "" || preferredID == "" {
		return profileID, preferredID, err
	}
	// v21 数据只保存 Installation，v31 的 SQL 迁移无法读取启动期安装清单来
	// 推导 Profile。首次读取时用新偏好列完成一次性回填；之后所有路径只读
	// runtime_profile_id，不保留旧 runtime_installation_id 的双读逻辑。
	installation, err := s.installations.Get(preferredID)
	if err != nil {
		return "", preferredID, fmt.Errorf("%w: Project 偏好的 Runtime %s 不存在",
			ErrRuntimeUnavailable, preferredID)
	}
	profileID = installation.Profile.RuntimeProfileID
	if err := s.projectBindings.RememberRuntimePreference(projectID, profileID, preferredID); err != nil {
		return "", preferredID, err
	}
	return profileID, preferredID, nil
}

// SelectProjectRuntimeInstallation 在启动动作发生时选择具体 Installation。
// 显式选择优先，其次使用仍可用的 Project 偏好；只有一个候选时自动选择。
// 多候选没有偏好时返回冲突，让 Studio 在当前启动面板内选择，而不是永久绑定。
func (s *Service) SelectProjectRuntimeInstallation(
	projectID, requestedProfileID, requestedInstallationID string,
) (RuntimeInstallation, error) {
	profileID, preferredID, err := s.projectRuntimePreference(projectID)
	if err != nil {
		return RuntimeInstallation{}, err
	}
	requestedProfileID = strings.TrimSpace(requestedProfileID)
	requestedInstallationID = strings.TrimSpace(requestedInstallationID)
	if profileID == "" {
		profileID = requestedProfileID
	}
	if profileID == "" {
		return RuntimeInstallation{}, fmt.Errorf("%w: Project 尚未选择 Runtime Profile", ErrConflict)
	}
	if requestedProfileID != "" && requestedProfileID != profileID {
		return RuntimeInstallation{}, fmt.Errorf("%w: 场景需要 %s，Project 默认 Profile 为 %s",
			ErrConflict, requestedProfileID, profileID)
	}
	available := make([]RuntimeInstallation, 0)
	for _, view := range s.installations.Views() {
		if view.ProfileID != profileID || !view.Enabled {
			continue
		}
		installation, getErr := s.installations.Get(view.InstallationID)
		// View 中的 Diagnostic 还包含上一次 Probe 的动态故障。离线或进程
		// 崩溃正是 Ensure 需要重新拉起的场景，不能把这次观测当成永久的
		// 安装不可用；只有安装清单自身的静态诊断才应排除候选。
		if getErr == nil && validateInstallationAvailable(installation) == nil {
			available = append(available, installation)
		}
	}
	selectID := requestedInstallationID
	if selectID == "" {
		selectID = preferredID
	}
	if selectID != "" {
		for _, installation := range available {
			if installation.InstallationID == selectID {
				return installation, nil
			}
		}
		if requestedInstallationID != "" {
			return RuntimeInstallation{}, fmt.Errorf("%w: Runtime Installation %s 不可用或与 %s 不兼容",
				ErrRuntimeUnavailable, selectID, profileID)
		}
	}
	if len(available) == 1 {
		return available[0], nil
	}
	if len(available) == 0 {
		return RuntimeInstallation{}, fmt.Errorf("%w: 没有可用的 %s Runtime Installation",
			ErrRuntimeUnavailable, profileID)
	}
	return RuntimeInstallation{}, fmt.Errorf("%w: %s 有多个可用 Runtime Installation，请在启动场景时选择",
		ErrConflict, profileID)
}

// EnsureProjectRuntime 是显式准备 Runtime 的兼容入口。打开 Project 不再调用它；
// 正式产品链在启动 Scene 时解析 Profile 与 Installation，避免仅浏览 Project 就占用仿真资源。
func (s *Service) EnsureProjectRuntime(
	ctx context.Context, projectID string,
) (RuntimeInfo, error) {
	installation, err := s.SelectProjectRuntimeInstallation(projectID, "", "")
	if err != nil {
		return RuntimeInfo{}, err
	}
	if err := validateInstallationAvailable(installation); err != nil {
		return RuntimeInfo{}, err
	}
	info, err := s.EnsureRuntimeInstallation(ctx, installation.InstallationID)
	if err != nil {
		return RuntimeInfo{}, err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	state, loadErr := s.state.LoadRuntimeState(projectID)
	if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
		return RuntimeInfo{}, loadErr
	}
	state.ProjectID = projectID
	state.RuntimeInstallationID = installation.InstallationID
	state.RuntimeProfileID = installation.Profile.RuntimeProfileID
	state.Revision++
	state.UpdatedAt = time.Now().UTC()
	if err := s.state.SaveRuntimeState(state); err != nil {
		return RuntimeInfo{}, err
	}
	return info, nil
}

// resolveProjectProfile 只检查可移植 Profile；Installation 在具体启动动作中选择。
func (s *Service) resolveProjectProfile(projectID, requested string) (string, error) {
	if s.installations == nil || s.projectBindings == nil {
		if requested != "" {
			return requested, nil
		}
		return s.registry.DefaultProfileID()
	}
	profileID, _, err := s.projectBindings.RuntimePreference(projectID)
	if err != nil {
		return "", err
	}
	if profileID == "" {
		profileID = requested
	}
	if profileID == "" {
		return "", fmt.Errorf("%w: Project 尚未选择 Runtime Profile", ErrConflict)
	}
	if requested != "" && requested != profileID {
		return "", fmt.Errorf("%w: Project Runtime 为 %s，不能使用 %s",
			ErrConflict, profileID, requested)
	}
	return profileID, nil
}

func (s *Service) runtimeIdentity(projectID, profileID, installationID string) (string, error) {
	if s.installations == nil || s.projectBindings == nil {
		if profileID != "" {
			return profileID, nil
		}
		return s.registry.DefaultBindingID()
	}
	installation, err := s.SelectProjectRuntimeInstallation(projectID, profileID, installationID)
	if err != nil {
		return "", err
	}
	if err := validateInstallationAvailable(installation); err != nil {
		return "", err
	}
	if profileID != "" && installation.Profile.RuntimeProfileID != profileID {
		return "", fmt.Errorf("%w: Project Runtime Profile 与场景状态不一致", ErrConflict)
	}
	return installation.InstallationID, nil
}

// ReleaseProject 按退出语义收敛场景与受管 Runtime。remote 安装没有受管进程，
// StopManaged 只解除本 Project 的状态，不会终止远程服务。
func (s *Service) ReleaseProject(ctx context.Context, projectID string) error {
	s.stateMu.Lock()
	state, err := s.state.LoadRuntimeState(projectID)
	s.stateMu.Unlock()
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	if state.InstanceID != "" {
		if _, stopErr := s.SceneOperation(ctx, projectID, state.InstanceID, "stop", nil); stopErr != nil && !errors.Is(stopErr, ErrNotFound) {
			result = errors.Join(result, stopErr)
		}
	}
	runtimeID := state.RuntimeInstallationID
	if runtimeID == "" {
		runtimeID = state.RuntimeProfileID
	}
	if runtimeID != "" {
		if _, stopErr := s.supervisor.StopManaged(ctx, runtimeID); stopErr != nil {
			result = errors.Join(result, stopErr)
		}
	}
	return result
}

// SwitchLayout 采用停止旧实例再启动新实例的受控重载。Runtime 进程保持，Viewer
// 与传感器会话由 stop 清理；新实例使用新的 request_id，旧 generation 无法复用。
func (s *Service) SwitchLayout(
	ctx context.Context,
	projectID, instanceID, sceneKey, layout string,
	seed int64,
) (SceneInstance, error) {
	if layout == "" {
		return SceneInstance{}, errors.New("layout 不能为空")
	}
	state, _, err := s.projectRuntime(projectID, instanceID)
	if err != nil {
		return SceneInstance{}, err
	}
	last := state.LastInstance
	if _, err := s.SceneOperation(ctx, projectID, instanceID, "stop", nil); err != nil {
		return SceneInstance{}, err
	}
	request := SceneStartRequest{
		RequestID:        "layout-switch-" + fmt.Sprint(time.Now().UTC().UnixNano()),
		RuntimeProfileID: state.RuntimeProfileID,
		Layout:           layout,
		Seed:             seed,
		Headless:         true,
		RenderBackend:    "egl",
	}
	if last != nil {
		request.RuntimeBundleID = last.RuntimeBundleID
		if sceneKey == "" {
			sceneKey = last.SceneKey
		}
		request.Headless = last.Headless
		request.RenderBackend = last.RenderBackend
	}
	return s.StartScene(ctx, projectID, sceneKey, request)
}

// RecoverInterruptedProject 是用户明确确认“放弃最后一次不可确认实例并重新启动”
// 后调用的恢复入口。普通 stop 仍必须经过 Runtime，以保证 Robot hold 和资源释放；
// 只有 Runtime 已不可连接且安装为本机受管进程时，才允许清除 Framework 的活动
// 实例槽。远程 Runtime 可能仍在驱动真实或共享资源，不能因网络中断而擅自放弃。
func (s *Service) RecoverInterruptedProject(
	ctx context.Context, projectID, instanceID string,
) (RuntimeInfo, error) {
	snapshot, err := s.Snapshot(ctx, projectID)
	if err != nil {
		return RuntimeInfo{}, err
	}
	if snapshot.Instance == nil || snapshot.Instance.InstanceID != instanceID {
		return RuntimeInfo{}, fmt.Errorf("%w: 待恢复的场景实例已变化", ErrConflict)
	}
	if snapshot.RecoveryInfo == nil || snapshot.RecoveryInfo.DerivedState != "interrupted" {
		return RuntimeInfo{}, fmt.Errorf("%w: 当前实例不是中断状态，请使用正常 stop", ErrConflict)
	}

	// Runtime 在确认窗口内恢复时，仍优先执行完整 stop/hold 链路。SceneOperation
	// 可能已经完成 stop 但在保存检查点时返回错误，因此后面必须重新读取状态。
	_, stopErr := s.SceneOperation(ctx, projectID, instanceID, "stop", nil)
	s.stateMu.Lock()
	state, loadErr := s.state.LoadRuntimeState(projectID)
	s.stateMu.Unlock()
	if loadErr != nil {
		return RuntimeInfo{}, errors.Join(stopErr, loadErr)
	}
	if state.InstanceID == "" {
		return s.EnsureProjectRuntime(ctx, projectID)
	}
	if state.InstanceID != instanceID {
		return RuntimeInfo{}, fmt.Errorf("%w: 恢复期间活动实例已变化", ErrConflict)
	}

	runtimeID := state.RuntimeInstallationID
	if runtimeID == "" {
		runtimeID = state.RuntimeProfileID
	}
	if s.installations != nil {
		installation, installationErr := s.RuntimeInstallation(runtimeID)
		if installationErr != nil {
			return RuntimeInfo{}, errors.Join(stopErr, installationErr)
		}
		if installation.LaunchMode == "remote" {
			return RuntimeInfo{}, fmt.Errorf(
				"%w: 远程 Runtime 状态不可确认，不能强制放弃实例；请先恢复远程连接后 stop",
				ErrRuntimeUnavailable,
			)
		}
	}
	if _, stopManagedErr := s.supervisor.StopManaged(ctx, runtimeID); stopManagedErr != nil {
		return RuntimeInfo{}, errors.Join(stopErr, stopManagedErr)
	}

	// 受管进程已经退出后再原子释放活动槽。LastInstance 保留 interrupted 终态用于
	// 日志/验收回看，但 InstanceID 清空后它不再阻止新的 SceneInstance。
	s.stateMu.Lock()
	state, loadErr = s.state.LoadRuntimeState(projectID)
	if loadErr == nil && state.InstanceID == instanceID {
		if state.LastInstance != nil {
			last := *state.LastInstance
			last.State = "interrupted"
			last.FailureReason = snapshot.RecoveryInfo.Message
			last.UpdatedAt = time.Now().UTC()
			state.LastInstance = &last
		}
		state.InstanceID = ""
		state.Revision++
		state.UpdatedAt = time.Now().UTC()
		loadErr = s.state.SaveRuntimeState(state)
	}
	s.stateMu.Unlock()
	if loadErr != nil {
		return RuntimeInfo{}, errors.Join(stopErr, loadErr)
	}
	return s.EnsureProjectRuntime(ctx, projectID)
}
