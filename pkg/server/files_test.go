package server

import (
	"context"
	cryptotls "crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
		assert.Equal(t, "/files/u4", signedPath(t, s.linkKey, out.URL, "u4"))
		assert.Equal(t, int64(inlineLimit+1), out.FileSize, "the size is taken from the cached file")
	})

	t.Run("small binary is served over http too", func(t *testing.T) {
		s := fileServer(t, fakeAPI(payloads))

		_, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 5})
		require.NoError(t, err)
		assert.False(t, out.Inline)
		assert.Equal(t, "/files/u5", signedPath(t, s.linkKey, out.URL, "u5"),
			"the link carries a signature the server itself minted")
	})

	t.Run("the download link is signed for the configured window", func(t *testing.T) {
		st, err := store.New(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, st.Close()) })
		require.NoError(t, st.UpsertMessage(ctx, store.Message{ChatID: -1001, MessageID: 5, Sent: seedBase(),
			SenderName: "alice", MediaType: "document", FileID: "f5", FileUniqueID: "u5", FileName: "blob.bin"}))
		s, err := New(Params{Store: st, Chats: testConfig(t, chatMap), AuthToken: testToken,
			Listen: "127.0.0.1:0", Telegram: fakeAPI(payloads), FileLinkTTL: time.Minute})
		require.NoError(t, err)

		before := time.Now().Add(time.Minute).Unix()
		_, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 5})
		require.NoError(t, err)
		after := time.Now().Add(time.Minute).Unix()

		u, err := url.Parse(out.URL)
		require.NoError(t, err)
		exp, err := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, exp, before)
		assert.LessOrEqual(t, exp, after, "the expiry is now plus the configured ttl")
		assert.Equal(t, signFileID(s.linkKey, "u5", exp), u.Query().Get("sig"),
			"the signature verifies against the key the server derived from its own token")
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
	s := fileServer(t, fakeAPI(map[string][]byte{"f2": []byte("nxagentd: connection refused\n"), "f11": svgData,
		"f12": []byte("odd bytes")}))
	_, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 2})
	require.NoError(t, err)
	require.True(t, out.Inline)

	require.NoError(t, s.store.UpsertMessage(ctx, store.Message{
		ChatID: -1001, MessageID: 11, Sent: seedBase().Add(6 * time.Minute), SenderName: "alice", Text: "diagram",
		MediaType: "document", FileID: "f11", FileUniqueID: "u11", FileName: "diagram.svg"}))
	_, svg, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 11})
	require.NoError(t, err)
	require.False(t, svg.Inline, "svg takes the link path, so it is cached rather than inlined")

	require.NoError(t, s.store.UpsertMessage(ctx, store.Message{
		ChatID: -1001, MessageID: 12, Sent: seedBase().Add(7 * time.Minute), SenderName: "alice", Text: "odd id",
		MediaType: "document", FileID: "f12", FileUniqueID: "u/12", FileName: "odd.bin"}))
	_, odd, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: 12})
	require.NoError(t, err)
	require.NotEmpty(t, odd.URL)

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
		assert.Equal(t, "private, no-store", resp.Header.Get("Cache-Control"),
			"a signed url is a credential in the query, so no shared cache may keep the bytes")
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

	t.Run("a signed url serves without a token", func(t *testing.T) {
		resp := get(t, fileURL(nil, "u2", time.Now().Add(time.Minute).Unix(), s.linkKey), "")
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "nxagentd: connection refused\n", string(body))
		assert.Equal(t, "attachment", resp.Header.Get("Content-Disposition"))
	})

	t.Run("an expired signature is a 410", func(t *testing.T) {
		resp := get(t, fileURL(nil, "u2", time.Now().Add(-time.Minute).Unix(), s.linkKey), "")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusGone, resp.StatusCode)
	})

	t.Run("a tampered signature is a 401", func(t *testing.T) {
		link := fileURL(nil, "u2", time.Now().Add(time.Minute).Unix(), s.linkKey)
		resp := get(t, strings.TrimSuffix(link, "0")+"1", "")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("an unknown id with a valid signature is still a 404", func(t *testing.T) {
		resp := get(t, fileURL(nil, "nope", time.Now().Add(time.Minute).Unix(), s.linkKey), "")
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("an id needing escaping round-trips through the route", func(t *testing.T) {
		resp := get(t, odd.URL, "")
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "odd bytes", string(body), "the signature covers the id the store is given")
	})
}

// signedPath asserts a download link carries a live signature over the given id and returns the
// link without its query, so the path assertions stay readable.
func signedPath(t *testing.T, key []byte, link, id string) string {
	t.Helper()

	path, query, found := strings.Cut(link, "?")
	require.True(t, found, "link %q carries no query", link)
	q, err := url.ParseQuery(query)
	require.NoError(t, err)

	_, expired, ok := verifyFileSig(key, id, q.Get("exp"), q.Get("sig"))
	assert.True(t, ok, "the signature does not verify")
	assert.False(t, expired, "a freshly minted link is already expired")
	return path
}

// TestFileURLFollowsTheClient checks that download links point back at the host the client of that
// very call reached us through, proxy included.
func TestFileURLFollowsTheClient(t *testing.T) {
	s := newServer(t)
	exp := time.Now().Add(time.Minute).Unix()
	link := func(req *mcp.CallToolRequest, id string) string {
		return signedPath(t, s.linkKey, fileURL(req, id, exp, s.linkKey), id)
	}

	assert.Equal(t, "/files/u1", link(nil, "u1"), "no request behind the call")
	assert.Equal(t, "/files/u1", link(&mcp.CallToolRequest{}, "u1"), "no transport metadata")

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
	assert.Equal(t, "http://tg.example.com/files/u1", link(plain, "u1"))

	assert.Equal(t, "https://tg.example.com/files/u1", link(call("tg.example.com", nil, true), "u1"))

	proxied := call("tg.example.com",
		map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "support.example.com, tg.example.com"}, false)
	assert.Equal(t, "https://support.example.com/files/u1", link(proxied, "u1"))
	assert.Equal(t, "https://support.example.com/files/a%2Fb", link(proxied, "a/b"), "ids are escaped")

	// a second client on another hostname builds its own urls and leaves the first one alone
	other := call("internal.example.com", nil, false)
	assert.Equal(t, "http://internal.example.com/files/u1", link(other, "u1"))
	assert.Equal(t, "https://support.example.com/files/u1", link(proxied, "u1"),
		"a concurrent call cannot repoint the links of this one")

	spoofed := call("tg.example.com", map[string]string{baseHeader: "https://evil.example.com"}, false)
	assert.Equal(t, "http://tg.example.com/files/u1", link(spoofed, "u1"),
		"a base the client sent itself never survives")

	mounted := call("tg.example.com", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "support.example.com",
		"X-Forwarded-Prefix": "/tg-mcp/",
	}, false)
	assert.Equal(t, "https://support.example.com/tg-mcp/files/u1", link(mounted, "u1"),
		"downloads stay inside the mount the proxy announced")
}

