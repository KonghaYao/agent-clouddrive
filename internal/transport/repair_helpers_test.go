package transport

import (
	"encoding/json"

	"github.com/agentclouddrive/agent-clouddrive/internal/application"
)

func repairStr(m application.Object, key string) string {
	v, _ := m[key].(string)
	return v
}

func repairNum(m application.Object, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}
