package simulation

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// 安装前检查整个同 Profile 的安装实例，避免依赖替换或 smoke 测试影响活动场景。
// 安装和目录刷新由同一安装队列串行执行；运行中的安装必须先由用户停止。
func (s *Service) CheckRuntimeInstallIdle(ctx context.Context, profileID string) error {
	for _, item := range s.installations.List() {
		if item.Profile.RuntimeProfileID != profileID {
			continue
		}
		slot, _ := s.supervisor.slot(item.InstallationID, true)
		slot.mu.Lock()
		alive := slot.process != nil && slot.process.Alive()
		slot.mu.Unlock()
		if alive {
			return fmt.Errorf("Runtime %s 正在运行，请先停止后安装", item.InstallationID)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		info, err := item.Binding().Client.Runtime(probeCtx)
		cancel()
		if err == nil && info.ActiveInstanceID != "" {
			return fmt.Errorf("Runtime %s 仍有活动场景，请先停止场景", item.InstallationID)
		}
	}
	return ctx.Err()
}

// ReloadResources 在既有对象上更新目录，已有 Project/Viewer 继续持有原来的
// Service。正在运行的 Runtime 保留其客户端和进程；新安装可立即使用，变更
// 已运行安装时明确要求先停止该 Runtime，避免把旧场景切到新的进程配置。
func (s *Service) ReloadResources(ctx context.Context, runtimeDir, sceneDir string) error {
	installations, err := LoadRuntimeInstallations(runtimeDir)
	if err != nil {
		return err
	}
	scenes, err := LoadSceneCatalog(sceneDir)
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	changed := map[string]bool{}
	for _, item := range installations.List() {
		old, oldErr := s.installations.Get(item.InstallationID)
		_, bindingErr := s.registry.configuredBinding(item.InstallationID)
		if oldErr == nil && reflect.DeepEqual(old, item) && (bindingErr == nil) == item.Enabled {
			continue
		}
		changed[item.InstallationID] = true
	}
	for _, old := range s.installations.List() {
		if _, err := installations.Get(old.InstallationID); err != nil {
			changed[old.InstallationID] = true
		}
	}
	ids := []string{}
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		old, oldErr := s.installations.Get(id)
		slot, _ := s.supervisor.slot(id, true)
		slot.mu.Lock()
		defer slot.mu.Unlock()
		if slot.process != nil && slot.process.Alive() {
			return fmt.Errorf("Runtime %s 正在运行，请停止后刷新安装", id)
		}
		if oldErr == nil {
			info, probeErr := old.Binding().Client.Runtime(ctx)
			if probeErr == nil && info.ActiveInstanceID != "" {
				return fmt.Errorf("Runtime %s 仍有活动场景", id)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.registry.mu.Lock()
	for _, id := range ids {
		item, err := installations.Get(id)
		if err == nil && item.Enabled {
			s.registry.bindings[id] = item.Binding()
		} else {
			delete(s.registry.bindings, id)
		}
		delete(s.registry.observed, id)
	}
	s.registry.reindexLocked()
	s.registry.mu.Unlock()
	// 完整目录解析成功才替换可见索引；已发布 Project Layout 独立保存，刷新时保留。
	s.installations.mu.Lock()
	s.installations.items, s.installations.order = installations.items, installations.order
	s.installations.mu.Unlock()
	s.sceneCatalog.mu.Lock()
	for id, item := range s.sceneCatalog.items {
		if strings.HasPrefix(item.Source, "project:") {
			scenes.items[id] = item
		}
	}
	for id, doc := range s.sceneCatalog.authoringDocuments {
		if _, ok := scenes.authoringDocuments[id]; !ok {
			scenes.authoringDocuments[id] = doc
		}
	}
	scenes.order = nil
	for id := range scenes.items {
		scenes.order = append(scenes.order, id)
	}
	sort.Strings(scenes.order)
	s.sceneCatalog.items, s.sceneCatalog.order, s.sceneCatalog.version = scenes.items, scenes.order, scenes.version
	s.sceneCatalog.authoringDocuments = scenes.authoringDocuments
	s.sceneCatalog.mu.Unlock()
	return nil
}

func (r *RuntimeRegistry) reindexLocked() {
	r.order = nil
	r.byProfile = map[string][]string{}
	for id, b := range r.bindings {
		r.order = append(r.order, id)
		r.byProfile[b.Profile.RuntimeProfileID] = append(r.byProfile[b.Profile.RuntimeProfileID], id)
	}
	sort.Strings(r.order)
}
