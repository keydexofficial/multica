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
		Type     string   `json:"type"`
		EventIDs []string `json:"event_ids"`
	} `json:"stop_reason"`
	SessionThreadID string `json:"session_thread_id"`
	Error           struct {
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
			if e.Type == "agent.custom_tool_use" && b.profile.CustomTools && e.SessionThreadID == "" {
				// The session pauses until a result arrives. Record the call and
				// answer it once the provider reports the blocking event IDs;
				// subagent-thread calls cannot be routed back by this bridge.
				if r.CustomToolUses == nil {
					r.CustomToolUses = map[string]string{}
				}
				r.CustomToolUses[e.ID] = e.Name
			}
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
			if e.StopReason.Type == "requires_action" && b.profile.CustomTools {
				handled, err := b.answerCustomTools(ctx, r, e.StopReason.EventIDs)
				if err != nil {
					return err
				}
				if handled {
					// The session resumes once the queued results arrive; a new
					// idle/running event will follow, so this pause is not final.
					r.Phase = "running"
					r.IdleSince = time.Time{}
					break
				}
			}
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

// answerCustomTools replies to custom tool calls that are blocking a paused
// session. It returns true only when every blocking event is a custom tool
// call this bridge recorded; any other blocking action (a tool confirmation,
// or a subagent-thread call) is left to the normal failure path. Results are
// write-once: an ambiguous response is never replayed, because the provider
// has no idempotency key for it.
func (b *Bridge) answerCustomTools(ctx context.Context, r *run, eventIDs []string) (bool, error) {
	if len(eventIDs) == 0 || r.SessionID == "" || r.Failure != "" {
		return false, nil
	}
	for _, id := range eventIDs {
		if _, ok := r.CustomToolUses[id]; !ok {
			return false, nil
		}
	}
	if r.CustomToolSubmitted == nil {
		r.CustomToolSubmitted = map[string]bool{}
	}
	if r.CustomToolAnswered == nil {
		r.CustomToolAnswered = map[string]bool{}
	}
	for _, id := range eventIDs {
		if r.CustomToolAnswered[id] {
			continue
		}
		if r.CustomToolSubmitted[id] {
			// A prior attempt may have reached the provider. The run cannot be
			// safely retried without replaying a write, so fail it and leave
			// the remote session for cancellation.
			r.Failure = fmt.Sprintf("%s custom tool result delivery could not be confirmed; inspect session %s", b.profile.DisplayName, r.SessionID)
			return false, nil
		}
		name := r.CustomToolUses[id]
		if name == "" {
			name = "the custom tool"
		}
		r.CustomToolSubmitted[id] = true
		if err := b.persist(); err != nil {
			return false, err
		}
		body := map[string]any{"events": []map[string]any{{
			"type":               "user.custom_tool_result",
			"custom_tool_use_id": id,
			"is_error":           true,
			"content": []map[string]string{{
				"type": "text",
				"text": "This Multica bridge does not execute custom tools. " + name + " was not run. Continue without it, or configure the work to use the sandbox and write files under " + outputDirPrefix + ".",
			}},
		}}}
		if err := b.anthropic.call(ctx, http.MethodPost, "/sessions/"+url.PathEscape(r.SessionID)+"/events", body, nil); err != nil {
			if rateLimited(err) {
				r.CustomToolSubmitted[id] = false
				if persistErr := b.persist(); persistErr != nil {
					return false, persistErr
				}
				return false, err
			}
			if definitelyRejected(err) {
				r.Failure = fmt.Sprintf("%s rejected a custom tool result: %v", b.profile.DisplayName, err)
				return false, nil
			}
			// Never replay a write whose outcome is unknown.
			r.Failure = fmt.Sprintf("%s custom tool result delivery could not be confirmed; inspect session %s", b.profile.DisplayName, r.SessionID)
			return false, nil
		}
		r.CustomToolAnswered[id] = true
		if err := b.persist(); err != nil {
			return false, err
		}
	}
	return true, nil
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
