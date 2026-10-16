package robot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Select original result values without reinterpreting coordinates, packet
// digests or timestamps. JSON Pointer addresses objects and array indexes.
func SelectResultPaths(result any, paths []string) (map[string]any, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	values := make(map[string]any, len(paths))
	for _, path := range paths {
		current := root
		if path != "" {
			if !strings.HasPrefix(path, "/") {
				return nil, fmt.Errorf("result path must be a JSON Pointer: %q", path)
			}
			for _, token := range strings.Split(path[1:], "/") {
				for i := 0; i < len(token); i++ {
					if token[i] == '~' {
						if i+1 >= len(token) || (token[i+1] != '0' && token[i+1] != '1') {
							return nil, fmt.Errorf("invalid JSON Pointer escape: %q", path)
						}
						i++
					}
				}
				token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
				switch value := current.(type) {
				case map[string]any:
					var exists bool
					current, exists = value[token]
					if !exists {
						keys := make([]string, 0, len(value))
						for key := range value {
							keys = append(keys, key)
						}
						sort.Strings(keys)
						if len(keys) > 24 {
							keys = append(keys[:24], "...")
						}
						return nil, fmt.Errorf("result path not found: %q; available fields at the missing segment: %v; paths are relative to execution.result (omit result_paths to read input/result)", path, keys)
					}
				case []any:
					index, err := strconv.Atoi(token)
					if err != nil || index < 0 || index >= len(value) || strconv.Itoa(index) != token {
						return nil, fmt.Errorf("invalid result array index: %q", path)
					}
					current = value[index]
				default:
					return nil, fmt.Errorf("result path not found: %q", path)
				}
			}
		}
		values[path] = current
	}
	return values, nil
}
