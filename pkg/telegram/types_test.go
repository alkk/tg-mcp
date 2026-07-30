package telegram

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageBodyAndTimes(t *testing.T) {
	tests := []struct {
		name     string
		msg      Message
		wantBody string
		wantSent time.Time
		wantEdit bool
	}{
		{
			name:     "text message",
			msg:      Message{Date: 1700000000, Text: "hello"},
			wantBody: "hello",
			wantSent: time.Unix(1700000000, 0).UTC(),
		},
		{
			name:     "caption used as body",
			msg:      Message{Date: 1700000000, Caption: "see the log"},
			wantBody: "see the log",
			wantSent: time.Unix(1700000000, 0).UTC(),
		},
		{
			name:     "no text at all",
			msg:      Message{Date: 1700000000},
			wantBody: "",
			wantSent: time.Unix(1700000000, 0).UTC(),
		},
		{
			name:     "edited message",
			msg:      Message{Date: 1700000000, EditDate: 1700000060, Text: "fixed"},
			wantBody: "fixed",
			wantSent: time.Unix(1700000000, 0).UTC(),
			wantEdit: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantBody, tt.msg.Body())
			assert.Equal(t, tt.wantSent, tt.msg.SentAt())

			edited, ok := tt.msg.EditedAt()
			assert.Equal(t, tt.wantEdit, ok)
			if tt.wantEdit {
				assert.Equal(t, time.Unix(tt.msg.EditDate, 0).UTC(), edited)
			}
		})
	}
}