// TestFileURLCarriesItsCredential pins the query the harness fetches with: the id it names is the
// one that is signed, and the host it was built for is not covered at all.
func TestFileURLCarriesItsCredential(t *testing.T) {
	key := deriveLinkKey("secret")
	exp := time.Now().Add(5 * time.Minute).Unix()

	raw := fileURL(nil, "a/b", exp, key)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "/files/a%2Fb", u.EscapedPath(), "the id stays escaped in the path")
	assert.Equal(t, "/files/a/b", u.Path)
	assert.Equal(t, strconv.FormatInt(exp, 10), u.Query().Get("exp"))
	assert.Equal(t, signFileID(key, "a/b", exp), u.Query().Get("sig"),
		"the signature covers the decoded id the route hands to the store")

	_, expired, ok := verifyFileSig(key, "a/b", u.Query().Get("exp"), u.Query().Get("sig"))
	assert.True(t, ok)
	assert.False(t, expired)

	_, _, forged := verifyFileSig(deriveLinkKey("other"), "a/b", u.Query().Get("exp"), u.Query().Get("sig"))
	assert.False(t, forged, "another server's key does not verify our links")

	withBase := fileURL(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{
		Header: http.Header{baseHeader: {"https://support.example.com"}}}}, "u1", exp, key)
	assert.Equal(t, "https://support.example.com/files/u1?exp="+strconv.FormatInt(exp, 10)+
		"&sig="+signFileID(key, "u1", exp), withBase,
		"only the id and the expiry are signed, so the same signature holds behind any host")

	past := fileURL(nil, "u1", time.Now().Add(-time.Minute).Unix(), key)
	pastQuery, err := url.Parse(past)
	require.NoError(t, err)
	_, expired, ok = verifyFileSig(key, "u1", pastQuery.Query().Get("exp"), pastQuery.Query().Get("sig"))
	assert.True(t, ok, "an expired link is still authentic")
	assert.True(t, expired)
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

