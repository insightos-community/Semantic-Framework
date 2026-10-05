package pilot

import "sync"

// Catalog 只接受 Robot profile 中的精确 Action 映射，不提供按名称选首个实例的接口。
type Catalog struct {
	mu       sync.RWMutex
	profiles map[string]RobotProfile
}

func NewCatalog(profiles ...RobotProfile) *Catalog {
	catalog := &Catalog{profiles: make(map[string]RobotProfile, len(profiles))}
	for _, profile := range profiles {
		catalog.Upsert(profile)
	}
	return catalog
}

func (c *Catalog) Upsert(profile RobotProfile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copyProfile := RobotProfile{RobotID: profile.RobotID, Bindings: make(map[string]AbilityBinding, len(profile.Bindings))}
	for key, binding := range profile.Bindings {
		copyProfile.Bindings[key] = binding
	}
	c.profiles[profile.RobotID] = copyProfile
}

// Replace 由 AbilityFramework 发现器在一次完整轮询成功后调用。它会一次性
// 替换某台 Robot 的绑定，避免已经离线的 Ability 实例继续留在目录中。
func (c *Catalog) Replace(profile RobotProfile) {
	c.Upsert(profile)
}

func (c *Catalog) Resolve(robotID string, action ActionRef) (AbilityBinding, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	profile, ok := c.profiles[robotID]
	if !ok {
		return AbilityBinding{}, ErrRobotNotFound
	}
	binding, ok := profile.Bindings[action.Key()]
	if !ok || binding.InstanceID == "" {
		// 区分“动作完全没绑定”和“同动作版本不匹配”，便于部署人员定位配置。
		for _, candidate := range profile.Bindings {
			if candidate.Action.Type == action.Type {
				return AbilityBinding{}, ErrInterfaceMismatch
			}
		}
		return AbilityBinding{}, ErrActionNotBound
	}
	return binding, nil
}

func (c *Catalog) ValidateRequired(robotID string, actions []ActionRef) error {
	for _, action := range actions {
		if _, err := c.Resolve(robotID, action); err != nil {
			return err
		}
	}
	return nil
}