func TestMessageSender(t *testing.T) {
	tests := []struct {
		name     string
		msg      Message
		wantID   int64
		wantName string
	}{
		{
			name:     "first and last name",
			msg:      Message{From: &User{ID: 42, FirstName: "Ada", LastName: "Lovelace"}},
			wantID:   42,
			wantName: "Ada Lovelace",
		},
		{
			name:     "first name only",
			msg:      Message{From: &User{ID: 42, FirstName: "Ada"}},
			wantID:   42,
			wantName: "Ada",
		},
		{
			name:     "username fallback",
			msg:      Message{From: &User{ID: 42, Username: "ada"}},
			wantID:   42,
			wantName: "@ada",
		},
		{
			name:     "no name at all",
			msg:      Message{From: &User{ID: 42}},
			wantID:   42,
			wantName: "unknown",
		},
		{
			name:     "anonymous admin falls back to sender chat title",
			msg:      Message{SenderChat: &Chat{ID: -100, Title: "Acme Support"}},
			wantID:   0,
			wantName: "Acme Support",
		},
		{
			name:     "sender chat without title",
			msg:      Message{SenderChat: &Chat{ID: -100, Username: "acme"}},
			wantID:   0,
			wantName: "@acme",
		},
		{
			name:     "neither from nor sender chat",
			msg:      Message{},
			wantID:   0,
			wantName: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, name := tt.msg.Sender()
			assert.Equal(t, tt.wantID, id)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

func TestMessageMedia(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		want Media
		ok   bool
	}{
		{
			name: "text only",
			msg:  Message{Text: "hello"},
		},
		{
			name: "photo picks the largest size",
			msg: Message{Photo: []PhotoSize{
				{FileID: "s", FileUniqueID: "us", Width: 90, Height: 60, FileSize: 1000},
				{FileID: "l", FileUniqueID: "ul", Width: 1280, Height: 720, FileSize: 90000},
				{FileID: "m", FileUniqueID: "um", Width: 320, Height: 180, FileSize: 9000},
			}},
			want: Media{Type: "photo", FileID: "l", FileUniqueID: "ul", FileName: "ul.jpg", FileSize: 90000},
			ok:   true,
		},
		{
			name: "photo prefers resolution over file size",
			msg: Message{Photo: []PhotoSize{
				{FileID: "l", FileUniqueID: "ul", Width: 1280, Height: 720},
				{FileID: "s", FileUniqueID: "us", Width: 90, Height: 60, FileSize: 1000},
			}},
			want: Media{Type: "photo", FileID: "l", FileUniqueID: "ul", FileName: "ul.jpg"},
			ok:   true,
		},
		{
			name: "photo breaks a resolution tie on file size",
			msg: Message{Photo: []PhotoSize{
				{FileID: "a", FileUniqueID: "ua", Width: 320, Height: 180, FileSize: 900},
				{FileID: "b", FileUniqueID: "ub", Width: 320, Height: 180, FileSize: 9000},
			}},
			want: Media{Type: "photo", FileID: "b", FileUniqueID: "ub", FileName: "ub.jpg", FileSize: 9000},
			ok:   true,
		},
		{
			name: "document keeps its name",
			msg:  Message{Document: &Document{FileID: "f", FileUniqueID: "uf", FileName: "netxmsd.log", FileSize: 5}},
			want: Media{Type: "document", FileID: "f", FileUniqueID: "uf", FileName: "netxmsd.log", FileSize: 5},
			ok:   true,
		},
		{
			name: "document without a name",
			msg:  Message{Document: &Document{FileID: "f", FileUniqueID: "uf"}},
			want: Media{Type: "document", FileID: "f", FileUniqueID: "uf", FileName: "uf"},
			ok:   true,
		},
		{
			name: "video without a name gets an extension",
			msg:  Message{Video: &Video{FileID: "v", FileUniqueID: "uv", FileSize: 700}},
			want: Media{Type: "video", FileID: "v", FileUniqueID: "uv", FileName: "uv.mp4", FileSize: 700},
			ok:   true,
		},
		{
			name: "voice",
			msg:  Message{Voice: &Voice{FileID: "o", FileUniqueID: "uo", FileSize: 12}},
			want: Media{Type: "voice", FileID: "o", FileUniqueID: "uo", FileName: "uo.ogg", FileSize: 12},
			ok:   true,
		},
		{
			name: "audio keeps its name",
			msg:  Message{Audio: &Audio{FileID: "a", FileUniqueID: "ua", FileName: "call.m4a", FileSize: 42}},
			want: Media{Type: "audio", FileID: "a", FileUniqueID: "ua", FileName: "call.m4a", FileSize: 42},
			ok:   true,
		},
		{
			name: "audio without a name gets an extension",
			msg:  Message{Audio: &Audio{FileID: "a", FileUniqueID: "ua"}},
			want: Media{Type: "audio", FileID: "a", FileUniqueID: "ua", FileName: "ua.mp3"},
			ok:   true,
		},
		{
			name: "animation wins over the document telegram sets alongside it",
			msg: Message{
				Animation: &Animation{FileID: "g", FileUniqueID: "ug", FileSize: 33},
				Document:  &Document{FileID: "d", FileUniqueID: "ud", FileName: "shrug.gif"},
			},
			want: Media{Type: "animation", FileID: "g", FileUniqueID: "ug", FileName: "ug.mp4", FileSize: 33},
			ok:   true,
		},
		{
			name: "video note",
			msg:  Message{VideoNote: &VideoNote{FileID: "n", FileUniqueID: "un", FileSize: 90}},
			want: Media{Type: "video_note", FileID: "n", FileUniqueID: "un", FileName: "un.mp4", FileSize: 90},
			ok:   true,
		},
		{
			name: "static sticker",
			msg:  Message{Sticker: &Sticker{FileID: "s", FileUniqueID: "us", Emoji: "👍", FileSize: 7}},
			want: Media{Type: "sticker", FileID: "s", FileUniqueID: "us", FileName: "us.webp", FileSize: 7},
			ok:   true,
		},
		{
			name: "animated sticker",
			msg:  Message{Sticker: &Sticker{FileID: "s", FileUniqueID: "us", IsAnimated: true}},
			want: Media{Type: "sticker", FileID: "s", FileUniqueID: "us", FileName: "us.tgs"},
			ok:   true,
		},
		{
			name: "video sticker",
			msg:  Message{Sticker: &Sticker{FileID: "s", FileUniqueID: "us", IsVideo: true}},
			want: Media{Type: "sticker", FileID: "s", FileUniqueID: "us", FileName: "us.webm"},
			ok:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			media, ok := tt.msg.Media()
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, media)
		})
	}
}

