package telegram

import (
	"strings"
	"time"
	"unicode/utf16"
)

// entity types we act on
const (
	entityMention     = "mention"
	entityTextMention = "text_mention"
)

// Update is a single item of a getUpdates batch; only message and edited_message are requested.
type Update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *Message `json:"message"`
	EditedMessage *Message `json:"edited_message"`
}

// User is a telegram user or bot.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// DisplayName is the human-readable name for a user, falling back to the username.
func (u *User) DisplayName() string {
	if u == nil {
		return unknownSender
	}
	if name := strings.TrimSpace(u.FirstName + " " + u.LastName); name != "" {
		return name
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return unknownSender
}

// Chat is a telegram chat; for groups only id, type and title are of interest.
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
}

// Entity marks a formatted span of a message; offset and length are in UTF-16 code units.
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *User  `json:"user"`
}

// PhotoSize is one rendition of a photo; a message carries several.
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

// Document is a generic file attachment.
type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// Video is a video attachment.
type Video struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// Voice is a voice note.
type Voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// Audio is a music or audio file attachment.
type Audio struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// Animation is a gif or a soundless mp4. Telegram sets document alongside it for backward
// compatibility, so it has to be matched first.
type Animation struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

// VideoNote is a round video message; it carries neither a name nor a mime type.
type VideoNote struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
}

// Sticker is a sticker, in webp, animated tgs or webm video form.
type Sticker struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Emoji        string `json:"emoji"`
	IsAnimated   bool   `json:"is_animated"`
	IsVideo      bool   `json:"is_video"`
	FileSize     int64  `json:"file_size"`
}

// File is the getFile result: file_path is a download path, or an absolute filesystem path
// when the api server runs with --local.
type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileSize     int64  `json:"file_size"`
	FilePath     string `json:"file_path"`
}

// Message is a telegram message, limited to the fields tg-mcp stores or acts on.
type Message struct {
	MessageID       int64       `json:"message_id"`
	MessageThreadID int64       `json:"message_thread_id"`
	From            *User       `json:"from"`
	SenderChat      *Chat       `json:"sender_chat"`
	Chat            Chat        `json:"chat"`
	Date            int64       `json:"date"`
	EditDate        int64       `json:"edit_date"`
	ReplyToMessage  *Message    `json:"reply_to_message"`
	Text            string      `json:"text"`
	Entities        []Entity    `json:"entities"`
	Caption         string      `json:"caption"`
	CaptionEntities []Entity    `json:"caption_entities"`
	Photo           []PhotoSize `json:"photo"`
	Document        *Document   `json:"document"`
	Video           *Video      `json:"video"`
	Voice           *Voice      `json:"voice"`
	Audio           *Audio      `json:"audio"`
	Animation       *Animation  `json:"animation"`
	VideoNote       *VideoNote  `json:"video_note"`
	Sticker         *Sticker    `json:"sticker"`
	MigrateToChatID int64       `json:"migrate_to_chat_id"`
}

// Media describes the attachment of a message, flattened across the media types we handle.
type Media struct {
	Type         string
	FileID       string
	FileUniqueID string
	FileName     string
	FileSize     int64
}

const unknownSender = "unknown"

// Body is the message text, or the caption for a media message.
func (m *Message) Body() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

// SentAt is the original send time in UTC.
func (m *Message) SentAt() time.Time { return time.Unix(m.Date, 0).UTC() }

// EditedAt is the edit time in UTC; ok is false for messages that were never edited.
func (m *Message) EditedAt() (t time.Time, ok bool) {
	if m.EditDate == 0 {
		return time.Time{}, false
	}
	return time.Unix(m.EditDate, 0).UTC(), true
}

// Sender resolves the author; an absent from (anonymous admin, channel post) yields id 0 and
// the sender chat title.
func (m *Message) Sender() (id int64, name string) {
	if m.From != nil {
		return m.From.ID, m.From.DisplayName()
	}
	if m.SenderChat != nil {
		switch {
		case m.SenderChat.Title != "":
			return 0, m.SenderChat.Title
		case m.SenderChat.Username != "":
			return 0, "@" + m.SenderChat.Username
		}
	}
	return 0, unknownSender
}

