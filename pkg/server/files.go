package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alkk/tg-mcp/pkg/store"
)

const (
	// inlineLimit bounds what travels inside a tool result; anything bigger is served over http.
	inlineLimit = 1 << 20
	filesRoute  = "/files/"
	sniffLen    = 512
	// sigBytes is how much of the mac lands in the url; 128 bits is far past what a minutes-long
	// window needs.
	sigBytes = 16
	// baseHeader carries the externally visible base url of the /mcp request into the tool call.
	// trackBase always sets or clears it, so a client cannot smuggle one in.
	baseHeader = "X-Tg-Mcp-Base"
	// threadImageCap bounds how many images ride along with a conversation read.
	threadImageCap = 5
	// threadImageTimeout bounds the whole fetch round of a thread read: a slow attachment must
	// not hold up the conversation it was posted in.
	threadImageTimeout = 10 * time.Second
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
		FileName: displayName(msg), FileSize: msg.FileSize}
	if info, statErr := os.Stat(path); statErr == nil {
		res.FileSize = info.Size()
	}

	content, mimeType, err := inlineContent(path, res.FileName, res.FileSize)
	if err != nil {
		return nil, fileResult{}, err
	}
	res.MimeType = mimeType
	if content == nil {
		res.URL = fileURL(req, msg.FileUniqueID, time.Now().Add(s.linkTTL).Unix(), s.linkKey)
		return nil, res, nil
	}
	res.Inline = true
	return &mcp.CallToolResult{Content: []mcp.Content{content}}, res, nil
}

// displayName is the name an attachment is reported under. It comes off the message row, never
// off the cache path or telegram's file path: the latter is only known on a cache miss, so it
// would make the reported name depend on whether the file happened to be cached.
func displayName(m store.Message) string {
	if m.FileName != "" {
		return m.FileName
	}
	return m.FileUniqueID
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
	path, err := s.store.SaveFile(m.FileUniqueID, func(w io.Writer) error {
		return s.telegram.Download(ctx, file.FilePath, w)
	})
	if err != nil {
		return "", fmt.Errorf("cache attachment of message %d: %w", m.MessageID, err)
	}
	slog.Info("attachment cached", "chat_id", m.ChatID, "message_id", m.MessageID,
		"media", m.MediaType, "file", displayName(m))
	return path, nil
}

// inlineContent decides how an attachment reaches the client: readable images and text below the
// size threshold ride along in the tool result, everything else is left for the /files/ endpoint
// and yields nil content.
func inlineContent(path, name string, size int64) (mcp.Content, string, error) {
	mimeType := extType(name)
	if size > inlineLimit {
		return nil, mimeType, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // path comes from our own file cache
	if err != nil {
		return nil, mimeType, fmt.Errorf("read cached file %q: %w", name, err)
	}
	sniffed := baseType(http.DetectContentType(data))
	if mimeType == "" {
		mimeType = sniffed
	}

	switch {
	case isInlineImage(mimeType) && isInlineImage(sniffed):
		// the name is the customer's, so the extension alone would inline a zip called shot.png
		// and fail the whole call on a type the vision api rejects; the block carries the sniffed
		// type, which is also what the api reads, while the reported one stays name-derived
		return &mcp.ImageContent{Data: data, MIMEType: sniffed}, mimeType, nil
	case strings.HasPrefix(mimeType, "image/"):
		// heic, svg, bmp, tiff, avif, and anything whose bytes belie its name: an image the
		// vision api rejects fails the whole call, while a link still gets the bytes to whoever
		// asked. This arm also keeps svg out of the text arm below, which matches on the "xml"
		// in image/svg+xml.
		return nil, mimeType, nil
	case isTextual(mimeType) && utf8.Valid(data):
		return &mcp.TextContent{Text: string(data)}, mimeType, nil
	}
	return nil, mimeType, nil
}

// extType resolves an attachment name to a media type, empty when the extension says nothing.
func extType(name string) string {
	return baseType(mime.TypeByExtension(strings.ToLower(filepath.Ext(name))))
}

// isInlineImage reports whether an image type can ride inside a tool result. The set is closed
// on purpose: these are the types a vision model accepts, and anything else is better off as a
// download link than as an image block that fails the call.
func isInlineImage(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
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

// threadImages fetches the first few images of a conversation and returns them as content blocks
// in chronological order, together with the message ids they came from. Candidates are picked by
// name before anything is downloaded, so a log or a video never burns a slot; stickers are left
// out because five reactions would eat the whole cap. Every failure degrades that one image to
// its metadata — an attachment problem must not fail a thread read.
func (s *Server) threadImages(ctx context.Context, msgs []store.Message) ([]mcp.Content, map[int64]bool) {
	if s.telegram == nil {
		return nil, nil
	}
	var picked []store.Message
	for _, m := range msgs {
		if !m.HasMedia() || m.MediaType == "sticker" || !isInlineImage(extType(displayName(m))) {
			continue
		}
		if m.FileSize > inlineLimit {
			// a rendition the row already knows is too big would be downloaded only to be
			// discarded; a zero file_size is the one the Bot API omits, decided after the fetch
			continue
		}
		if picked = append(picked, m); len(picked) == threadImageCap {
			break
		}
	}
	if len(picked) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, threadImageTimeout)
	defer cancel()

	// indexed slots rather than a channel: chronological order stays structural instead of
	// something the assembly below has to restore
	fetched := make([]mcp.Content, len(picked))
	var wg sync.WaitGroup
	for i, m := range picked {
		wg.Go(func() { fetched[i] = s.threadImage(ctx, m) })
	}
	wg.Wait()

	blocks := make([]mcp.Content, 0, len(fetched))
	inlined := map[int64]bool{}
	for i, c := range fetched {
		if c == nil {
			continue
		}
		blocks = append(blocks, c)
		inlined[picked[i].MessageID] = true
	}
	return blocks, inlined
}

// threadImage renders one attachment as an image block, nil when it could not be fetched or the
// bytes turn out not to be inlinable after all. The size is re-stated from disk: the Bot API
// leaves file_size out on some renditions, so the stored one cannot gate the inline limit.
func (s *Server) threadImage(ctx context.Context, m store.Message) (content mcp.Content) {
	// this runs on its own goroutine, where nothing recovers: a panic in the fetch would take the
	// process down instead of degrading the one image, the way every other failure here does
	defer func() {
		if r := recover(); r != nil {
			slog.Error("thread image panicked", "panic", r, "chat_id", m.ChatID, "message_id", m.MessageID)
			content = nil
		}
	}()

	path, err := s.cachedFile(ctx, m)
	if err != nil {
		slog.Warn("thread image not fetched", "err", err, "chat_id", m.ChatID, "message_id", m.MessageID)
		return nil
	}
	name, size := displayName(m), m.FileSize
	if info, statErr := os.Stat(path); statErr == nil {
		size = info.Size()
	}
	inlined, _, err := inlineContent(path, name, size)
	if err != nil {
		slog.Warn("thread image not read", "err", err, "chat_id", m.ChatID, "message_id", m.MessageID)
		return nil
	}
	img, ok := inlined.(*mcp.ImageContent)
	if !ok {
		return nil
	}
	return img
}

// linkKeyPurpose domain-separates the download-link key from the auth token it is derived from,
// so a link signature can never be replayed as a credential and the key rotates with the token.
const linkKeyPurpose = "tg-mcp/files-url/v1"

// deriveLinkKey derives the key signing download links from the bearer token.
func deriveLinkKey(authToken string) []byte {
	mac := hmac.New(sha256.New, []byte(authToken))
	mac.Write([]byte(linkKeyPurpose))
	return mac.Sum(nil)
}

// signFileID signs a file_unique_id and its expiry. Only those two are covered: the base url is
// rebuilt per request from X-Forwarded-*, so signing the host would break a download that reached
// us by another path. The id is base64url, so it cannot contain the separator.
func signFileID(key []byte, id string, exp int64) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil)[:sigBytes])
}