// TestToolsGetFileImageTypes pins which image types ride inside the tool result and which take
// the download link instead, name and bytes both. Only the branch is asserted, never the resolved
// subtype: the system mime table differs across platforms for everything outside go's builtin set.
func TestToolsGetFileImageTypes(t *testing.T) {
	ctx := context.Background()
	// binary bytes for every type meant to take the link path, so an extension the platform mime
	// table does not know cannot sniff into text/plain and inline through the text arm instead
	binary := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	svgData := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`)

	tests := []struct {
		name   string
		data   []byte
		inline bool
	}{
		{name: "photo.jpg", data: pngData, inline: true},
		{name: "shot.png", data: pngData, inline: true},
		{name: "anim.gif", data: pngData, inline: true},
		{name: "sticker.webp", data: pngData, inline: true},
		{name: "phone.heic", data: binary},
		{name: "diagram.svg", data: svgData},
		{name: "scan.bmp", data: binary},
		{name: "fax.tif", data: binary},
		{name: "photo.avif", data: binary},
		{name: "favicon.ico", data: binary},
		{name: "trojan.png", data: append([]byte("PK\x03\x04"), binary...)}, // a zip under an image name
		{name: "", data: append([]byte("BM"), binary...)},                   // no extension, sniffed as image/bmp
	}

	st, err := store.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	payloads := map[string][]byte{}
	for i, tt := range tests {
		id := strconv.Itoa(i)
		payloads["f"+id] = tt.data
		require.NoError(t, st.UpsertMessage(ctx, store.Message{ChatID: -1001, MessageID: int64(100 + i),
			Sent: seedBase().Add(time.Duration(i) * time.Minute), SenderName: "alice", MediaType: "document",
			FileID: "f" + id, FileUniqueID: "u" + id, FileName: tt.name}))
	}
	s, err := New(Params{Store: st, Chats: testConfig(t, chatMap), AuthToken: testToken,
		Listen: "127.0.0.1:0", Telegram: fakeAPI(payloads)})
	require.NoError(t, err)

	for i, tt := range tests {
		name := tt.name
		if name == "" {
			name = "no extension"
		}
		t.Run(name, func(t *testing.T) {
			res, out, err := s.getFile(ctx, nil, getFileParams{Customer: "acme", MessageID: int64(100 + i)})
			require.NoError(t, err)

			if !tt.inline {
				assert.False(t, out.Inline, "mime %q must take the link path", out.MimeType)
				assert.Nil(t, res, "the bytes stay out of the tool result")
				assert.Equal(t, "/files/u"+strconv.Itoa(i), signedPath(t, s.linkKey, out.URL, "u"+strconv.Itoa(i)))
				return
			}

			assert.True(t, out.Inline, "mime %q must inline", out.MimeType)
			assert.Empty(t, out.URL, "an inlined image carries no second route to the same bytes")
			require.NotNil(t, res)
			require.Len(t, res.Content, 1)
			img, ok := res.Content[0].(*mcp.ImageContent)
			require.True(t, ok, "unexpected content type %T", res.Content[0])
			assert.Equal(t, tt.data, img.Data)
		})
	}
}

func TestIsInlineImage(t *testing.T) {
	tests := []struct {
		mimeType string
		want     bool
	}{
		{mimeType: "image/jpeg", want: true},
		{mimeType: "image/png", want: true},
		{mimeType: "image/gif", want: true},
		{mimeType: "image/webp", want: true},
		{mimeType: "image/svg+xml"},
		{mimeType: "image/heic"},
		{mimeType: "image/bmp"},
		{mimeType: "image/tiff"},
		{mimeType: "image/avif"},
		{mimeType: "image/vnd.microsoft.icon"},
		{mimeType: "image/png; charset=binary"}, // parameters are stripped before the lookup
		{mimeType: "text/plain"},
		{mimeType: ""},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			assert.Equal(t, tt.want, isInlineImage(tt.mimeType))
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
	link := fileURL(&mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: seen}}, "u1",
		time.Now().Add(time.Minute).Unix(), s.linkKey)
	assert.Equal(t, "/files/u1", signedPath(t, s.linkKey, link, "u1"),
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

func TestDeriveLinkKey(t *testing.T) {
	key := deriveLinkKey("secret")
	assert.Len(t, key, 32)
	assert.Equal(t, key, deriveLinkKey("secret"), "same token derives the same key")
	assert.NotEqual(t, key, deriveLinkKey("other"), "the key rotates with the token")
	assert.NotEqual(t, []byte("secret"), key, "the key is not the token itself")
}

func TestSignFileID(t *testing.T) {
	key := deriveLinkKey("secret")
	sig := signFileID(key, "AgADuQ", 1700000000)

	assert.Len(t, sig, 2*sigBytes)
	assert.Equal(t, sig, signFileID(key, "AgADuQ", 1700000000), "stable for the same inputs")
	assert.NotEqual(t, sig, signFileID(key, "AgADuR", 1700000000), "covers the id")
	assert.NotEqual(t, sig, signFileID(key, "AgADuQ", 1700000001), "covers the expiry")
	assert.NotEqual(t, sig, signFileID(deriveLinkKey("other"), "AgADuQ", 1700000000), "covers the key")
	assert.NotEqual(t, signFileID(key, "a", 12), signFileID(key, "a\n1", 2),
		"the separator keeps the mac input injective")
}

func TestVerifyFileSig(t *testing.T) {
	key := deriveLinkKey("secret")
	const id = "AgADuQ"
	live := time.Now().Add(time.Minute).Unix()
	dead := time.Now().Add(-time.Minute).Unix()

	tests := []struct {
		name            string
		id, expRaw, sig string
		wantExp         int64
		expired, ok     bool
	}{
		{name: "valid and unexpired", id: id, expRaw: strconv.FormatInt(live, 10),
			sig: signFileID(key, id, live), wantExp: live, ok: true},
		{name: "valid but expired", id: id, expRaw: strconv.FormatInt(dead, 10),
			sig: signFileID(key, id, dead), wantExp: dead, expired: true, ok: true},
		{name: "tampered sig", id: id, expRaw: strconv.FormatInt(live, 10),
			sig: strings.Repeat("0", 2*sigBytes)},
		{name: "tampered id", id: "AgADuR", expRaw: strconv.FormatInt(live, 10),
			sig: signFileID(key, id, live)},
		{name: "tampered exp", id: id, expRaw: strconv.FormatInt(live+1, 10),
			sig: signFileID(key, id, live)},
		{name: "forged and expired reports only forgery", id: id, expRaw: strconv.FormatInt(dead, 10),
			sig: strings.Repeat("0", 2*sigBytes)},
		{name: "non-numeric exp", id: id, expRaw: "soon", sig: signFileID(key, id, live)},
		{name: "empty exp", id: id, sig: signFileID(key, id, live)},
		{name: "empty sig", id: id, expRaw: strconv.FormatInt(live, 10)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp, expired, ok := verifyFileSig(key, tt.id, tt.expRaw, tt.sig)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.expired, expired)
			if tt.ok {
				assert.Equal(t, tt.wantExp, exp, "the verified expiry comes back for the 410 body")
			}
		})
	}
}

// pngOf is a distinct valid-looking png per marker, so a content block can be traced back to the
// message whose attachment it came from.
func pngOf(marker string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), marker...)
}

// threadMsg builds a message carrying one attachment, named after its media type by default the
// way telegram names a photo.
func threadMsg(id int64, media, name string, size int64) store.Message {
	sfx := strconv.FormatInt(id, 10)
	return store.Message{ChatID: -1001, MessageID: id, Sent: seedBase().Add(time.Duration(id) * time.Minute),
		SenderID: 11, SenderName: "alice", MediaType: media, FileID: "f" + sfx, FileUniqueID: "u" + sfx,
		FileName: name, FileSize: size}
}

func photoMsg(id int64) store.Message {
	return threadMsg(id, "photo", "u"+strconv.FormatInt(id, 10)+".jpg", int64(len(pngOf("x"))))
}

// imageData returns the payload of each block, in the order the helper produced them.
func imageData(t *testing.T, blocks []mcp.Content) [][]byte {
	t.Helper()
	res := make([][]byte, 0, len(blocks))
	for _, b := range blocks {
		img, ok := b.(*mcp.ImageContent)
		require.True(t, ok, "unexpected content type %T", b)
		res = append(res, img.Data)
	}
	return res
}

func TestThreadImages(t *testing.T) {
	ctx := context.Background()

	t.Run("the cap holds and order is chronological", func(t *testing.T) {
		payloads := map[string][]byte{}
		var msgs []store.Message
		for i := int64(1); i <= 7; i++ {
			payloads["f"+strconv.FormatInt(i, 10)] = pngOf(strconv.FormatInt(i, 10))
			msgs = append(msgs, photoMsg(i))
		}
		tg := fakeAPI(payloads)
		s := fileServer(t, tg)

		blocks, inlined := s.threadImages(ctx, msgs)
		require.Len(t, blocks, threadImageCap)
		assert.Equal(t, [][]byte{pngOf("1"), pngOf("2"), pngOf("3"), pngOf("4"), pngOf("5")},
			imageData(t, blocks), "the oldest images come first, whatever order they downloaded in")
		assert.Equal(t, map[int64]bool{1: true, 2: true, 3: true, 4: true, 5: true}, inlined)
		assert.Len(t, tg.GetFileCalls(), threadImageCap, "the images past the cap are never fetched")
	})

	t.Run("a failed download degrades to metadata", func(t *testing.T) {
		payloads := map[string][]byte{"f1": pngOf("1"), "f2": pngOf("2"), "f4": pngOf("4"), "f5": pngOf("5")}
		s := fileServer(t, fakeAPI(payloads))

		msgs := []store.Message{photoMsg(1), photoMsg(2), photoMsg(3), photoMsg(4), photoMsg(5)}
		blocks, inlined := s.threadImages(ctx, msgs)
		require.Len(t, blocks, 4)
		assert.Equal(t, [][]byte{pngOf("1"), pngOf("2"), pngOf("4"), pngOf("5")}, imageData(t, blocks))
		assert.Equal(t, map[int64]bool{1: true, 2: true, 4: true, 5: true}, inlined,
			"the unfetchable image keeps its metadata and nothing else fails")
	})

	t.Run("no telegram client", func(t *testing.T) {
		s := fileServer(t, nil)
		blocks, inlined := s.threadImages(ctx, []store.Message{photoMsg(1)})
		assert.Empty(t, blocks)
		assert.Empty(t, inlined)
	})

	t.Run("nothing qualifies", func(t *testing.T) {
		tg := fakeAPI(map[string][]byte{})
		s := fileServer(t, tg)

		msgs := []store.Message{
			{ChatID: -1001, MessageID: 1, SenderName: "alice", Text: "no attachment"},
			threadMsg(2, "document", "server.log", 12),
			threadMsg(3, "video", "clip.mp4", 12),
			threadMsg(4, "document", "report.pdf", 12),
			threadMsg(5, "document", "shot.heic", 12),
		}
		blocks, inlined := s.threadImages(ctx, msgs)
		assert.Empty(t, blocks)
		assert.Empty(t, inlined)
		assert.Empty(t, tg.GetFileCalls(), "non-images are recognized by name, before any download")
	})

	t.Run("stickers and other media do not consume slots", func(t *testing.T) {
		payloads := map[string][]byte{}
		msgs := []store.Message{
			threadMsg(1, "sticker", "u1.webp", 12),
			threadMsg(2, "sticker", "u2.webp", 12),
			threadMsg(3, "document", "server.log", 12),
			threadMsg(4, "voice", "u4.ogg", 12),
		}
		for i := int64(5); i <= 9; i++ {
			payloads["f"+strconv.FormatInt(i, 10)] = pngOf(strconv.FormatInt(i, 10))
			msgs = append(msgs, photoMsg(i))
		}
		s := fileServer(t, fakeAPI(payloads))

		blocks, inlined := s.threadImages(ctx, msgs)
		require.Len(t, blocks, threadImageCap, "five reactions must not eat the whole cap")
		assert.Equal(t, [][]byte{pngOf("5"), pngOf("6"), pngOf("7"), pngOf("8"), pngOf("9")},
			imageData(t, blocks))
		assert.Equal(t, map[int64]bool{5: true, 6: true, 7: true, 8: true, 9: true}, inlined)
	})

	t.Run("an oversized image is left to get_file", func(t *testing.T) {
		payloads := map[string][]byte{"f1": pngOf("1"), "f2": make([]byte, inlineLimit+1), "f3": pngOf("3")}
		s := fileServer(t, fakeAPI(payloads))

		blocks, inlined := s.threadImages(ctx, []store.Message{photoMsg(1), photoMsg(2), photoMsg(3)})
		require.Len(t, blocks, 2)
		assert.Equal(t, [][]byte{pngOf("1"), pngOf("3")}, imageData(t, blocks))
		assert.Equal(t, map[int64]bool{1: true, 3: true}, inlined)
	})

	t.Run("a missing file_size is taken off the disk", func(t *testing.T) {
		payloads := map[string][]byte{"f1": make([]byte, inlineLimit+1), "f2": pngOf("2")}
		s := fileServer(t, fakeAPI(payloads))

		// the Bot API omits file_size on some renditions; trusting the row would inline both
		big, small := threadMsg(1, "photo", "u1.jpg", 0), threadMsg(2, "photo", "u2.jpg", 0)
		blocks, inlined := s.threadImages(ctx, []store.Message{big, small})
		require.Len(t, blocks, 1)
		assert.Equal(t, [][]byte{pngOf("2")}, imageData(t, blocks))
		assert.Equal(t, map[int64]bool{2: true}, inlined)
	})

	t.Run("a known-oversized rendition is never downloaded", func(t *testing.T) {
		payloads := map[string][]byte{"f2": pngOf("2")}
		tg := fakeAPI(payloads)
		s := fileServer(t, tg)

		big := threadMsg(1, "photo", "u1.jpg", inlineLimit+1)
		blocks, inlined := s.threadImages(ctx, []store.Message{big, photoMsg(2)})
		require.Len(t, blocks, 1)
		assert.Equal(t, [][]byte{pngOf("2")}, imageData(t, blocks))
		assert.Equal(t, map[int64]bool{2: true}, inlined)
		require.Len(t, tg.GetFileCalls(), 1, "the row already says it is too big to inline")
		assert.Equal(t, "f2", tg.GetFileCalls()[0].FileID)
	})

	t.Run("bytes that belie the name degrade to metadata", func(t *testing.T) {
		payloads := map[string][]byte{"f1": []byte("PK\x03\x04not a picture"), "f2": pngOf("2")}
		s := fileServer(t, fakeAPI(payloads))

		blocks, inlined := s.threadImages(ctx, []store.Message{photoMsg(1), photoMsg(2)})
		require.Len(t, blocks, 1, "an image block the vision api rejects would fail the whole read")
		assert.Equal(t, [][]byte{pngOf("2")}, imageData(t, blocks))
		assert.Equal(t, map[int64]bool{2: true}, inlined)
	})

	t.Run("an unreadable cached file degrades to metadata", func(t *testing.T) {
		st := &mocks.MessageStore{
			CachedFunc: func(string) (string, bool) { return t.TempDir(), true }, // a directory never reads
		}
		s, err := New(Params{Store: st, Telegram: fakeAPI(nil), Chats: testConfig(t, chatMap),
			AuthToken: testToken, Listen: "127.0.0.1:0"})
		require.NoError(t, err)

		blocks, inlined := s.threadImages(ctx, []store.Message{photoMsg(1)})
		assert.Empty(t, blocks)
		assert.Empty(t, inlined)
	})

	t.Run("a panicking fetch degrades to metadata", func(t *testing.T) {
		s := fileServer(t, &mocks.TelegramAPI{
			GetFileFunc: func(context.Context, string) (telegram.File, error) { panic("bot api exploded") },
		})

		blocks, inlined := s.threadImages(ctx, []store.Message{photoMsg(1)})
		assert.Empty(t, blocks, "a panic on the fetch goroutine must not take the process down")
		assert.Empty(t, inlined)
	})

	t.Run("a canceled request stops the fetch", func(t *testing.T) {
		blocked, cancel := context.WithCancel(ctx)
		cancel()
		s := fileServer(t, &mocks.TelegramAPI{
			GetFileFunc: func(c context.Context, _ string) (telegram.File, error) {
				<-c.Done()
				return telegram.File{}, c.Err()
			},
		})

		done := make(chan struct{})
		go func() {
			defer close(done)
			blocks, inlined := s.threadImages(blocked, []store.Message{photoMsg(1), photoMsg(2)})
			assert.Empty(t, blocks)
			assert.Empty(t, inlined)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the fetch round outlived the request it was derived from")
		}
	})
}
