package claudemanagedruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	// maxChatFileBytes keeps each upload inside the bridge's 30-second step.
	maxChatFileBytes = 5 << 20
	// maxChatImageBytes matches the image limit of a user message content block.
	maxChatImageBytes = 5 << 20
	// chatAttachmentMountDir prefixes each task's uploaded files so names from
	// different turns of a continued session never collide.
	chatAttachmentMountDir = "/mnt/session/uploads/"
)

var imageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// chatAttachmentMeta is the workspace attachment record a task credential can
// read. Chat claims carry only IDs and names.
type chatAttachmentMeta struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	DownloadURL string `json:"download_url"`
}

// attachChatFiles hands a chat message's files to the session: images become
// image blocks on the message, other files are uploaded and mounted for the
// agent to read. Anything that cannot be transferred is named in the prompt so
// the agent does not answer as if the user had sent text only.
func (b *Bridge) attachChatFiles(ctx context.Context, taskAPI *api, t *claimedTask, r *run, p *strings.Builder) error {
	if len(t.ChatAttachments) == 0 {
		return nil
	}
	var mounted, images, skipped []string
	used := map[string]bool{}
	for _, attached := range t.ChatAttachments {
		var meta chatAttachmentMeta
		err := taskAPI.call(ctx, http.MethodGet, "/api/attachments/"+url.PathEscape(attached.ID), nil, &meta)
		var missing *apiError
		if errors.As(err, &missing) && missing.Status == http.StatusNotFound {
			skipped = append(skipped, uniqueAttachmentName(attached.Filename, attached.ID, used)+" (deleted)")
			continue
		}
		if err != nil {
			return fmt.Errorf("read chat attachment %s: %w", attached.Filename, err)
		}
		name := uniqueAttachmentName(meta.Filename, attached.ID, used)
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
			return fmt.Errorf("download chat attachment %s: %w", name, err)
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
		mountPath := chatAttachmentMountDir + t.ID + "/" + name
		r.Files = append(r.Files, map[string]any{"type": "file", "file_id": fileID, "mount_path": mountPath})
		mounted = append(mounted, mountPath)
	}
	p.WriteString("\nMessage attachments:\n")
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
		if r == '/' || r == '\\' || r < 0x20 {
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
		name = name[len(name)-200:]
	}
	unique := name
	for i := 2; used[unique]; i++ {
		ext := path.Ext(name)
		unique = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), i, ext)
	}
	used[unique] = true
	return unique
}
