package channel

import (
	"context"
	"errors"
	"fmt"

	"github.com/432539/gpt2api/internal/upstream/adapter"
)

// Router 负责"本地模型 + 模态 → 可用的 (Channel, Mapping, Adapter) 候选列表"。
//
// 调用方按顺序尝试候选,失败时由上层把结果通过 Service.MarkHealth 回写。
type Router struct {
	svc *Service
}

func NewRouter(svc *Service) *Router { return &Router{svc: svc} }

// Svc 暴露底层 Service,让调用方可以做 MarkHealth 等附加动作。
func (r *Router) Svc() *Service { return r.svc }

// Route 是单个路由候选。
type Route struct {
	Channel       *Channel
	Mapping       *Mapping
	UpstreamModel string
	Adapter       adapter.Adapter
}

var ErrNoRoute = errors.New("channel: no route")

// Resolve 返回对应本地模型 + 模态的候选路由(按优先级排序)。
// 当列表为空时返回 ErrNoRoute,调用方应回退到内置 ChatGPT 账号池。
func (r *Router) Resolve(ctx context.Context, localModel, modality string) ([]*Route, error) {
	mws, err := r.svc.dao.ResolveByLocalModel(ctx, localModel, modality)
	if err != nil {
		return nil, err
	}
	if len(mws) == 0 {
		return nil, ErrNoRoute
	}
	routes := make([]*Route, 0, len(mws))
	for _, mw := range mws {
		c := mw.Channel
		// 跳过不健康(连续失败 ≥3 次)渠道,除非没有其它选择。
		// 这里先都塞进来,由上层 try 循环决定。
		key, err := r.svc.DecryptKey(&c)
		if err != nil {
			continue
		}
		ad, err := adapter.New(c.Type, adapter.Params{
			BaseURL: c.BaseURL, APIKey: key, TimeoutS: c.TimeoutS,
			Extra: func() string {
				if c.Extra.Valid {
					return c.Extra.String
				}
				return ""
			}(),
		})
		if err != nil {
			continue
		}
		m := mw.Mapping
		routes = append(routes, &Route{
			Channel: &c, Mapping: &m,
			UpstreamModel: m.UpstreamModel,
			Adapter:       ad,
		})
	}
	if len(routes) == 0 {
		return nil, ErrNoRoute
	}
	// 先把 healthy 放前面,unhealthy 放最后(保留 fallback)。
	sorted := make([]*Route, 0, len(routes))
	var tail []*Route
	for _, r := range routes {
		if r.Channel.Status == StatusUnhealthy {
			tail = append(tail, r)
		} else {
			sorted = append(sorted, r)
		}
	}
	sorted = append(sorted, tail...)
	return sorted, nil
}

// BuildAdapter 给"测试连接"等场景用,直接按渠道 ID 构造 adapter。
func (r *Router) BuildAdapter(ctx context.Context, channelID uint64) (adapter.Adapter, *Channel, error) {
	c, err := r.svc.dao.GetByID(ctx, channelID)
	if err != nil {
		return nil, nil, err
	}
	key, err := r.svc.DecryptKey(c)
	if err != nil {
		return nil, nil, fmt.Errorf("decrypt key: %w", err)
	}
	ad, err := adapter.New(c.Type, adapter.Params{
		BaseURL: c.BaseURL, APIKey: key, TimeoutS: c.TimeoutS,
		Extra: func() string {
			if c.Extra.Valid {
				return c.Extra.String
			}
			return ""
		}(),
	})
	if err != nil {
		return nil, nil, err
	}
	return ad, c, nil
}
