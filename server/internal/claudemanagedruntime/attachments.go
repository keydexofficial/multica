package claudemanagedruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxChatFileBytes keeps each upload inside the bridge's 30-second step.
	maxChatFileBytes = 5 << 20
	// maxChatImageBytes matches the image limit of a user message content block.
	maxChatImageBytes = 5 << 20
	// attachmentUploadNamespace is the public mount_path namespace OMA maps
	// beneath the sandbox uploads root. The agent-visible path is
	// /mnt/session/uploads/<rest>.
	attachmentUploadNamespace = "/uploads/"
	attachmentSandboxRoot     = "/mnt/session/uploads/"
)

var imageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// chatAttachmentMeta is the workspace attachment record a task credential can
// read. Chat claims carry only IDs and names, so NeedsMetadata defers the read
// until the file is transferred; issue claims already include the metadata.
type chatAttachmentMeta struct {
	ID            string `json:"id"`
	Filename      string `json:"filename"`
	ContentType   string `json:"content_type"`
	SizeBytes     int64  `json:"size_bytes"`
	DownloadURL   string `json:"download_url"`
	NeedsMetadata bool   `json:"-"`
}

// attachChatFiles hands a chat message's files to the session: images become
// image blocks on the message, other files are uploaded and mounted for the
// agent to read. Anything that cannot be transferred is named in the prompt so
// the agent does not answer as if the user had sent text only.
func (b *Bridge) attachChatFiles(ctx context.Context, taskAPI *api, t *claimedTask, r *run, p *strings.Builder) error {
	if len(t.ChatAttachments) == 0 {
		return nil
	}
	attachments := make([]chatAttachmentMeta, 0, len(t.ChatAttachments))
	for _, attached := range t.ChatAttachments {
		attachments = append(attachments, chatAttachmentMeta{ID: attached.ID, Filename: attached.Filename, NeedsMetadata: true})
	}
	return b.attachFiles(ctx, taskAPI, "Message attachments", t.ID, attachments, true, r, p)
}

// attachIssueFiles hands an issue's attachments to the session the same way,
// but from metadata already present on the issue response. Issue runs never
// continue a prior session when files are attached, so every mount starts from
// a clean session.
func (b *Bridge) attachIssueFiles(ctx context.Context, taskAPI *api, t *claimedTask, attachments []chatAttachmentMeta, r *run, p *strings.Builder) error {
	if len(attachments) == 0 {
		return nil
	}
	if !b.profile.IssueAttachments {
		return fmt.Errorf("issue attachments are not supported by the %s bridge", b.profile.DisplayName)
	}
	return b.attachFiles(ctx, taskAPI, "Issue attachments", t.ID, attachments, false, r, p)
}

// attachmentMountPath is the public mount_path (uploads namespace) and the
// agent-visible path for one attached file. OMA prefixes the public namespace
// with /mnt/session when it mounts the sandbox, so the prompt must name the
// prefixed path while the resource carries the unprefixed one.
func attachmentMountPath(taskID, name string) (mountPath, visiblePath string) {
	rest := taskID + "/" + name
	return attachmentUploadNamespace + rest, attachmentSandboxRoot + rest
}

func (b *Bridge) attachFiles(ctx context.Context, taskAPI *api, heading, taskID string, attachments []chatAttachmentMeta, resolveMetadata bool, r *run, p *strings.Builder) error {
	var mounted, images, skipped []string
	used := map[string]bool{}
	for _, attached := range attachments {
		meta := attached
		if resolveMetadata && meta.NeedsMetadata {
			var current chatAttachmentMeta
			err := taskAPI.call(ctx, http.MethodGet, "/api/attachments/"+url.PathEscape(meta.ID), nil, &current)
			var missing *apiError
			if errors.As(err, &missing) && missing.Status == http.StatusNotFound {
				skipped = append(skipped, uniqueAttachmentName(meta.Filename, meta.ID, used)+" (deleted)")
				continue
			}
			if err != nil {
				return fmt.Errorf("read chat attachment %s: %w", meta.Filename, err)
			}
			meta = current
		}
		name := uniqueAttachmentName(meta.Filename, meta.ID, used)
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(meta.ContentType, ";")[0]))
		limit := int64(maxChatFileBytes)
		if imageMediaTypes[mediaType] {
			limit = maxChatImageBytes
		}
		data, err := taskAPI.download(ctx, meta.DownloadURL, limit)
		if errors.Is(err, errTooLarge) {
			skipped = append(skipped, name+" (too large)")
			continue
		}
		if err != nil {
			return fmt.Errorf("download attachment %s: %w", name, err)
		}
		fileID, err := b.uploadProviderFile(ctx, name, mediaType, data)
		if err != nil {
			var rejected *apiError
			if errors.As(err, &rejected) && (rejected.Status == http.StatusBadRequest || rejected.Status == http.StatusRequestEntityTooLarge || rejected.Status == http.StatusUnsupportedMediaType) {
				skipped = append(skipped, name+" (rejected by "+b.profile.DisplayName+")")
				continue
			}
			return fmt.Errorf("upload attachment %s to %s: %w", name, b.profile.DisplayName, err)
		}
		if imageMediaTypes[mediaType] {
			r.Images = append(r.Images, map[string]any{"type": "image", "source": map[string]any{"type": "file", "file_id": fileID}})
			images = append(images, name)
			continue
		}
		mountPath, visiblePath := attachmentMountPath(taskID, name)
		r.Files = append(r.Files, map[string]any{"type": "file", "file_id": fileID, "mount_path": mountPath})
		mounted = append(mounted, visiblePath)
	}
	fmt.Fprintf(p, "\n%s:\n", heading)
	for _, mountedPath := range mounted {
		fmt.Fprintf(p, "- %s (mounted file)\n", mountedPath)
	}
	for _, name := range images {
		fmt.Fprintf(p, "- %s (image attached to this message)\n", name)
	}
	for _, name := range skipped {
		fmt.Fprintf(p, "- %s: not available in this session\n", name)
	}
	return nil
}

func (b *Bridge) uploadProviderFile(ctx context.Context, name, contentType string, data []byte) (string, error) {
	client := b.anthropic
	endpoint := "/files"
	if b.profile.BetaQuery {
		client = client.withBeta("files-api-2025-04-14")
		endpoint += "?beta=true"
	}
	var file struct {
		ID string `json:"id"`
	}
	if err := client.upload(ctx, endpoint, name, contentType, data, nil, &file); err != nil {
		return "", err
	}
	if file.ID == "" {
		return "", fmt.Errorf("%s file upload returned no file ID", b.profile.DisplayName)
	}
	return file.ID, nil
}

// betaPath adds the beta query a self-hosted provider requires on newer
// endpoints such as session resources and Files.
func (b *Bridge) betaPath(path string) string {
	if b.profile.BetaQuery {
		return path + "?beta=true"
	}
	return path
}

// uniqueAttachmentName returns a path-free, unique name for a mount path.
func uniqueAttachmentName(filename, fallbackID string, used map[string]bool) string {
	name := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(filename))
	if name == "" || name == "." || name == ".." {
		name = fallbackID
	}
	if name == "" {
		name = "attachment"
	}
	if len(name) > 200 {
		// Truncate from the front without splitting a multi-byte rune: OMA
		// rejects a mount path that is not valid UTF-8.
		start := len(name) - 200
		for start < len(name) && !utf8.RuneStart(name[start]) {
			start++
		}
		name = name[start:]
	}
	unique := name
	for i := 2; used[unique]; i++ {
		ext := path.Ext(name)
		unique = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext)
	}
	used[unique] = true
	return unique
}
