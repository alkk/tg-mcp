package server

import (
	"context"
	cryptotls "crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alkk/tg-mcp/pkg/server/mocks"
	"github.com/alkk/tg-mcp/pkg/store"
	"github.com/alkk/tg-mcp/pkg/telegram"
)

// pngData is a tiny but valid-looking png: the signature is all http.DetectContentType needs.
var pngData = append([]byte("\x89PNG\r\n\x1a\n"), []byte("pixels")...)

// fileServer seeds one message per attachment shape and wires the given fake bot api to it.
func fileServer(t *testing.T, tg *mocks.TelegramAPI) *Server {
	t.Helper()

	st, err := store.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	base := seedBase()
	msgs := []store.Message{
		{ChatID: -1001, MessageID: 1, Sent: base, SenderID: 11, SenderName: "alice", Text: "screenshot",
			MediaType: "photo", FileID: "f1", FileUniqueID: "u1", FileName: "shot.png", FileSize: int64(len(pngData))},
		{ChatID: -1001, MessageID: 2, Sent: base.Add(time.Minute), SenderID: 11, SenderName: "alice",
			Text: "log", MediaType: "document", FileID: "f2", FileUniqueID: "u2", FileName: "server.log"},
		{ChatID: -1001, MessageID: 3, Sent: base.Add(2 * time.Minute), SenderID: 11, SenderName: "alice",
			Text: "no attachment here"},
		{ChatID: -1001, MessageID: 4, Sent: base.Add(3 * time.Minute), SenderID: 11, SenderName: "alice",
			Text: "core dump", MediaType: "document", FileID: "f4", FileUniqueID: "u4", FileName: "core.bin"},
		{ChatID: -1001, MessageID: 5, Sent: base.Add(4 * time.Minute), SenderID: 11, SenderName: "alice",
			Text: "tiny binary", MediaType: "document", FileID: "f5", FileUniqueID: "u5", FileName: "blob.bin"},
		{ChatID: -1002, MessageID: 10, Sent: base.Add(5 * time.Minute), SenderID: 21, SenderName: "carol",
			Text: "invoice", MediaType: "document", FileID: "f10", FileUniqueID: "u10", FileName: "invoice.pdf"},
	}
	require.NoError(t, st.UpsertBatch(context.Background(), msgs))

	p := Params{Store: st, Chats: testConfig(t, chatMap), AuthToken: testToken, Listen: "127.0.0.1:0"}
	if tg != nil { // a nil mock must stay a nil interface, that is the unconfigured case
		p.Telegram = tg
	}
	srv, err := New(p)
	require.NoError(t, err)
	return srv
}

// fakeAPI serves the given payload per file id, recording how often getFile was called.
func fakeAPI(payloads map[string][]byte) *mocks.TelegramAPI {
	paths := map[string]string{}
	for id := range payloads {
		paths["/tg/"+id] = id
	}
	return &mocks.TelegramAPI{
		GetFileFunc: func(_ context.Context, fileID string) (telegram.File, error) {
			if _, ok := payloads[fileID]; !ok {
				return telegram.File{}, errors.New("file is not available")
			}
			return telegram.File{FileID: fileID, FilePath: "/tg/" + fileID}, nil
		},
		DownloadFunc: func(_ context.Context, filePath string, dst io.Writer) error {
			id, ok := paths[filePath]
			if !ok {
				return errors.New("no such path")
			}
			_, _ = dst.Write(payloads[id])
			return nil
		},
	}
}

