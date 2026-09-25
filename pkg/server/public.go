package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

// publicResult is the body of GET /public/{name}; a nil Message means the chat has no messages yet.
type publicResult struct {
	Message *publicMessage `json:"message"`
}

type publicMessage struct {
	Sent   string       `json:"sent"`
	Sender string       `json:"sender"`
	Text   string       `json:"text"`
	Media  *publicMedia `json:"media,omitempty"`
	Link   string       `json:"link,omitempty"`
}

type publicMedia struct {
	Type     string `json:"type"`
	FileName string `json:"file_name,omitempty"`
}

// servePublic returns the newest message of the chat exposed under a public alias. A chat without
// one resolves to nothing, so an unknown alias gets the same plain 404 as an unmatched path and
// never hints that private chats exist.
func (s *Server) servePublic(w http.ResponseWriter, r *http.Request) {
	chat, ok := s.chats.ByPublic(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	var res publicResult
	if msg, ok := s.store.Latest(chat.ID); ok {
		res.Message = publicView(msg, chat.Username)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(res)
}

// publicView shapes a message for the public endpoint; username is the chat's public handle, and
// without one the message gets no link.
func publicView(m store.Message, username string) *publicMessage {
	pm := &publicMessage{Sent: m.Sent.UTC().Format(time.RFC3339), Sender: m.SenderName, Text: m.Text}
	if m.HasMedia() {
		pm.Media = &publicMedia{Type: m.MediaType}
		// a made-up name carries the file unique id, the key of the private file cache
		if !telegram.SynthesizedFileName(m.FileName, m.FileUniqueID) {
			pm.Media.FileName = m.FileName
		}
	}
	if username != "" {
		pm.Link = "https://t.me/" + username + "/" + strconv.FormatInt(m.MessageID, 10)
	}
	return pm
}