// Media extracts the attachment of a message; ok is false for text-only messages. Every
// file-bearing type is covered: a caption-less attachment we did not recognize would look like a
// service message to ingest and the whole message would be dropped.
//
// Order matters — telegram sets document alongside animation, and photo alongside neither.
func (m *Message) Media() (media Media, ok bool) {
	switch {
	case len(m.Photo) > 0:
		p := largestPhoto(m.Photo)
		return Media{Type: "photo", FileID: p.FileID, FileUniqueID: p.FileUniqueID,
			FileName: p.FileUniqueID + ".jpg", FileSize: p.FileSize}, true
	case m.Animation != nil:
		return Media{Type: "animation", FileID: m.Animation.FileID, FileUniqueID: m.Animation.FileUniqueID,
			FileName: fileName(m.Animation.FileName, m.Animation.FileUniqueID, ".mp4"), FileSize: m.Animation.FileSize}, true
	case m.Document != nil:
		return Media{Type: "document", FileID: m.Document.FileID, FileUniqueID: m.Document.FileUniqueID,
			FileName: fileName(m.Document.FileName, m.Document.FileUniqueID, ""), FileSize: m.Document.FileSize}, true
	case m.Video != nil:
		return Media{Type: "video", FileID: m.Video.FileID, FileUniqueID: m.Video.FileUniqueID,
			FileName: fileName(m.Video.FileName, m.Video.FileUniqueID, ".mp4"), FileSize: m.Video.FileSize}, true
	case m.VideoNote != nil:
		return Media{Type: "video_note", FileID: m.VideoNote.FileID, FileUniqueID: m.VideoNote.FileUniqueID,
			FileName: m.VideoNote.FileUniqueID + ".mp4", FileSize: m.VideoNote.FileSize}, true
	case m.Audio != nil:
		return Media{Type: "audio", FileID: m.Audio.FileID, FileUniqueID: m.Audio.FileUniqueID,
			FileName: fileName(m.Audio.FileName, m.Audio.FileUniqueID, ".mp3"), FileSize: m.Audio.FileSize}, true
	case m.Voice != nil:
		return Media{Type: "voice", FileID: m.Voice.FileID, FileUniqueID: m.Voice.FileUniqueID,
			FileName: m.Voice.FileUniqueID + ".ogg", FileSize: m.Voice.FileSize}, true
	case m.Sticker != nil:
		return Media{Type: "sticker", FileID: m.Sticker.FileID, FileUniqueID: m.Sticker.FileUniqueID,
			FileName: m.Sticker.FileUniqueID + stickerExt(m.Sticker), FileSize: m.Sticker.FileSize}, true
	}
	return Media{}, false
}

// MentionsBot reports whether the message addresses the bot by @username, in either the text
// or the caption entities.
func (m *Message) MentionsBot(username string) bool {
	if username == "" {
		return false
	}
	return mentions(m.Text, m.Entities, username) || mentions(m.Caption, m.CaptionEntities, username)
}

func mentions(text string, entities []Entity, username string) bool {
	var units []uint16
	for _, e := range entities {
		switch e.Type {
		case entityTextMention:
			if e.User != nil && strings.EqualFold(e.User.Username, username) {
				return true
			}
		case entityMention:
			if units == nil {
				units = utf16.Encode([]rune(text))
			}
			// the upper bounds are checked by subtraction: offset+length overflows to a negative
			// number on absurd input and would sail past a sum comparison into a slice panic
			if e.Offset < 0 || e.Length <= 0 || e.Offset > len(units) || e.Length > len(units)-e.Offset {
				continue
			}
			if strings.EqualFold(string(utf16.Decode(units[e.Offset:e.Offset+e.Length])), "@"+username) {
				return true
			}
		}
	}
	return false
}

func largestPhoto(sizes []PhotoSize) PhotoSize {
	largest := sizes[0]
	for _, p := range sizes[1:] {
		// resolution decides, file size only breaks a tie: telegram omits it on some renditions
		area, best := p.Width*p.Height, largest.Width*largest.Height
		if area > best || (area == best && p.FileSize > largest.FileSize) {
			largest = p
		}
	}
	return largest
}

// stickerExt picks the extension matching the sticker format telegram sent.
func stickerExt(s *Sticker) string {
	switch {
	case s.IsAnimated:
		return ".tgs"
	case s.IsVideo:
		return ".webm"
	}
	return ".webp"
}

// fileName falls back to the file unique id when telegram sends no name for an attachment.
func fileName(name, uniqueID, ext string) string {
	if name != "" {
		return name
	}
	return uniqueID + ext
}
