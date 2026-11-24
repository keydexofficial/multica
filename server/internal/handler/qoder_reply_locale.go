package handler

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/cloudagent"
	"github.com/multica-ai/multica/server/internal/integrations/wecom"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type chatNoticeUserLookup interface {
	GetUser(context.Context, pgtype.UUID) (db.User, error)
}

// cloudChatNoticeLocale picks the locale for a bridge-generated delivery
// notice. Only cloud providers whose runtime can take chat turns carry one;
// local daemons render their own notices, and a chat-incapable cloud runtime
// never claims a chat turn in the first place.
func (h *Handler) cloudChatNoticeLocale(ctx context.Context, runtime db.AgentRuntime, task db.AgentTaskQueue, session db.ChatSession, channelType, chatType string) string {
	if !cloudagent.SupportsChat(runtime.Provider) {
		return ""
	}
	// A cosmetic lookup cannot hold up a task claim. Never use the runtime
	// owner's profile to decide what the chat's reader should see.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	reader := task.InitiatorUserID
	if !reader.Valid {
		reader = session.CreatorID
	}
	return chatNoticeLocale(ctx, h.Queries, reader, channelType, chatType)
}

func chatNoticeLocale(ctx context.Context, q chatNoticeUserLookup, reader pgtype.UUID, channelType, chatType string) string {
	// A room has no personal profile. Match the channel adapters' existing
	// defaults; WeCom's room language is explicitly configurable at deployment.
	fallback := "en"
	switch channelType {
	case "wecom":
		fallback = string(wecom.DeploymentLocale())
	case "lark", "dingtalk":
		fallback = "zh-Hans"
	}
	if channelType != "" && chatType != "p2p" {
		return fallback
	}
	if q != nil && reader.Valid {
		if user, err := q.GetUser(ctx, reader); err == nil {
			if locale := strings.TrimSpace(user.Language.String); locale != "" {
				return locale
			}
		}
	}
	return fallback
}
