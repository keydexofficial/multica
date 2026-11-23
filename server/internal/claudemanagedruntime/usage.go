package claudemanagedruntime

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
)

type UsageMode string

const UsageSession UsageMode = "session"

type sessionUsage struct {
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
	CacheCreation        struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	ListCost struct {
		Amount   any    `json:"amount"`
		Currency string `json:"currency"`
	} `json:"list_cost"`
}

func (u sessionUsage) cacheWriteTokens() int64 {
	return u.CacheCreation.Ephemeral5m + u.CacheCreation.Ephemeral1h
}

func (u sessionUsage) costUSD() float64 {
	switch v := u.ListCost.Amount.(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func (u sessionUsage) costTicks() int64 {
	cost := u.costUSD()
	if cost <= 0 {
		return 0
	}
	return int64(math.Round(cost * 1e8))
}

func (u sessionUsage) empty() bool {
	return u.InputTokens <= 0 && u.OutputTokens <= 0 && u.CacheReadInputTokens <= 0 && u.cacheWriteTokens() <= 0 && u.costTicks() <= 0
}

func usageDelta(total, base sessionUsage) sessionUsage {
	positive := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		return v
	}
	delta := sessionUsage{
		InputTokens:          positive(total.InputTokens - base.InputTokens),
		OutputTokens:         positive(total.OutputTokens - base.OutputTokens),
		CacheReadInputTokens: positive(total.CacheReadInputTokens - base.CacheReadInputTokens),
	}
	delta.CacheCreation.Ephemeral5m = positive(total.CacheCreation.Ephemeral5m - base.CacheCreation.Ephemeral5m)
	delta.CacheCreation.Ephemeral1h = positive(total.CacheCreation.Ephemeral1h - base.CacheCreation.Ephemeral1h)
	delta.ListCost.Currency = total.ListCost.Currency
	delta.ListCost.Amount = math.Max(0, total.costUSD()-base.costUSD())
	return delta
}

func (b *Bridge) reportUsage(ctx context.Context, r *run) error {
	if b.profile.Usage != UsageSession || r.SessionID == "" || r.UsageReported {
		return nil
	}
	markReported := func() error {
		r.UsageReported = true
		return b.persist()
	}
	var session struct {
		Usage *sessionUsage `json:"usage"`
	}
	if err := b.anthropic.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(r.SessionID), nil, &session); err != nil {
		if definitelyRejected(err) {
			return markReported()
		}
		return err
	}
	if session.Usage == nil {
		return markReported()
	}
	delta := usageDelta(*session.Usage, r.UsageBase)
	if delta.empty() {
		return markReported()
	}
	payload := map[string]any{
		"usage": []map[string]any{{
			"provider":           b.profile.Provider,
			"model":              "",
			"input_tokens":       delta.InputTokens,
			"output_tokens":      delta.OutputTokens,
			"cache_read_tokens":  delta.CacheReadInputTokens,
			"cache_write_tokens": delta.cacheWriteTokens(),
			"cost_usd_ticks":     delta.costTicks(),
		}},
	}
	if err := b.taskCall(ctx, r, "/usage", payload, nil); err != nil {
		if !definitelyRejected(err) {
			return err
		}
		return markReported()
	}
	return markReported()
}
