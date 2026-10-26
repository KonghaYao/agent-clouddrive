package domain

import "sort"

type BatchStatus string

const (
	BatchPending           BatchStatus = "pending"
	BatchRunning           BatchStatus = "running"
	BatchSucceeded         BatchStatus = "succeeded"
	BatchFailed            BatchStatus = "failed"
	BatchBlockedDependency BatchStatus = "blocked_dependency"
)

type BatchItem struct {
	ID          string      `json:"item_id"`
	DependsOn   []string    `json:"depends_on,omitempty"`
	OperationID string      `json:"operation_id,omitempty"`
	Status      BatchStatus `json:"status"`
	ErrorCode   Code        `json:"error_code,omitempty"`
}

// ScheduleBatch returns ready independent work and pending descendants of failed
// items. No successful independent item is rolled back. Persist each returned
// transition separately and retain per-item idempotency keys on retries.
func ScheduleBatch(items []BatchItem) (ready []string, blocked []string, err error) {
	ready = []string{}
	blocked = []string{}
	if len(items) > 1000 {
		return nil, nil, NewError(InvalidArgument, "batch exceeds 1000 items")
	}
	byID := map[string]BatchItem{}
	indegree := map[string]int{}
	children := map[string][]string{}
	for _, item := range items {
		if item.ID == "" {
			return nil, nil, NewError(InvalidArgument, "batch item ID is required")
		}
		if _, ok := byID[item.ID]; ok {
			return nil, nil, NewError(InvalidArgument, "duplicate batch item ID")
		}
		switch item.Status {
		case BatchPending, BatchRunning, BatchSucceeded, BatchFailed, BatchBlockedDependency:
		default:
			return nil, nil, NewError(InvalidArgument, "invalid batch status")
		}
		byID[item.ID] = item
	}
	for _, item := range items {
		seen := map[string]bool{}
		for _, dep := range item.DependsOn {
			if dep == item.ID || seen[dep] {
				return nil, nil, NewError(InvalidArgument, "self or duplicate dependency")
			}
			if _, ok := byID[dep]; !ok {
				return nil, nil, NewError(InvalidArgument, "unknown batch dependency")
			}
			seen[dep] = true
			children[dep] = append(children[dep], item.ID)
			indegree[item.ID]++
		}
	}
	queue := []string{}
	for _, item := range items {
		if indegree[item.ID] == 0 {
			queue = append(queue, item.ID)
		}
	}
	order := []string{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if len(order) != len(items) {
		return nil, nil, NewError(InvalidArgument, "cyclic batch dependency")
	}
	failed := map[string]bool{}
	for _, id := range order {
		item := byID[id]
		failure := item.Status == BatchFailed || item.Status == BatchBlockedDependency
		allSucceeded := true
		for _, dep := range item.DependsOn {
			if failed[dep] {
				failure = true
			}
			if byID[dep].Status != BatchSucceeded {
				allSucceeded = false
			}
		}
		failed[id] = failure
		if item.Status != BatchPending {
			continue
		}
		if failure {
			blocked = append(blocked, id)
		} else if allSucceeded {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	sort.Strings(blocked)
	return ready, blocked, nil
}