func TestToolsGetFile(t *testing.T) {
	ctx := context.Background()
	payloads := map[string][]byte{
		"f1":  pngData,
		"f2":  []byte("nxagentd: connection refused\n"),
		"f4":  make([]byte, inlineLimit+1),
		"f5":  {0x00, 0x01, 0x02, 0xff, 0xfe},
		"f10": []byte("%PDF-1.4 invoice"),
	}

	t.Run("image comes back inline", func(t *testing.T) {
		tg := fakeAPI(payloads)
		s := fileServer(t, tg)

		res, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 1})
		require.NoError(t, err)
		assert.True(t, out.Inline)
		assert.Equal(t, "image/png", out.MimeType)
		assert.Equal(t, "shot.png", out.FileName)
		assert.Equal(t, int64(len(pngData)), out.FileSize)
		assert.Equal(t, "photo", out.Media)
		assert.Equal(t, "acme", out.Customer)
		assert.Empty(t, out.URL)

		require.NotNil(t, res)
		require.Len(t, res.Content, 1)
		img, ok := res.Content[0].(*mcp.ImageContent)
		require.True(t, ok, "unexpected content type %T", res.Content[0])
		assert.Equal(t, pngData, img.Data)
		assert.Equal(t, "image/png", img.MIMEType)
		assert.Len(t, tg.GetFileCalls(), 1)
	})

	t.Run("second call is served from the cache", func(t *testing.T) {
		tg := fakeAPI(payloads)
		s := fileServer(t, tg)

		_, _, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 1})
		require.NoError(t, err)
		_, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 1})
		require.NoError(t, err)

		assert.True(t, out.Inline)
		assert.Len(t, tg.GetFileCalls(), 1, "a cached attachment is never downloaded twice")
		assert.Len(t, tg.DownloadCalls(), 1)
	})

	t.Run("text comes back as text", func(t *testing.T) {
		s := fileServer(t, fakeAPI(payloads))

		res, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 2})
		require.NoError(t, err)
		assert.True(t, out.Inline)
		assert.True(t, strings.HasPrefix(out.MimeType, "text/"),
			"system mime tables map .log differently across platforms, got %q", out.MimeType)

		require.NotNil(t, res)
		require.Len(t, res.Content, 1)
		text, ok := res.Content[0].(*mcp.TextContent)
		require.True(t, ok, "unexpected content type %T", res.Content[0])
		assert.Equal(t, "nxagentd: connection refused\n", text.Text)
	})

	t.Run("large file is served over http instead", func(t *testing.T) {
		s := fileServer(t, fakeAPI(payloads))

		res, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 4})
		require.NoError(t, err)
		assert.Nil(t, res, "the bytes stay out of the tool result")
		assert.False(t, out.Inline)
		assert.Equal(t, "/files/u4", out.URL)
		assert.Equal(t, int64(inlineLimit+1), out.FileSize, "the size is taken from the cached file")
	})

	t.Run("small binary is served over http too", func(t *testing.T) {
		s := fileServer(t, fakeAPI(payloads))

		_, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 5})
		require.NoError(t, err)
		assert.False(t, out.Inline)
		assert.Equal(t, "/files/u5", out.URL)
	})

	t.Run("labels the group of a multi group customer", func(t *testing.T) {
		s := fileServer(t, fakeAPI(payloads))

		_, out, err := s.getFile(ctx, nil, getFileParams{Customer: "globex", Label: "main", MessageID: 10})
		require.NoError(t, err)
		assert.Equal(t, "globex", out.Customer)
		assert.Equal(t, "main", out.Label)
		assert.Equal(t, "invoice.pdf", out.FileName)
	})

	t.Run("errors", func(t *testing.T) {
		s := fileServer(t, fakeAPI(payloads))
		tests := []struct {
			name    string
			params  getFileParams
			wantErr string
		}{
			{name: "message without attachment", params: getFileParams{Customer: "acme", MessageID: 3},
				wantErr: "message 3 carries no attachment"},
			{name: "unknown message", params: getFileParams{Customer: "acme", MessageID: 99},
				wantErr: "message not found"},
			{name: "unknown customer", params: getFileParams{Customer: "initech", MessageID: 1},
				wantErr: `unknown customer "initech"`},
			{name: "ambiguous customer", params: getFileParams{Customer: "globex", MessageID: 10},
				wantErr: "pass label"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				_, _, err := s.getFile(ctx, nil, tt.params)
				require.ErrorContains(t, err, tt.wantErr)
			})
		}
	})

	t.Run("cloud size limit is surfaced", func(t *testing.T) {
		s := fileServer(t, &mocks.TelegramAPI{
			GetFileFunc: func(context.Context, string) (telegram.File, error) {
				return telegram.File{}, errors.New("telegram getFile failed: 400 Bad Request: file is too big: " +
					"the cloud bot api caps downloads at 20 MB, a self-hosted api server lifts the limit")
			},
		})

		_, _, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 1})
		require.ErrorContains(t, err, "resolve attachment of message 1")
		assert.Contains(t, err.Error(), "caps downloads at 20 MB")
	})

	t.Run("download failure leaves nothing cached", func(t *testing.T) {
		tg := fakeAPI(payloads)
		tg.DownloadFunc = func(context.Context, string, io.Writer) error { return errors.New("connection reset") }
		s := fileServer(t, tg)

		_, _, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 1})
		require.ErrorContains(t, err, "cache attachment of message 1")

		_, _, err = s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 1})
		require.Error(t, err, "the failed download is not cached as an empty file")
		assert.Len(t, tg.GetFileCalls(), 2)
	})

	t.Run("no telegram client", func(t *testing.T) {
		s := fileServer(t, nil)
		_, _, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 1})
		require.ErrorContains(t, err, "no telegram client configured")
	})
}

