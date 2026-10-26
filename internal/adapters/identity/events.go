package identity

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

// IdentityEvents 以保留的事件 ID 为连续性锚点；不使用时间采样猜测停用历史。
func (k *Keycloak) IdentityEvents(ctx context.Context, issuer, anchor string) (ports.IdentityEventBatch, error) {
	var out ports.IdentityEventBatch
	if issuer != k.cfg.Issuer {
		return out, ports.ErrUnauthorized
	}
	token, e := k.serviceToken(ctx)
	if e != nil {
		return out, e
	}
	var config struct {
		Enabled bool `json:"adminEventsEnabled"`
		Details bool `json:"adminEventsDetailsEnabled"`
	}
	base := strings.TrimRight(k.cfg.AdminURL, "/")
	if e = k.request(ctx, "GET", base+"/events/config", token, nil, &config); e != nil {
		return out, e
	}
	if !config.Enabled || !config.Details {
		return out, ports.ErrUnverified
	}
	seen := map[string]bool{}
	var anchorTime int64
	for first := 0; first < 10000; first += 100 {
		var page []struct {
			ID             string `json:"id"`
			Time           int64  `json:"time"`
			Operation      string `json:"operationType"`
			Resource       string `json:"resourceType"`
			Path           string `json:"resourcePath"`
			Representation string `json:"representation"`
			Error          string `json:"error"`
		}
		endpoint := base + "/admin-events?first=" + strconv.Itoa(first) + "&max=100&direction=desc"
		if e = k.request(ctx, "GET", endpoint, token, nil, &page); e != nil {
			return out, e
		}
		for _, event := range page {
			if event.ID == "" || event.Time <= 0 {
				return out, ports.ErrIntegrity
			}
			if seen[event.ID] {
				continue
			}
			seen[event.ID] = true
			if out.Anchor == "" {
				out.Anchor = event.ID
			}
			if event.ID == anchor {
				out.Continuous = true
				anchorTime = event.Time
			}
			item := ports.IdentityEvent{ID: event.ID, Time: time.UnixMilli(event.Time).UTC()}
			// realm 配置变化可能包含事件记录的暂停/恢复，不能沿用旧分享。
			if event.Error == "" && event.Resource == "REALM" && event.Operation == "UPDATE" {
				item.Disable = true
			}
			if event.Error == "" && event.Resource == "USER" && strings.HasPrefix(event.Path, "users/") {
				subject, err := url.PathUnescape(strings.TrimPrefix(event.Path, "users/"))
				if err != nil {
					return out, ports.ErrIntegrity
				}
				if strings.Contains(subject, "/") {
					continue
				}
				if !safeSubject(subject) {
					return out, ports.ErrIntegrity
				}
				item.Subject = subject
				if event.Operation == "DELETE" {
					item.Disable = true
				}
				if event.Operation == "UPDATE" {
					var user struct {
						Enabled *bool `json:"enabled"`
					}
					// 缺失 representation 不能证明未停用，按停用保守处理。
					item.Disable = json.Unmarshal([]byte(event.Representation), &user) != nil || user.Enabled == nil || !*user.Enabled
				}
			}
			out.Events = append(out.Events, item)
		}
		// 跨过锚点所在毫秒后才停止；同毫秒事件顺序不稳定，需完整去重。
		if anchorTime > 0 && len(page) > 0 && page[len(page)-1].Time < anchorTime {
			return out, nil
		}
		if len(page) < 100 {
			// 空日志没有可持久验证的锚点。要求管理员产生一个保留事件后再确认分享，
			// 否则 empty→停用→启用→清空 可能在两次采样之间不可见。
			if out.Anchor == "" {
				return out, ports.ErrUnverified
			}
			return out, nil
		}
	}
	// 超出有界补拉窗口不能证明连续性；提升 epoch 撤销全部旧分享，
	// 将当前头作为新边界，避免在大历史 realm 中永久阻塞新确认。
	out.Continuous = false
	return out, nil
}
