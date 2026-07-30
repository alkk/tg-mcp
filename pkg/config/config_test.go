package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    map[int64]ChatInfo
		wantErr string
	}{
		{
			name: "valid, single and multi group customers",
			yaml: `
chats:
  -1001234567890:
    customer: acme
  -1009876543210:
    customer: globex
    label: main
  -1005555555555:
    customer: globex
    label: escalations
`,
			want: map[int64]ChatInfo{
				-1001234567890: {Customer: "acme"},
				-1009876543210: {Customer: "globex", Label: "main"},
				-1005555555555: {Customer: "globex", Label: "escalations"},
			},
		},
		{
			name: "empty chat map is allowed, ids not known yet",
			yaml: "chats:\n",
			want: map[int64]ChatInfo{},
		},
		{
			name: "empty file",
			yaml: "",
			want: map[int64]ChatInfo{},
		},
		{
			name: "same label for different customers",
			yaml: `
chats:
  -1001:
    customer: acme
    label: main
  -1002:
    customer: globex
    label: main
`,
			want: map[int64]ChatInfo{
				-1001: {Customer: "acme", Label: "main"},
				-1002: {Customer: "globex", Label: "main"},
			},
		},
		{
			name: "empty customer slug",
			yaml: `
chats:
  -1001:
    label: main
`,
			wantErr: "chat -1001: empty customer slug",
		},
		{
			name: "duplicate label within customer",
			yaml: `
chats:
  -1001:
    customer: acme
    label: main
  -1002:
    customer: acme
    label: main
`,
			wantErr: `customer "acme": label "main" used by both chat -1002 and -1001`,
		},
		{
			name: "several chats of one customer without labels",
			yaml: `
chats:
  -1001:
    customer: acme
  -1002:
    customer: acme
`,
			wantErr: `customer "acme": chat -1002 has no label`,
		},
		{
			name: "one labeled and one unlabeled chat of the same customer",
			yaml: `
chats:
  -1001:
    customer: acme
  -1002:
    customer: acme
    label: dev
`,
			wantErr: `customer "acme": chat -1001 has no label`,
		},
		{
			name:    "unknown field",
			yaml:    "chats:\n  -1001:\n    customer: acme\n    labl: main\n",
			wantErr: "parse chat map",
		},
		{
			name:    "malformed yaml",
			yaml:    "chats: [oops\n",
			wantErr: "parse chat map",
		},
		{
			name:    "non-numeric chat id",
			yaml:    "chats:\n  acme-group:\n    customer: acme\n",
			wantErr: "parse chat map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "chats.yml")
			require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0o600))

			cfg, err := Load(path)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, cfg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.chats)
		})
	}
}

func TestLoad_missingFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open chat map")
	assert.Nil(t, cfg)
}

func TestLoad_exampleFile(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "chats.example.yml"))
	require.NoError(t, err)
	assert.Equal(t, []string{"acme", "globex"}, cfg.Customers())
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{chats: map[int64]ChatInfo{
		-1001: {Customer: "acme"},
		-1002: {Customer: "globex", Label: "main"},
		-1003: {Customer: "globex", Label: "escalations"},
	}}
}

func TestConfig_ByChat(t *testing.T) {
	cfg := testConfig(t)

	tests := []struct {
		name   string
		chatID int64
		want   ChatInfo
		wantOK bool
	}{
		{name: "known chat without label", chatID: -1001, want: ChatInfo{Customer: "acme"}, wantOK: true},
		{name: "known chat with label", chatID: -1002, want: ChatInfo{Customer: "globex", Label: "main"}, wantOK: true},
		{name: "unknown chat", chatID: -9999, want: ChatInfo{}, wantOK: false},
		{name: "zero chat id", chatID: 0, want: ChatInfo{}, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, ok := cfg.ByChat(tt.chatID)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, info)
		})
	}
}

func TestConfig_ByCustomer(t *testing.T) {
	cfg := testConfig(t)

	tests := []struct {
		name     string
		customer string
		want     []Chat
	}{
		{
			name:     "single chat",
			customer: "acme",
			want:     []Chat{{ID: -1001, ChatInfo: ChatInfo{Customer: "acme"}}},
		},
		{
			name:     "several chats ordered by label",
			customer: "globex",
			want: []Chat{
				{ID: -1003, ChatInfo: ChatInfo{Customer: "globex", Label: "escalations"}},
				{ID: -1002, ChatInfo: ChatInfo{Customer: "globex", Label: "main"}},
			},
		},
		{name: "unknown customer", customer: "initech", want: nil},
		{name: "empty slug", customer: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cfg.ByCustomer(tt.customer))
		})
	}
}

func TestConfig_Customers(t *testing.T) {
	assert.Equal(t, []string{"acme", "globex"}, testConfig(t).Customers())
	assert.Empty(t, (&Config{chats: map[int64]ChatInfo{}}).Customers())
}

func TestConfig_All(t *testing.T) {
	want := []Chat{
		{ID: -1001, ChatInfo: ChatInfo{Customer: "acme"}},
		{ID: -1003, ChatInfo: ChatInfo{Customer: "globex", Label: "escalations"}},
		{ID: -1002, ChatInfo: ChatInfo{Customer: "globex", Label: "main"}},
	}
	assert.Equal(t, want, testConfig(t).All())
	assert.Empty(t, (&Config{chats: map[int64]ChatInfo{}}).All())
}
