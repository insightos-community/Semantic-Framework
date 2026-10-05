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
	"sort"
	"sync"
	"time"
)

// RuntimeBinding 把一个静态 profile 与客户端、按需启动器关联。
// Client 和 Launcher 只保存在 Server 内存中，不会进入 Studio Snapshot。
type RuntimeBinding struct {
	InstallationID string
	Profile        RuntimeProfile
	Client         RuntimeClient
	Launcher       RuntimeLauncher
}

// RuntimeRegistry 保存所有可用的运行环境。
type RuntimeRegistry struct {
	mu        sync.RWMutex
	bindings  map[string]RuntimeBinding
	observed  map[string]RuntimeProfile
	byProfile map[string][]string
	order     []string
}

func NewRuntimeRegistry(bindings ...RuntimeBinding) (*RuntimeRegistry, error) {
	registry := &RuntimeRegistry{
		byProfile: make(map[string][]string),
		bindings:  make(map[string]RuntimeBinding),
		observed:  make(map[string]RuntimeProfile),
	}
	for _, binding := range bindings {
		if err := registry.Register(binding); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *RuntimeRegistry) Register(binding RuntimeBinding) error {
	if binding.Profile.RuntimeProfileID == "" || binding.Client == nil {
		return errors.New("runtime_profile_id 和 RuntimeClient 不能为空")
	}
	key := binding.InstallationID
	if key == "" {
		key = binding.Profile.RuntimeProfileID
		binding.InstallationID = key
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.bindings[key]; exists {
		return fmt.Errorf("%w: Runtime installation 重复: %s", ErrConflict, key)
	}
	r.bindings[key] = binding
	r.byProfile[binding.Profile.RuntimeProfileID] = append(r.byProfile[binding.Profile.RuntimeProfileID], key)
	r.order = append(r.order, key)
	sort.Strings(r.order)
	return nil
}

func (r *RuntimeRegistry) Profiles() []RuntimeProfile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]RuntimeProfile, 0, len(r.order))
	for _, profileID := range r.order {
		profile := r.bindings[profileID].Profile
		if actual, ok := r.observed[profileID]; ok {
			profile = actual
		}
		result = append(result, profile)
	}
	return result
}

func (r *RuntimeRegistry) bindingKeyLocked(identity string) (string, error) {
	if _, ok := r.bindings[identity]; ok {
		return identity, nil
	}
	keys := r.byProfile[identity]
	if len(keys) == 1 {
		return keys[0], nil
	}
	if len(keys) > 1 {
		return "", fmt.Errorf("%w: Runtime profile %s 对应多个安装，必须使用 installation_id",
			ErrConflict, identity)
	}
	return "", fmt.Errorf("%w: Runtime installation/profile 不存在: %s", ErrNotFound, identity)
}
func (r *RuntimeRegistry) Binding(identity string) (RuntimeBinding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, err := r.bindingKeyLocked(identity)
	if err != nil {
		return RuntimeBinding{}, err
	}
	binding := r.bindings[key]
	if actual, observed := r.observed[key]; observed {
		binding.Profile = actual
	}
	return binding, nil
}

// configuredBinding 返回 Framework 启动时登记的期望值，仅供探测身份使用。
// 业务能力检查必须通过 Binding 读取 Runtime 最近一次报告的真实 Profile。
func (r *RuntimeRegistry) configuredBinding(identity string) (RuntimeBinding, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, err := r.bindingKeyLocked(identity)
	if err != nil {
		return RuntimeBinding{}, err
	}
	return r.bindings[key], nil
}

func (r *RuntimeRegistry) observe(identity string, profile RuntimeProfile) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := identity
	if _, ok := r.bindings[key]; !ok {
		keys := r.byProfile[identity]
		if len(keys) != 1 {
			return
		}
		key = keys[0]
	}
	if _, ok := r.bindings[key]; ok {
		profile.AvailabilityKnown = true
		r.observed[key] = profile
	}
}

