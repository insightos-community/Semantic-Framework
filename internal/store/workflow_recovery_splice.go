package store

import (
	"database/sql"
	"fmt"
	"sort"
)

// retainedRecoverySteps reuses pending records, never their old execution data.
// Only the edges crossing the repaired boundary change. Completed predecessors
// and edges within the retained graph keep their exact identity.
func retainedRecoverySteps(tx *sql.Tx, taskID, failedID string, ids []string, prefix []SubTaskDraft) ([]SubTask, map[string][]string, error) {
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || keep[id] {
			return nil, nil, fmt.Errorf("invalid retained step %q: %w", id, ErrInvalidState)
		}
		keep[id] = true
	}
	if len(keep) == 0 {
		return nil, nil, nil
	}
	items, err := listSubTasksQuery(tx, taskID)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[string]SubTask, len(items))
	var retained []SubTask
	for _, item := range items {
		byID[item.ID] = item
		if keep[item.ID] {
			if item.Status != TaskStatusPending || item.ExecutionRef != "" {
				return nil, nil, ErrInvalidState
			}
			retained = append(retained, item)
		}
	}
	if len(retained) != len(keep) {
		return nil, nil, ErrInvalidState
	}
	// Repair dependencies reference only the repair graph. Reusing an old ID
	// would make a retained edge ambiguous, so require new IDs for this form.
	leaves := make(map[string]bool, len(prefix))
	for _, item := range prefix {
		if _, exists := byID[item.ID]; exists {
			return nil, nil, fmt.Errorf("repair ID %q already exists: %w", item.ID, ErrInvalidState)
		}
		leaves[item.ID] = true
	}
	for _, item := range prefix {
		for _, dep := range item.DependsOn {
			delete(leaves, dep)
		}
	}
	deps := make(map[string][]string, len(keep))
	rows, err := tx.Query(`SELECT subtask_id,depends_on_subtask_id FROM subtask_dependencies WHERE task_id=?`, taskID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var id, dep string
		if err := rows.Scan(&id, &dep); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if keep[id] {
			deps[id] = append(deps[id], dep)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	for _, item := range retained {
		result := make(map[string]bool)
		hasRetainedParent, crossesRepair := false, false
		for _, dep := range deps[item.ID] {
			parent, exists := byID[dep]
			if !exists {
				return nil, nil, ErrInvalidState
			}
			switch {
			case keep[dep]:
				result[dep] = true
				hasRetainedParent = true
			case parent.Status == TaskStatusCompleted:
				result[dep] = true
			case dep == failedID || parent.Status == TaskStatusPending:
				crossesRepair = true
			default:
				return nil, nil, fmt.Errorf("retained step %q depends on unavailable %q: %w", item.ID, dep, ErrInvalidState)
			}
		}
		if crossesRepair || !hasRetainedParent {
			for leaf := range leaves {
				result[leaf] = true
			}
		}
		deps[item.ID] = nil
		for dep := range result {
			deps[item.ID] = append(deps[item.ID], dep)
		}
		sort.Strings(deps[item.ID])
	}
	return retained, deps, nil
}
