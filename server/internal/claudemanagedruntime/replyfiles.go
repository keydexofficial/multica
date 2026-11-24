package claudemanagedruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	// Files the agent writes here are projected into the session's file
	// resources and attached to the chat reply.
	outputDirPrefix = "/mnt/user-data/outputs/"
	// maxReplyFileBytes keeps one transfer inside the bridge's step budget.
	maxReplyFileBytes    = 10 << 20
	maxReplyFileAttempts = 3
	replyFileTimeout     = 20 * time.Second
)

// chatReply tracks the files a chat run produced and the bridge's progress
// attaching them to the reply. Baseline holds the output files that already
// existed when this turn started, so files from earlier turns of a continued
// session are not sent again.
type chatReply struct {
	Baseline  []string        `json:"baseline"`
	Collected bool            `json:"collected,omitempty"`
	Delivered []deliveredFile `json:"delivered,omitempty"`
	// Handled counts delivered files already attached or skipped, so a retry
	// does not upload one twice.
	Handled  int             `json:"handled,omitempty"`
	Attempts int             `json:"attempts,omitempty"`
	Skipped  []replyFileSkip `json:"skipped,omitempty"`
	Locale   string          `json:"locale,omitempty"`
	// The task credential is never persisted; after a restart the remaining
	// files are not attached and the run completes without them.
	token string
}

type deliveredFile struct {
	FileID string `json:"file_id"`
	Name   string `json:"name,omitempty"`
}

type replyFileSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// resetForNewSession discards progress that belonged to a session the bridge
// abandoned. The credential and locale stay because the run continues.
func (f *chatReply) resetForNewSession() {
	f.Baseline = []string{}
	f.Collected = false
	f.Delivered = nil
	f.Handled = 0
	f.Attempts = 0
	f.Skipped = nil
}

type sessionFileResource struct {
	Type      string `json:"type"`
	FileID    string `json:"file_id"`
	MountPath string `json:"mount_path"`
}

// sessionFileResources lists one session's file resources, following the
// provider's opaque next_page cursor.
func (b *Bridge) sessionFileResources(ctx context.Context, sessionID string) ([]sessionFileResource, error) {
	var out []sessionFileResource
	page := ""
	for pages := 0; pages < 100; pages++ {
		q := url.Values{"limit": {"100"}}
		if b.profile.BetaQuery {
			q.Set("beta", "true")
		}
		if page != "" {
			q.Set("page", page)
		}
		var response struct {
			Data     []sessionFileResource `json:"data"`
			NextPage *string               `json:"next_page"`
		}
		if err := b.anthropic.call(ctx, http.MethodGet, "/sessions/"+url.PathEscape(sessionID)+"/resources?"+q.Encode(), nil, &response); err != nil {
			return nil, err
		}
		out = append(out, response.Data...)
		if response.NextPage == nil || *response.NextPage == "" || *response.NextPage == page {
			break
		}
		page = *response.NextPage
	}
	return out, nil
}

// outputFileIDs returns the output files already present in a session. A
// continued session is snapshot this way before its next message so earlier
// turns' files are not attached again.
func (b *Bridge) outputFileIDs(ctx context.Context, sessionID string) ([]string, error) {
	resources, err := b.sessionFileResources(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, resource := range resources {
		if resource.Type == "file" && resource.FileID != "" && strings.HasPrefix(resource.MountPath, outputDirPrefix) {
			ids = append(ids, resource.FileID)
		}
	}
	return ids, nil
}

// collectReplyFiles records this turn's new output files once, before any
// transfer starts.
func (b *Bridge) collectReplyFiles(ctx context.Context, r *run) error {
	if r.Reply == nil || r.Reply.Collected {
		return nil
	}
	resources, err := b.sessionFileResources(ctx, r.SessionID)
	if err != nil {
		if definitelyRejected(err) {
			// The session is gone or unreadable; complete the reply without
			// files rather than retry this step forever.
			r.Reply.Collected = true
			return b.persist()
		}
		return err
	}
	baseline := make(map[string]bool, len(r.Reply.Baseline))
	for _, id := range r.Reply.Baseline {
		baseline[id] = true
	}
	seen := make(map[string]bool, len(r.Reply.Delivered))
	for _, delivered := range r.Reply.Delivered {
		seen[delivered.FileID] = true
	}
	for _, resource := range resources {
		if resource.Type != "file" || resource.FileID == "" || !strings.HasPrefix(resource.MountPath, outputDirPrefix) {
			continue
		}
		if baseline[resource.FileID] || seen[resource.FileID] {
			continue
		}
		r.Reply.Delivered = append(r.Reply.Delivered, deliveredFile{FileID: resource.FileID, Name: path.Base(resource.MountPath)})
		seen[resource.FileID] = true
	}
	r.Reply.Collected = true
	return b.persist()
}

// transferNextReplyFile handles one file so each transfer has the step to
// itself. A file that cannot be attached is logged and skipped; the reply is
// still delivered.
func (b *Bridge) transferNextReplyFile(ctx context.Context, r *run) error {
	f := r.Reply
	if f == nil || f.Handled >= len(f.Delivered) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d := f.Delivered[f.Handled]
	transferCtx, cancel := context.WithTimeout(ctx, replyFileTimeout)
	defer cancel()
	reason, err := b.transferReplyFile(transferCtx, r, d)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		f.Attempts++
		if f.Attempts < maxReplyFileAttempts {
			if persistErr := b.persist(); persistErr != nil {
				return persistErr
			}
			return err
		}
		reason = fmt.Sprintf("not attached because the file transfer failed after %d attempts", maxReplyFileAttempts)
	}
	if reason != "" {
		name := d.Name
		if name == "" {
			name = d.FileID
		}
		f.Skipped = append(f.Skipped, replyFileSkip{Name: name, Reason: reason})
		b.log.WarnContext(ctx, b.profile.DisplayName+" file delivery was not confirmed", "task_id", r.TaskID, "file", name, "reason", reason)
	}
	f.Handled++
	f.Attempts = 0
	return b.persist()
}