func TestMessageMentionsBot(t *testing.T) {
	tests := []struct {
		name string
		msg  Message
		bot  string
		want bool
	}{
		{
			name: "mention in text",
			msg: Message{Text: "hey @tgbot look at this",
				Entities: []Entity{{Type: "mention", Offset: 4, Length: 6}}},
			bot:  "tgbot",
			want: true,
		},
		{
			name: "mention is case insensitive",
			msg: Message{Text: "@TgBot ping",
				Entities: []Entity{{Type: "mention", Offset: 0, Length: 6}}},
			bot:  "tgbot",
			want: true,
		},
		{
			name: "mention of somebody else",
			msg: Message{Text: "@someone else",
				Entities: []Entity{{Type: "mention", Offset: 0, Length: 8}}},
			bot: "tgbot",
		},
		{
			name: "mention in caption",
			msg: Message{Caption: "log for @tgbot", Photo: []PhotoSize{{FileID: "p"}},
				CaptionEntities: []Entity{{Type: "mention", Offset: 8, Length: 6}}},
			bot:  "tgbot",
			want: true,
		},
		{
			name: "text_mention entity",
			msg: Message{Text: "hi bot",
				Entities: []Entity{{Type: "text_mention", Offset: 3, Length: 3, User: &User{Username: "tgbot", IsBot: true}}}},
			bot:  "tgbot",
			want: true,
		},
		{
			name: "utf-16 offsets past emoji",
			msg: Message{Text: "🚀🚀 @tgbot",
				Entities: []Entity{{Type: "mention", Offset: 5, Length: 6}}},
			bot:  "tgbot",
			want: true,
		},
		{
			name: "entity out of range is ignored",
			msg: Message{Text: "short",
				Entities: []Entity{{Type: "mention", Offset: 40, Length: 6}}},
			bot: "tgbot",
		},
		{
			name: "entity bounds that overflow on addition are ignored",
			msg: Message{Text: "short",
				Entities: []Entity{{Type: "mention", Offset: math.MaxInt - 1, Length: math.MaxInt - 1}}},
			bot: "tgbot",
		},
		{
			name: "no entities",
			msg:  Message{Text: "plain @tgbot text without entities"},
			bot:  "tgbot",
		},
		{
			name: "unknown bot username",
			msg: Message{Text: "@tgbot",
				Entities: []Entity{{Type: "mention", Offset: 0, Length: 6}}},
			bot: "",
		},
		{
			name: "other entity types ignored",
			msg: Message{Text: "@tgbot",
				Entities: []Entity{{Type: "bold", Offset: 0, Length: 6}}},
			bot: "tgbot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.msg.MentionsBot(tt.bot))
		})
	}
}

func TestUpdateUnmarshal(t *testing.T) {
	raw := `{
	  "update_id": 7,
	  "message": {
	    "message_id": 12,
	    "message_thread_id": 3,
	    "date": 1700000000,
	    "chat": {"id": -1001, "type": "supergroup", "title": "Acme"},
	    "from": {"id": 5, "first_name": "Ada", "username": "ada"},
	    "reply_to_message": {"message_id": 11, "date": 1699999999, "text": "parent"},
	    "text": "child",
	    "migrate_to_chat_id": -1002
	  }
	}`

	var upd Update
	require.NoError(t, json.Unmarshal([]byte(raw), &upd))

	assert.Equal(t, int64(7), upd.UpdateID)
	require.NotNil(t, upd.Message)
	assert.Equal(t, int64(12), upd.Message.MessageID)
	assert.Equal(t, int64(3), upd.Message.MessageThreadID)
	assert.Equal(t, int64(-1001), upd.Message.Chat.ID)
	assert.Equal(t, int64(-1002), upd.Message.MigrateToChatID)
	require.NotNil(t, upd.Message.ReplyToMessage)
	assert.Equal(t, int64(11), upd.Message.ReplyToMessage.MessageID)
	assert.Nil(t, upd.EditedMessage)
}

func TestUserDisplayNameNil(t *testing.T) {
	var u *User
	assert.Equal(t, "unknown", u.DisplayName())
}
