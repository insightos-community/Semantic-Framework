package robot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func (s *Service) resolveInputBindings(request RunRequest) (map[string]any, error) {
	// Clone through the same JSON representation used by Skill input validation.
	// Never alter stored results or the caller's partial input while binding.
	root, err := SelectResultPaths(request.Input, []string{""})
	if err != nil {
		return nil, err
	}
	input, ok := root[""].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("input_bindings requires an input object")
	}
	for _, binding := range request.InputBindings {
		source, err := s.st.GetRobotExecution(binding.ExecutionID)
		if err != nil || source.ProjectID != request.ProjectID || source.RobotID != request.RobotID {
			return nil, fmt.Errorf("binding source %q is outside this Project/Robot", binding.ExecutionID)
		}
		if source.Status != "completed" {
			return nil, fmt.Errorf("binding source %q is not completed", binding.ExecutionID)
		}
		value, err := SelectResultPaths(source.Result, []string{binding.ResultPath})
		if err != nil {
			return nil, err
		}
		if err := setInputPath(input, binding.InputPath, value[binding.ResultPath]); err != nil {
			return nil, err
		}
	}
	// Preserve the ordinary map/float representation expected by existing
	// validators, digest canonicalization and SDK transport.
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var normalized map[string]any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func setInputPath(input map[string]any, path string, value any) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("input_path must address an input field: %q", path)
	}
	tokens := strings.Split(path[1:], "/")
	var current any = input
	for index, raw := range tokens {
		for i := 0; i < len(raw); i++ {
			if raw[i] == '~' {
				if i+1 >= len(raw) || (raw[i+1] != '0' && raw[i+1] != '1') {
					return fmt.Errorf("invalid input_path escape: %q", path)
				}
				i++
			}
		}
		key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		last := index == len(tokens)-1
		switch node := current.(type) {
		case map[string]any:
			if last {
				node[key] = value
				return nil
			}
			if _, exists := node[key]; !exists {
				node[key] = map[string]any{}
			}
			current = node[key]
		case []any:
			position, err := strconv.Atoi(key)
			if err != nil || position < 0 || position >= len(node) || strconv.Itoa(position) != key {
				return fmt.Errorf("invalid input_path array index: %q", path)
			}
			if last {
				node[position] = value
				return nil
			}
			current = node[position]
		default:
			return fmt.Errorf("input_path crosses a scalar: %q", path)
		}
	}
	return nil
}
