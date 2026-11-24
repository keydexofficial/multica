package claudemanagedruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type event struct {
	ToolUseID string `json:"tool_use_id"`
	ID        string `json:"id"`
	// ProcessedAt is null for a sent event still queued.
	ProcessedAt string `json:"processed_at"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Content     []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Input      map[string]any `json:"input"`
	StopReason struct {
		Type string `json:"type"`
	} `json:"stop_reason"`
	Error struct {
		Message     string `json:"message"`
		RetryStatus struct {
			Type string `json:"type"`
		} `json:"retry_status"`
	} `json:"error"`
}

func (e event) text() string {
	var parts []string
	for _, c := range e.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}
func (b *Bridge) events(ctx context.Context, r *run) error {
	// Without a page cursor, list from the last processed event and skip every
	// event up to and including Cursor: events can share a timestamp. A page
	// cursor is sent with the filters of the request that returned it.
	fetch := func(applyTimeFilter bool) (fresh []event, nextPage, nextPageFrom string, missing bool, err error) {
		q := url.Values{"limit": {"100"}, "order": {"asc"}}
		skipping := r.Page == "" && r.Cursor != ""
		if r.Page != "" {
			q.Set("page", r.Page)
			if r.PageFrom != "" {
				q.Set("created_at[gte]", r.PageFrom)
			}
		} else if applyTimeFilter && r.CursorAt != "" {
			q.Set("created_at[gte]", r.CursorAt)
		}
		for {
			var page struct {
				Data     []event `json:"data"`
				NextPage string  `json:"next_page"`
			}
			if err := b.anthropic.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(r.SessionID)+"/events?"+q.Encode(), nil, &page); err != nil {
				var ae *apiError
				if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
					return nil, "", "", false, b.reject(ctx, r, b.profile.DisplayName+" session no longer exists")
				}
				return nil, "", "", false, err
			}
			fresh = page.Data
			if skipping {
				fresh = nil
				for i, e := range page.Data {
					if e.ID == r.Cursor {
						fresh, skipping = page.Data[i+1:], false
						break
					}
				}
			}
			if !skipping {
				if page.NextPage != "" {
					nextPageFrom = q.Get("created_at[gte]")
				}
				return fresh, page.NextPage, nextPageFrom, false, nil
			}
			if page.NextPage == "" {
				return nil, "", "", true, nil
			}
			if page.NextPage == q.Get("page") {
				return nil, "", "", false, fmt.Errorf("%s pagination cursor did not advance", b.profile.DisplayName)
			}
			q.Set("page", page.NextPage)
		}
	}
	fresh, nextPage, nextPageFrom, missing, err := fetch(true)
	if err != nil {
		return err
	}
	if missing && r.Cursor != "" {
		// created_at may differ from processed_at (self-hosted providers keep
		// both), so the time filter can hide the cursor event. Retry once
		// without it instead of wedging on the same filtered scan every tick.
		fresh, nextPage, nextPageFrom, missing, err = fetch(false)
		if err != nil {
			return err
		}
	}
	if missing {
		return fmt.Errorf("%s event %s is missing from session %s history", b.profile.DisplayName, r.Cursor, r.SessionID)
	}
	for _, e := range fresh {
		if e.ID == "" {
			return fmt.Errorf("%s event has no ID", b.profile.DisplayName)
		}
		if e.ID == r.Cursor {
			continue
		}
		// A new session starts at creation. A continued session's history holds
		// earlier runs, so its run starts at the user message carrying the prompt.
		if !r.Started && e.Type == "user.message" && e.text() == r.Prompt {
			r.Started = true
			r.Phase = "running"
		}
		if !r.Started {
			r.Cursor, r.CursorAt = e.ID, e.ProcessedAt
			r.Page, r.PageFrom = "", ""
			continue
		}
		msg := map[string]any{}
		switch e.Type {
		case "agent.message":
			r.resume()
			msg["type"] = "text"
			msg["content"] = e.text()
			r.Output = e.text()
		case "agent.tool_use", "agent.mcp_tool_use", "agent.custom_tool_use":
			r.resume()
			msg["type"] = "tool_use"
			msg["call_id"] = e.ID
			msg["tool"] = e.Name
			msg["input"] = e.Input
		case "agent.tool_result", "agent.mcp_tool_result", "agent.custom_tool_result":
			r.resume()
			msg["type"] = "tool_result"
			if e.ToolUseID != "" {
				msg["call_id"] = e.ToolUseID
			}
			msg["output"] = e.text()
		case "session.status_running", "session.thread_status_running":
			r.resume()
		case "session.error":
			msg["type"] = "error"
			msg["content"] = e.Error.Message
			if e.Error.RetryStatus.Type != "retrying" {
				r.Failure = e.Error.Message
			}
		case "session.status_idle":
			if e.StopReason.Type == "end_turn" && r.Failure == "" {
				r.Phase = "idle"
				r.IdleSince = time.Now()
			} else {
				r.Phase = "final"
				r.Outcome = "failed"
				if r.Failure == "" {
					r.Failure = b.profile.DisplayName + " stopped: " + e.StopReason.Type
				}
			}
		case "session.status_terminated", "session.deleted":
			r.Phase = "final"
			r.Outcome = "failed"
			r.Failure = b.profile.DisplayName + " session terminated"
		}
		if len(msg) > 0 {
			msg["seq"] = r.Seq + 1
			msg["idempotency_key"] = "claude:" + e.ID
			if err := b.taskCall(ctx, r, "/messages", map[string]any{"messages": []map[string]any{msg}}, nil); err != nil {
				return err
			}
			r.Seq++
		}
		r.Cursor, r.CursorAt = e.ID, e.ProcessedAt
		r.Page, r.PageFrom = "", ""
		if err := b.persist(); err != nil {
			return err
		}
		if r.Phase == "final" {
			return b.deliver(ctx, r)
		}
	}
	r.Page, r.PageFrom = "", ""
	if nextPage != "" {
		r.Page, r.PageFrom = nextPage, nextPageFrom
	}
	if err := b.persist(); err != nil {
		return err
	}
	if r.Phase == "sending" {
		return fmt.Errorf("submission not yet confirmed; inspect %s session %s before any manual resubmission", b.profile.DisplayName, r.SessionID)
	}
	return nil
}

// latestEventID returns the newest event's ID. A continued session already
// holds earlier turns, so the bridge records this cursor before submitting the
// next message; otherwise an identical earlier user.message would be mistaken
// for this run's start and its idle event would complete the run early. Only
// the ID is recorded: the provider's created_at may differ from its
// processed_at, so a processed_at-based created_at[gte] filter can hide the
// cursor event.
func (b *Bridge) latestEventID(ctx context.Context, sessionID string) (string, error) {
	q := url.Values{"limit": {"1"}, "order": {"desc"}}
	var page struct {
		Data []event `json:"data"`
	}
	if err := b.anthropic.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(sessionID)+"/events?"+q.Encode(), nil, &page); err != nil {
		return "", err
	}
	if len(page.Data) == 0 {
		return "", nil
	}
	if page.Data[0].ID == "" {
		return "", fmt.Errorf("%s event has no ID", b.profile.DisplayName)
	}
	return page.Data[0].ID, nil
}
