package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alkk/tg-mcp/pkg/store"
)

const (
	// inlineLimit bounds what travels inside a tool result; anything bigger is served over http.
	inlineLimit = 1 << 20
	filesRoute  = "/files/"
	sniffLen    = 512
	// baseHeader carries the externally visible base url of the /mcp request into the tool call.
	// trackBase always sets or clears it, so a client cannot smuggle one in.
	baseHeader = "X-Tg-Mcp-Base"
)

type getFileParams struct {
	Customer  string `json:"customer" jsonschema:"customer slug"`
	MessageID int64  `json:"message_id" jsonschema:"telegram message id of the message carrying the attachment"`
	Label     string `json:"label,omitempty" jsonschema:"group label, required when the customer has several groups"`
}

// fileResult describes the attachment; the bytes are either in the tool result content or behind
// the download URL.
type fileResult struct {
	MessageID int64  `json:"message_id"`
	Customer  string `json:"customer"`
	Label     string `json:"label,omitempty"`
	Media     string `json:"media"`
	FileName  string `json:"file_name"`
	FileSize  int64  `json:"file_size"`
	MimeType  string `json:"mime_type,omitempty"`
	Inline    bool   `json:"inline"`
	URL       string `json:"url,omitempty"`
}

func (s *Server) getFile(ctx context.Context, req *mcp.CallToolRequest,
	in getFileParams) (*mcp.CallToolResult, fileResult, error) {
	chat, err := s.singleChat(in.Customer, in.Label)
	if err != nil {
		return nil, fileResult{}, err
	}
	msg, err := s.store.MessageByID(ctx, chat.ID, in.MessageID)
	if err != nil {
		return nil, fileResult{}, fmt.Errorf("look up message %d: %w", in.MessageID, err)
	}
	if !msg.HasMedia() {
		return nil, fileResult{}, fmt.Errorf("message %d carries no attachment", in.MessageID)
	}

	path, err := s.cachedFile(ctx, msg)
	if err != nil {
		return nil, fileResult{}, err
	}

	customer, label := s.chatNamer()(chat.ID)
	res := fileResult{MessageID: msg.MessageID, Customer: customer, Label: label, Media: msg.MediaType,
		FileName: filepath.Base(path), FileSize: msg.FileSize}
	if info, statErr := os.Stat(path); statErr == nil {
		res.FileSize = info.Size()
	}

	content, mimeType, err := inlineContent(path, res.FileName, res.FileSize)
	if err != nil {
		return nil, fileResult{}, err
	}
	res.MimeType = mimeType
	if content == nil {
		res.URL = fileURL(req, msg.FileUniqueID)
		return nil, res, nil
	}
	res.Inline = true
	return &mcp.CallToolResult{Content: []mcp.Content{content}}, res, nil
}

// cachedFile returns the local path of an attachment, downloading it on a cache miss.
func (s *Server) cachedFile(ctx context.Context, m store.Message) (string, error) {
	if path, ok := s.store.Cached(m.FileUniqueID); ok {
		return path, nil
	}
	if s.telegram == nil {
		return "", errors.New("no telegram client configured, attachments cannot be fetched")
	}

	file, err := s.telegram.GetFile(ctx, m.FileID)
	if err != nil {
		return "", fmt.Errorf("resolve attachment of message %d: %w", m.MessageID, err)
	}
	name := m.FileName
	if name == "" {
		name = filepath.Base(file.FilePath)
	}

	path, err := s.store.SaveFile(m.FileUniqueID, name, func(w io.Writer) error {
		return s.telegram.Download(ctx, file.FilePath, w)
	})
	if err != nil {
		return "", fmt.Errorf("cache attachment of message %d: %w", m.MessageID, err)
	}
	slog.Info("attachment cached", "chat_id", m.ChatID, "message_id", m.MessageID,
		"media", m.MediaType, "file", name)
	return path, nil
}

// inlineContent decides how an attachment reaches the client: images and text below the size
// threshold ride along in the tool result, everything else is left for the /files/ endpoint and
// yields nil content.
func inlineContent(path, name string, size int64) (mcp.Content, string, error) {
	mimeType := baseType(mime.TypeByExtension(strings.ToLower(filepath.Ext(name))))
	if size > inlineLimit {
		return nil, mimeType, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // path comes from our own file cache
	if err != nil {
		return nil, mimeType, fmt.Errorf("read cached file %q: %w", name, err)
	}
	if mimeType == "" {
		mimeType = baseType(http.DetectContentType(data))
	}

	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return &mcp.ImageContent{Data: data, MIMEType: mimeType}, mimeType, nil
	case isTextual(mimeType) && utf8.Valid(data):
		return &mcp.TextContent{Text: string(data)}, mimeType, nil
	}
	return nil, mimeType, nil
}

func isTextual(mimeType string) bool {
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	for _, marker := range []string{"json", "xml", "yaml", "javascript", "x-sh"} {
		if strings.Contains(mimeType, marker) {
			return true
		}
	}
	return false
}

// baseType drops the parameters of a media type, leaving just type/subtype.
func baseType(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	return strings.TrimSpace(base)
}

// fileURL builds the download link for an attachment. The base comes from the very /mcp request
// this tool call arrived on, so the link points back through whatever proxy that client reached us
// through and no concurrent call can repoint it; without one it degrades to a path relative to the
// same listener.
func fileURL(req *mcp.CallToolRequest, fileUniqueID string) string {
	var base string
	if req != nil && req.Extra != nil {
		base = req.Extra.Header.Get(baseHeader)
	}
	return base + filesRoute + url.PathEscape(fileUniqueID)
}

// requestBase derives the externally visible base url of an incoming request, empty when the host
// is unknown.
func requestBase(r *http.Request) string {
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host, _, _ = strings.Cut(fwd, ",")
		host = strings.TrimSpace(host)
	}
	if host == "" {
		return ""
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme, _, _ = strings.Cut(fwd, ",")
		scheme = strings.TrimSpace(scheme)
	}
	return scheme + "://" + host
}

// serveFile hands out a cached attachment by file_unique_id, the id get_file returns in its
// download url. It sits behind the same bearer token as /mcp, so curl needs the header too.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	path, ok := s.store.Cached(r.PathValue("id"))
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	fh, err := os.Open(path) //nolint:gosec // path comes from our own file cache
	if err != nil {
		slog.Warn("cached file could not be opened", "err", err, "path", path)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	name := filepath.Base(path)
	// customer-supplied bytes: force a download and forbid content sniffing, so an .html
	// attachment cannot run as a page on this origin
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), fh)
}