func (r *RuntimeRegistry) BindingIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

func (r *RuntimeRegistry) DefaultBindingID() (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) != 1 {
		return "", errors.New("存在多个 Runtime installation 时必须明确绑定")
	}
	return r.order[0], nil
}

func (r *RuntimeRegistry) DefaultProfileID() (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.order) != 1 {
		return "", errors.New("存在多个 Runtime profile 时必须明确指定 runtime_profile_id")
	}
	return r.bindings[r.order[0]].Profile.RuntimeProfileID, nil
}

// Compatible 检查场景类型是否能由 profile 加载。空类型仅兼容原型数据。
func (r *RuntimeRegistry) Compatible(profileID, sceneKind string) bool {
	binding, err := r.Binding(profileID)
	if err != nil {
		return false
	}
	if sceneKind == "" {
		return true
	}
	for _, candidate := range binding.Profile.SceneKinds {
		if candidate == sceneKind {
			return true
		}
	}
	return false
}

// runtimeSlot 只串行化同一 profile 的启动和停止。不同 profile 使用不同锁，
// 因此 native、robosuite 与 LIBERO 的健康检查和启动不会互相阻塞。
type runtimeSlot struct {
	mu      sync.Mutex
	process RuntimeProcess
}

// RuntimeSupervisor 管理每个 profile 对应的外部进程。一个 Runtime 进程只
// 运行一个场景，但不同 profile 可以由不同进程并行存在。
type RuntimeSupervisor struct {
	slotsMu      sync.Mutex
	registry     *RuntimeRegistry
	startTimeout time.Duration
	slots        map[string]*runtimeSlot
}

func NewRuntimeSupervisor(registry *RuntimeRegistry) *RuntimeSupervisor {
	slots := make(map[string]*runtimeSlot)
	for _, installationID := range registry.BindingIDs() {
		slots[installationID] = &runtimeSlot{}
	}
	return &RuntimeSupervisor{
		registry: registry, startTimeout: 20 * time.Second, slots: slots,
	}
}