// transferReplyFile downloads one provider file and attaches it to the chat
// reply. Once an upload is attempted, an ambiguous result must not replay a
// write, so it becomes a delivery notice instead of a retry.
func (b *Bridge) transferReplyFile(ctx context.Context, r *run, d deliveredFile) (string, error) {
	if r.Reply.token == "" {
		return "not attached because the bridge restarted during the run", nil
	}
	client := b.anthropic
	metaPath := "/files/" + url.PathEscape(d.FileID)
	contentPath := metaPath + "/content"
	if b.profile.BetaQuery {
		client = client.withBeta("files-api-2025-04-14")
		metaPath += "?beta=true"
		contentPath += "?beta=true"
	}
	var meta struct {
		Filename     string `json:"filename"`
		SizeBytes    int64  `json:"size_bytes"`
		Downloadable bool   `json:"downloadable"`
		Scope        *struct {
			ID string `json:"id"`
		} `json:"scope"`
	}
	refused := func(err error) bool {
		var ae *apiError
		return errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusForbidden)
	}
	if err := client.call(ctx, http.MethodGet, metaPath, nil, &meta); refused(err) {
		return "not attached because the file was not offered for download", nil
	} else if err != nil {
		return "", err
	}
	// The provider's key can read every file in the workspace, so only a file
	// scoped to this run's session is passed on, never another chat's.
	if meta.Scope == nil || meta.Scope.ID != r.SessionID {
		return "not attached because access to the file could not be verified", nil
	}
	if !meta.Downloadable {
		return "not attached because the file was not offered for download", nil
	}
	if meta.SizeBytes > maxReplyFileBytes {
		return "not attached because it exceeds the 10 MB limit", nil
	}
	data, err := client.download(ctx, contentPath, maxReplyFileBytes)
	if errors.Is(err, errTooLarge) {
		return "not attached because it exceeds the 10 MB limit", nil
	} else if refused(err) {
		return "not attached because the file was not offered for download", nil
	} else if err != nil {
		return "", err
	}
	name := d.Name
	if meta.Filename != "" {
		name = path.Base(meta.Filename)
	}
	// The provider controls this name; treat it as data, not a path or a
	// header value.
	name = uniqueAttachmentName(name, d.FileID, map[string]bool{})
	if err := ctx.Err(); err != nil {
		return "", err
	}
	taskAPI := *b.multica
	taskAPI.token = r.Reply.token
	var attachment struct {
		ID string `json:"id"`
	}
	if err := taskAPI.upload(ctx, "/api/upload-file", name, "", data, map[string]string{"task_id": r.TaskID}, &attachment); err != nil {
		if definitelyRejected(err) {
			return "not attached because Multica refused the upload", nil
		}
		return "delivery could not be confirmed; check the attachments before retrying", nil
	}
	if strings.TrimSpace(attachment.ID) == "" {
		return "not attached because Multica could not save the attachment", nil
	}
	return "", nil
}

// output leaves the agent's text unchanged and adds persisted delivery
// notices. Building it without mutating run.Output keeps completion retries
// identical.
func (f *chatReply) output(text string) string {
	if len(f.Skipped) == 0 {
		return text
	}
	var out strings.Builder
	out.WriteString(text)
	if text != "" {
		out.WriteString("\n\n")
	}
	copy := replyFileCopyFor(f.Locale)
	out.WriteString(copy.title + "\n\n")
	for _, file := range f.Skipped {
		// Names are data, not links, markup or additional lines in the notice.
		name := strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "\\<", ">", "\\>").Replace(strconv.Quote(file.Name))
		fmt.Fprintf(&out, "- %s: %s\n", name, copy.reason(file.Reason))
	}
	return strings.TrimSuffix(out.String(), "\n")
}