// verifyFileSig checks a signature over id and exp, returning the expiry it verified. ok reports
// authenticity alone; exp and expired are only meaningful when ok is true, so the caller must
// answer 401 on !ok before it ever looks at expired — a forged link must not learn from a 410
// that the id exists.
func verifyFileSig(key []byte, id, expRaw, sig string) (exp int64, expired, ok bool) {
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return 0, false, false
	}
	if !hmac.Equal([]byte(sig), []byte(signFileID(key, id, exp))) {
		return 0, false, false
	}
	return exp, time.Now().Unix() > exp, true
}

// fileURL builds the download link for an attachment. The base comes from the very /mcp request
// this tool call arrived on, so the link points back through whatever proxy that client reached us
// through and no concurrent call can repoint it; without one it degrades to a path relative to the
// same listener. The expiry and its signature ride in the query, which is what lets a harness that
// never sees the bearer token fetch the bytes.
func fileURL(req *mcp.CallToolRequest, fileUniqueID string, exp int64, key []byte) string {
	var base string
	if req != nil && req.Extra != nil {
		base = req.Extra.Header.Get(baseHeader)
	}
	q := url.Values{"exp": {strconv.FormatInt(exp, 10)}, "sig": {signFileID(key, fileUniqueID, exp)}}
	return base + filesRoute + url.PathEscape(fileUniqueID) + "?" + q.Encode()
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
	return scheme + "://" + host + forwardedPrefix(r)
}

// forwardedPrefix reads the path prefix a proxy mounted us under, normalized to "/prefix" without
// a trailing slash, so downloads stay inside that mount. Anything that is not a plain absolute
// path is ignored rather than passed through: no prefix at all is the working default, while a
// prefix carrying a host or a scheme would point every download somewhere else entirely. The
// segment walk and the absolute-path gate both read the form that is actually emitted rather than
// the one url.Parse reports: an empty segment is the "///host" url.Parse declines to read as an
// authority, a dot segment is what a proxy resolves away before matching its own location, and
// "%2F@host" has an absolute u.Path but escapes back into an authority.
func forwardedPrefix(r *http.Request) string {
	prefix, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Prefix"), ",")
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	u, err := url.Parse(prefix)
	if err != nil || u.Scheme != "" || u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	esc := u.EscapedPath()
	if !strings.HasPrefix(esc, "/") {
		return ""
	}
	for _, seg := range strings.Split(u.Path, "/")[1:] {
		if seg == "" || seg == "." || seg == ".." {
			return ""
		}
	}
	return esc
}

// serveFile hands out a cached attachment by file_unique_id, the id get_file returns in its
// download url. fileAuth in front of it takes either the bearer token /mcp uses or the signature
// riding in the query, so curl works with the header and a harness works with the link alone.
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
	// customer-supplied bytes: force a download and forbid content sniffing, so an .html
	// attachment cannot run as a page on this origin. No filename= — the cache path carries no
	// name, and the consumer already has it from get_file and every listing tool.
	// under bearer-only the Authorization header kept shared caches out by itself; a credential
	// in the query does not, and a cached copy would outlive the signature that earned it
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", info.ModTime(), fh)
}