func (s *RuntimeSupervisor) Ensure(ctx context.Context, profileID string) (RuntimeInfo, error) {
	binding, err := s.registry.configuredBinding(profileID)
	if err != nil {
		return RuntimeInfo{}, err
	}
	slot, ok := s.slot(binding.InstallationID, false)
	if !ok {
		return RuntimeInfo{}, fmt.Errorf("%w: Runtime profile 未装配 Supervisor", ErrNotFound)
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	binding, err = s.registry.configuredBinding(binding.InstallationID)
	if err != nil {
		return RuntimeInfo{}, err
	}
	info, actual, probeErr := probeRuntime(ctx, binding)
	if actual.RuntimeProfileID != "" {
		s.registry.observe(profileID, actual)
	}
	if probeErr == nil {
		if slot.process != nil && !slot.process.Alive() {
			slot.process = nil
		}
		info.Managed = slot.process != nil && slot.process.Alive()
		return info, nil
	}
	if binding.Launcher == nil {
		return RuntimeInfo{}, fmt.Errorf("%w: profile %s 探测失败: %v",
			ErrRuntimeUnavailable, profileID, probeErr)
	}
	startedHere := false
	if slot.process == nil || !slot.process.Alive() {
		slot.process, err = binding.Launcher.Start(ctx)
		if err != nil {
			if errors.Is(err, ErrRuntimeUnavailable) {
				return RuntimeInfo{}, err
			}
			return RuntimeInfo{}, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, err)
		}
		startedHere = true
	}
	deadline := time.Now().Add(s.startTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		info, actual, probeErr := probeRuntime(probeCtx, binding)
		cancel()
		if actual.RuntimeProfileID != "" {
			s.registry.observe(profileID, actual)
		}
		if probeErr == nil {
			info.Managed = true
			return info, nil
		}
		lastErr = probeErr
		select {
		case <-ctx.Done():
			cleanupErr := stopStartedRuntime(slot, startedHere)
			return RuntimeInfo{}, errors.Join(ctx.Err(), cleanupErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
	cleanupErr := stopStartedRuntime(slot, startedHere)
	return RuntimeInfo{}, errors.Join(
		fmt.Errorf("%w: profile %s 启动超时: %v", ErrRuntimeUnavailable, profileID, lastErr),
		cleanupErr,
	)
}

// stopStartedRuntime 只清理由本次 Ensure 新启动且尚未通过健康检查的进程。
// 已经存在的外部进程或其他调用建立的受管进程不能因一次探测超时被误杀。
func stopStartedRuntime(slot *runtimeSlot, startedHere bool) error {
	if !startedHere || slot.process == nil {
		return nil
	}
	process := slot.process
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := process.Stop(stopCtx)
	// 仍存活时保留句柄，避免后续把孤儿进程误认成外部 Runtime。
	if (err == nil || !process.Alive()) && slot.process == process {
		slot.process = nil
	}
	if err != nil {
		return fmt.Errorf("清理启动失败的 Runtime 进程失败: %w", err)
	}
	return nil
}

func probeRuntime(ctx context.Context, binding RuntimeBinding) (RuntimeInfo, RuntimeProfile, error) {
	if err := binding.Client.Health(ctx); err != nil {
		return RuntimeInfo{}, RuntimeProfile{}, err
	}
	profiles, err := binding.Client.RuntimeProfiles(ctx)
	if err != nil {
		return RuntimeInfo{}, RuntimeProfile{}, err
	}
	actual, err := matchRuntimeProfile(binding.Profile, profiles)
	if err != nil {
		return RuntimeInfo{}, RuntimeProfile{}, err
	}
	if !actual.Available || !actual.EnvironmentReady {
		reason := actual.UnavailableReason
		if reason == "" {
			reason = "Runtime 环境尚未准备完成"
		}
		return RuntimeInfo{}, actual, fmt.Errorf(
			"%w: profile %s 不可用: %s", ErrRuntimeUnavailable, actual.RuntimeProfileID, reason)
	}
	info, err := binding.Client.Runtime(ctx)
	if err != nil {
		return RuntimeInfo{}, actual, err
	}
	if info.RuntimeProfileID != "" && info.RuntimeProfileID != actual.RuntimeProfileID {
		return RuntimeInfo{}, actual, fmt.Errorf(
			"%w: runtime 响应的 profile 为 %s，期望 %s", ErrRuntimeUnavailable,
			info.RuntimeProfileID, actual.RuntimeProfileID)
	}
	if info.Engine != "" && info.Engine != actual.Engine {
		return RuntimeInfo{}, actual, fmt.Errorf(
			"%w: runtime engine 为 %s，profile 声明为 %s", ErrRuntimeUnavailable,
			info.Engine, actual.Engine)
	}
	if info.APIVersion != "" && info.APIVersion != actual.APIVersion {
		return RuntimeInfo{}, actual, fmt.Errorf(
			"%w: runtime API 为 %s，profile 声明为 %s", ErrRuntimeUnavailable,
			info.APIVersion, actual.APIVersion)
	}
	info.RuntimeProfileID = actual.RuntimeProfileID
	info.Engine = actual.Engine
	info.RuntimeInstallationID = binding.InstallationID
	info.APIVersion = actual.APIVersion
	info.Capabilities = actual.Capabilities
	return info, actual, nil
}

func matchRuntimeProfile(expected RuntimeProfile, profiles []RuntimeProfile) (RuntimeProfile, error) {
	var matched RuntimeProfile
	found := false
	for _, profile := range profiles {
		if profile.RuntimeProfileID != expected.RuntimeProfileID {
			continue
		}
		if found {
			return RuntimeProfile{}, fmt.Errorf(
				"%w: endpoint 重复声明 profile %s", ErrRuntimeUnavailable, expected.RuntimeProfileID)
		}
		matched = profile
		found = true
	}
	if !found {
		return RuntimeProfile{}, fmt.Errorf(
			"%w: endpoint 未提供 profile %s", ErrRuntimeUnavailable, expected.RuntimeProfileID)
	}
	if matched.Engine == "" || matched.Loader == "" || matched.APIVersion == "" {
		return RuntimeProfile{}, fmt.Errorf(
			"%w: profile %s 缺少 engine、loader 或 api_version", ErrRuntimeUnavailable, matched.RuntimeProfileID)
	}
	if err := validateConfiguredProfileField("engine", expected.Engine, matched.Engine); err != nil {
		return RuntimeProfile{}, err
	}
	if err := validateConfiguredProfileField("loader", expected.Loader, matched.Loader); err != nil {
		return RuntimeProfile{}, err
	}
	if err := validateConfiguredProfileField("api_version", expected.APIVersion, matched.APIVersion); err != nil {
		return RuntimeProfile{}, err
	}
	return matched, nil
}

func validateConfiguredProfileField(field, expected, actual string) error {
	if expected != "" && expected != actual {
		return fmt.Errorf(
			"%w: profile %s 配置为 %s，Runtime 报告为 %s",
			ErrRuntimeUnavailable, field, expected, actual)
	}
	return nil
}

func (s *RuntimeSupervisor) Probe(ctx context.Context, profileID string) (RuntimeInfo, error) {
	binding, err := s.registry.configuredBinding(profileID)
	if err != nil {
		return RuntimeInfo{}, err
	}
	info, actual, err := probeRuntime(ctx, binding)
	if actual.RuntimeProfileID != "" {
		s.registry.observe(profileID, actual)
	}
	if err != nil {
		return RuntimeInfo{}, err
	}
	if slot, _ := s.slot(profileID, false); slot != nil {
		slot.mu.Lock()
		info.Managed = slot.process != nil && slot.process.Alive()
		slot.mu.Unlock()
	}
	return info, nil
}

// StopManaged 只停止由本 Supervisor 启动的进程。外部 endpoint 的 slot 没有
// process，因此场景 stop 不会关闭用户单独启动或共享的 Runtime。
func (s *RuntimeSupervisor) StopManaged(ctx context.Context, profileID string) (bool, error) {
	slot, ok := s.slot(profileID, false)
	if !ok {
		return false, fmt.Errorf("%w: Runtime profile 未装配 Supervisor", ErrNotFound)
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	process := slot.process
	if process == nil {
		return false, nil
	}
	if !process.Alive() {
		// 子进程已经异常退出时只清除受管句柄。再次调用 Stop 可能返回旧的
		// wait/exit 错误，反而阻止 Project 恢复并重新启动一个 Runtime。
		slot.process = nil
		return true, nil
	}
	err := process.Stop(ctx)
	// Stop 返回错误但进程已经退出时也必须清掉句柄；仍存活时保留 slot，
	// 使后续 Close 可以再次回收，而不是把残留进程误认成外部 endpoint。
	if err == nil || !process.Alive() {
		if slot.process == process {
			slot.process = nil
		}
	}
	if err != nil {
		return true, fmt.Errorf("停止受管 Runtime profile %s 失败: %w", profileID, err)
	}
	return true, nil
}

func (s *RuntimeSupervisor) Close(ctx context.Context) error {
	var result error
	s.slotsMu.Lock()
	ids := make([]string, 0, len(s.slots))
	for id := range s.slots {
		ids = append(ids, id)
	}
	s.slotsMu.Unlock()
	for _, profileID := range ids {
		if _, err := s.StopManaged(ctx, profileID); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (s *RuntimeSupervisor) slot(id string, create bool) (*runtimeSlot, bool) {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	slot, ok := s.slots[id]
	if !ok && create {
		slot = &runtimeSlot{}
		s.slots[id] = slot
		ok = true
	}
	return slot, ok
}