func TestServeFile(t *testing.T) {
	ctx := context.Background()
	svgData := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`)
	s := fileServer(t, fakeAPI(map[string][]byte{"f2": []byte("nxagentd: connection refused\n"), "f11": svgData}))
	_, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 2})
	require.NoError(t, err)
	require.True(t, out.Inline)

	require.NoError(t, s.store.UpsertMessage(ctx, store.Message{
		ChatID: -1001, MessageID: 11, Sent: seedBase().Add(6 * time.Minute), SenderName: "alice", Text: "diagram",
		MediaType: "document", FileID: "f11", FileUniqueID: "u11", FileName: "diagram.svg"}))
	_, _, err = s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 11})
	require.NoError(t, err)

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	get := func(t *testing.T, path, token string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, http.NoBody)
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		return resp
	}

	t.Run("serves a cached file", func(t *testing.T) {
		resp := get(t, "/files/u2", testToken)
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "nxagentd: connection refused\n", string(body))
		assert.Equal(t, "attachment", resp.Header.Get("Content-Disposition"),
			"no filename= — the consumer gets the name from get_file")
		assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"),
			"customer bytes must never be sniffed into an active type on this origin")
	})

	t.Run("content type is sniffed, not derived from the name", func(t *testing.T) {
		resp := get(t, "/files/u11", testToken)
		defer resp.Body.Close()

		// ServeContent gets no name, so an .svg is typed by its bytes: accepted, since
		// attachment plus nosniff means it is saved either way and get_file reports the
		// name-derived type the harness actually reads
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/xml; charset=utf-8", resp.Header.Get("Content-Type"))
		assert.Equal(t, "attachment", resp.Header.Get("Content-Disposition"))
		assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	})

	t.Run("unknown id", func(t *testing.T) {
		resp := get(t, "/files/nope", testToken)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("without token", func(t *testing.T) {
		resp := get(t, "/files/u2", "")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
	})

	t.Run("wrong token", func(t *testing.T) {
		resp := get(t, "/files/u2", "nope")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

// TestFileURLFollowsTheClient checks that download links point back at the host the client of that
// very call reached us through, proxy included.
func TestFileURLFollowsTheClient(t *testing.T) {
	s := newServer(t)
	assert.Equal(t, "/files/u1", fileURL(nil, "u1"), "no request behind the call")
	assert.Equal(t, "/files/u1", fileURL(&mcp.CallToolRequest{}, "u1"), "no transport metadata")

	// call runs a request through the mcp middleware and returns the tool call it would carry
	call := func(host string, headers map[string]string, tls bool) *mcp.CallToolRequest {
		req := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+testToken)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if tls {
			req.TLS = &cryptotls.ConnectionState{}
		}

		var seen http.Header
		s.auth(s.trackBase(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = r.Header.Clone()
		}))).ServeHTTP(httptest.NewRecorder(), req)
		return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: seen}}
	}

	plain := call("tg.example.com", nil, false)
	assert.Equal(t, "http://tg.example.com/files/u1", fileURL(plain, "u1"))

	assert.Equal(t, "https://tg.example.com/files/u1", fileURL(call("tg.example.com", nil, true), "u1"))

	proxied := call("tg.example.com",
		map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "support.example.com, tg.example.com"}, false)
	assert.Equal(t, "https://support.example.com/files/u1", fileURL(proxied, "u1"))
	assert.Equal(t, "https://support.example.com/files/a%2Fb", fileURL(proxied, "a/b"), "ids are escaped")

	// a second client on another hostname builds its own urls and leaves the first one alone
	other := call("internal.example.com", nil, false)
	assert.Equal(t, "http://internal.example.com/files/u1", fileURL(other, "u1"))
	assert.Equal(t, "https://support.example.com/files/u1", fileURL(proxied, "u1"),
		"a concurrent call cannot repoint the links of this one")

	spoofed := call("tg.example.com", map[string]string{baseHeader: "https://evil.example.com"}, false)
	assert.Equal(t, "http://tg.example.com/files/u1", fileURL(spoofed, "u1"),
		"a base the client sent itself never survives")

	mounted := call("tg.example.com", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "support.example.com",
		"X-Forwarded-Prefix": "/tg-mcp/",
	}, false)
	assert.Equal(t, "https://support.example.com/tg-mcp/files/u1", fileURL(mounted, "u1"),
		"downloads stay inside the mount the proxy announced")
}

func TestForwardedPrefix(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "absent"},
		{name: "plain", header: "/tg-mcp", want: "/tg-mcp"},
		{name: "trailing slash", header: "/tg-mcp/", want: "/tg-mcp"},
		{name: "padded", header: "  /tg-mcp  ", want: "/tg-mcp"},
		{name: "chained", header: "/tg-mcp, /inner", want: "/tg-mcp"},
		{name: "nested", header: "/support/tg-mcp", want: "/support/tg-mcp"},
		{name: "escaped", header: "/tg mcp", want: "/tg%20mcp"},
		{name: "root only", header: "/"},
		{name: "relative", header: "tg-mcp"},
		{name: "absolute url", header: "https://evil.example.com/tg-mcp"},
		{name: "protocol relative", header: "//evil.example.com"},
		{name: "triple slash", header: "///evil.example.com"},
		{name: "embedded double slash", header: "/tg-mcp//inner"},
		{name: "encoded slash authority", header: "%2F@evil.example.com"},
		{name: "encoded slash", header: "%2Ftg-mcp"},
		{name: "dot dot", header: "/tg-mcp/.."},
		{name: "encoded dot dot", header: "/tg-mcp/%2e%2e"},
		{name: "single dot", header: "/tg-mcp/./inner"},
		{name: "query", header: "/tg-mcp?a=b"},
		{name: "fragment", header: "/tg-mcp#frag"},
		{name: "control character", header: "/tg\x7fmcp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
			if tt.header != "" {
				req.Header.Set("X-Forwarded-Prefix", tt.header)
			}
			assert.Equal(t, tt.want, forwardedPrefix(req))
		})
	}
}

func TestInlineContentUnreadableFile(t *testing.T) {
	_, _, err := inlineContent(t.TempDir()+"/missing.txt", "missing.txt", 10)
	require.ErrorContains(t, err, "read cached file")
}

func TestIsTextual(t *testing.T) {
	tests := []struct {
		mimeType string
		want     bool
	}{
		{mimeType: "text/plain", want: true},
		{mimeType: "application/json", want: true},
		{mimeType: "application/xml", want: true},
		{mimeType: "application/yaml", want: true},
		{mimeType: "text/javascript", want: true},
		{mimeType: "application/x-sh", want: true},
		{mimeType: "image/png"},
		{mimeType: "application/octet-stream"},
		{mimeType: ""},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			assert.Equal(t, tt.want, isTextual(tt.mimeType))
		})
	}
}

func TestBaseType(t *testing.T) {
	assert.Equal(t, "text/plain", baseType("text/plain; charset=utf-8"))
	assert.Equal(t, "image/png", baseType("image/png"))
	assert.Empty(t, baseType(""))
}

func TestRequestBaseIgnoresHostlessRequests(t *testing.T) {
	s := newServer(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody)
	req.Host = ""
	req.Header.Set(baseHeader, "https://evil.example.com")
	assert.Empty(t, requestBase(req))

	var seen http.Header
	s.trackBase(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	})).ServeHTTP(httptest.NewRecorder(), req)
	assert.Equal(t, "/files/u1", fileURL(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: seen}}, "u1"),
		"a hostless request clears the header instead of trusting it")
}

func TestFileNameFallback(t *testing.T) {
	tg := fakeAPI(map[string][]byte{"f1": pngData})
	s := fileServer(t, tg)

	// a message logged without a file name falls back to its unique id: telegram's file path is
	// only known on a cache miss, so reporting it would make the name cache-dependent
	require.NoError(t, s.store.UpsertMessage(context.Background(), store.Message{
		ChatID: -1001, MessageID: 7, Sent: seedBase(), SenderName: "alice", Text: "shot",
		MediaType: "photo", FileID: "f1", FileUniqueID: "u7",
	}))

	_, out, err := s.getFile(context.Background(), nil, getFileParams{Customer: "acme", MessageID: 7})
	require.NoError(t, err)
	assert.Equal(t, "u7", out.FileName, "the unique id is the last resort")
	assert.True(t, strings.HasPrefix(out.MimeType, "image/"), "the type is sniffed when the name has no extension")
}

// TestFileNamePerMessage is the regression for the layout this cache replaced: file_unique_id
// keys bytes, not names, so the same file resent under a second name used to leave two files in
// one directory and let ReadDir order decide which name get_file reported.
func TestFileNamePerMessage(t *testing.T) {
	ctx := context.Background()
	tg := fakeAPI(map[string][]byte{"f1": pngData})
	s := fileServer(t, tg)

	for _, m := range []store.Message{
		{ChatID: -1001, MessageID: 8, Sent: seedBase(), SenderName: "alice", Text: "shot",
			MediaType: "photo", FileID: "f1", FileUniqueID: "ur", FileName: "first.png"},
		{ChatID: -1001, MessageID: 9, Sent: seedBase().Add(time.Minute), SenderName: "bob", Text: "same shot",
			MediaType: "photo", FileID: "f1", FileUniqueID: "ur", FileName: "second.png"},
	} {
		require.NoError(t, s.store.UpsertMessage(ctx, m))
	}

	_, first, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 8})
	require.NoError(t, err)
	assert.Equal(t, "first.png", first.FileName)

	_, second, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 9})
	require.NoError(t, err)
	assert.Equal(t, "second.png", second.FileName, "each message reports the name it carries")
	assert.Len(t, tg.GetFileCalls(), 1, "one id is one cache entry, whatever it was named")
}
