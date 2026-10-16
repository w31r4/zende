package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"zende.local/harness/internal/model"
	"zende.local/harness/internal/protocol"
)

func (c *Core) importImage(ctx context.Context, req protocol.Request, p protocol.ImportAttachment) (protocol.Accepted, error) {
	if previous, found, err := c.Store.AcceptedRequest(ctx, req); found || err != nil {
		return previous, err
	}
	a, err := c.Store.Agent(ctx, p.AgentID)
	if err != nil {
		return protocol.Accepted{}, err
	}
	if a.State == "archived" || a.State == "copying" {
		return protocol.Accepted{}, protocol.E("conflict", "agent is read-only")
	}
	if p.Path == "" || len(p.Path) > 4096 || strings.IndexByte(p.Path, 0) >= 0 {
		return protocol.Accepted{}, protocol.E("invalid_params", "image path is required and must be at most 4096 bytes")
	}
	path := p.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.Cwd, path)
	}
	// Nonblocking open prevents a FIFO/device from holding the request before
	// we can reject it. Normal files and symlinks to normal files remain usable.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return protocol.Accepted{}, protocol.E("attachment_unavailable", "image file could not be opened")
	}
	f := os.NewFile(uintptr(fd), "attachment-input")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return protocol.Accepted{}, protocol.E("invalid_params", "image must be a regular file")
	}
	if st.Size() < 1 || st.Size() > protocol.MaxImageBytes {
		return protocol.Accepted{}, protocol.E("attachment_limit", "image must contain 1 byte to 4 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, protocol.MaxImageBytes+1))
	if err != nil {
		return protocol.Accepted{}, protocol.E("attachment_unavailable", "image file could not be read")
	}
	if err := ctx.Err(); err != nil {
		return protocol.Accepted{}, err
	}
	return c.Store.SaveImage(ctx, req, a.ID, filepath.Base(path), data)
}

func (c *Core) checkImageCapability(ctx context.Context, agent string) error {
	a, err := c.Store.Agent(ctx, agent)
	if err != nil {
		return err
	}
	selection := a.Model
	if selection == "" {
		selection = c.Config.Config.Model
	}
	resolver := c.modelResolver()
	if resolver == nil && !strings.HasPrefix(selection, "plugin:") {
		return protocol.E("unsupported_capability", "select a model with image input; draft attachments are retained")
	}
	var binding model.Binding
	if strings.HasPrefix(selection, "plugin:") {
		choice, readErr := c.Store.Selection(ctx, agent)
		if readErr != nil {
			return readErr
		}
		if len(choice.Binding) > 0 {
			binding, err = c.restoreBinding(ctx, agent, choice.Binding)
		} else {
			binding, err = c.resolveModelSelection(ctx, agent, selection, choice.Choice.Effort, "")
		}
	} else {
		binding, err = resolver.Resolve(ctx, selection)
	}
	if err != nil {
		return safeFailure(err, "model_unavailable", "selected model is unavailable; draft attachments are retained")
	}
	if !binding.Images || binding.Adapter == nil {
		return protocol.E("unsupported_capability", "selected model does not support image input; draft attachments are retained")
	}
	return nil
}

func (c *Core) resolveImages(ctx context.Context, agent string, messages []model.Message, enabled bool) (map[string]model.Image, error) {
	var images map[string]model.Image
	total := 0
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type != "image" {
				continue
			}
			if !enabled || message.Role != "user" {
				return nil, protocol.E("unsupported_capability", "model does not support this image input; original history is retained")
			}
			if images == nil {
				images = make(map[string]model.Image)
			}
			image, ok := images[block.Attachment]
			if !ok {
				info, err := c.Store.ImageInfo(ctx, protocol.AttachmentTarget{AgentID: agent, ID: block.Attachment})
				if err != nil {
					return nil, err
				}
				if info.Bytes < 1 || info.Bytes > protocol.MaxImageBytes || total+info.Bytes > protocol.MaxRequestImageBytes {
					return nil, protocol.E("context_limit", "request images exceed 12 MiB; original attachments are retained")
				}
				data, err := c.Store.ReadImage(ctx, protocol.AttachmentTarget{AgentID: agent, ID: block.Attachment})
				if err != nil {
					return nil, err
				}
				image = model.Image{MediaType: data.MediaType, Data: data.Data}
				images[block.Attachment] = image
			}
			total += len(image.Data) // Repeated references count each wire occurrence.
			if total > protocol.MaxRequestImageBytes {
				return nil, protocol.E("context_limit", "request images exceed 12 MiB; original attachments are retained")
			}
		}
	}
	return images, nil
}
