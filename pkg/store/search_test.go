package store

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_Search(t *testing.T) {
	ctx := t.Context()

	// chat -100 carries the interesting texts, chat -200 exists to prove the chat filter works.
	s := testStore(t)
	require.NoError(t, s.UpsertBatch(ctx, []Message{
		text(msgAt(-100, 1, 0), "agent refused the connection"),
		text(msgAt(-100, 2, time.Minute), "error: cannot open database"),
		text(msgAt(-100, 3, 2*time.Minute), "ran nxagentd -v on the host"),
		text(msgAt(-100, 4, 3*time.Minute), "disk is 50% full"),
		text(msgAt(-100, 5, 4*time.Minute), "connection refused again"),
		text(msgAt(-200, 6, time.Minute), "error: cannot open database"),
	}))

	tests := []struct {
		name    string
		query   string
		chats   []int64
		want    []int64
		snippet string
	}{
		{name: "single term", query: "refused", chats: []int64{-100}, want: []int64{5, 1}},
		{name: "two terms are ANDed", query: "connection refused", chats: []int64{-100}, want: []int64{5, 1}},
		{name: "term missing everywhere", query: "refused nonexistent", chats: []int64{-100}, want: nil},
		{
			name: "punctuation in the query", query: "error: cannot", chats: []int64{-100},
			want: []int64{2}, snippet: "error: cannot open database",
		},
		{name: "flag-looking term", query: "nxagentd -v", chats: []int64{-100}, want: []int64{3}},
		{
			name: "percent sign is not a wildcard", query: "50%", chats: []int64{-100},
			want: []int64{4}, snippet: "disk is 50% full",
		},
		{name: "quoted phrase matches", query: `"cannot open"`, chats: []int64{-100}, want: []int64{2}},
		{name: "quoted phrase in the wrong order", query: `"open cannot"`, chats: []int64{-100}, want: nil},
		{name: "punctuation only falls back to like", query: "-", chats: []int64{-100}, want: []int64{3}},
		{
			name: "quoted phrase falls back to like without its quotes", query: `"xagentd -v"`,
			chats: []int64{-100}, want: []int64{3},
		},
		{name: "quotes around nothing match nothing", query: `" "`, chats: []int64{-100}, want: nil},
		{name: "substring falls back to like", query: "atabas", chats: []int64{-100}, want: []int64{2}},
		{name: "case insensitive", query: "REFUSED", chats: []int64{-100}, want: []int64{5, 1}},
		{name: "multiple chats", query: "database", chats: []int64{-100, -200}, want: []int64{6, 2}},
		{name: "other chat only", query: "database", chats: []int64{-200}, want: []int64{6}},
		{name: "no match at all", query: "kubernetes", chats: []int64{-100}, want: nil},
		{name: "empty query", query: "   ", chats: []int64{-100}, want: nil},
		{name: "no chats", query: "refused", chats: nil, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Search(ctx, tt.query, tt.chats, time.Time{}, time.Time{}, 0)
			require.NoError(t, err)
			assert.Equal(t, tt.want, hitIDs(got), "newest first")
			if tt.snippet != "" {
				require.NotEmpty(t, got)
				assert.Equal(t, tt.snippet, got[0].Snippet)
			}
		})
	}

	t.Run("punctuation is not thrown away by the tokenizer", func(t *testing.T) {
		punct := testStore(t)
		require.NoError(t, punct.UpsertBatch(ctx, []Message{
			text(msgAt(-100, 1, 0), "disk is 50% full"),
			text(msgAt(-100, 2, time.Minute), "50 disks arrived"),
			text(msgAt(-100, 3, 2*time.Minute), "ran nxagentd -v"),
			text(msgAt(-100, 4, 3*time.Minute), "v is the version flag"),
		}))

		got, err := punct.Search(ctx, "50%", []int64{-100}, time.Time{}, time.Time{}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{1}, hitIDs(got), `"50 disks" tokenizes to the same "50" but is not what was asked`)

		got, err = punct.Search(ctx, "-v", []int64{-100}, time.Time{}, time.Time{}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{3}, hitIDs(got))

		// nothing carries the punctuation literally, so the loose match gets its turn rather than
		// leaving the caller with nothing
		got, err = punct.Search(ctx, "arrived!", []int64{-100}, time.Time{}, time.Time{}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{2}, hitIDs(got))
	})

	t.Run("hits carry the full message", func(t *testing.T) {
		got, err := s.Search(ctx, "nxagentd", []int64{-100}, time.Time{}, time.Time{}, 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, int64(-100), got[0].ChatID)
		assert.Equal(t, "ran nxagentd -v on the host", got[0].Text)
		assert.Equal(t, "Alice", got[0].SenderName)
		assert.False(t, got[0].Sent.IsZero())
	})

	t.Run("closed store", func(t *testing.T) {
		closed := testStore(t)
		require.NoError(t, closed.Close())
		_, err := closed.Search(ctx, "refused", []int64{-100}, time.Time{}, time.Time{}, 0)
		require.Error(t, err)
	})
}

func TestStore_Search_limitAndRange(t *testing.T) {
	ctx := t.Context()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	s := testStore(t)
	require.NoError(t, s.UpsertBatch(ctx, []Message{
		text(msgAt(-100, 1, 0), "timeout on poller"),
		text(msgAt(-100, 2, time.Hour), "timeout on agent"),
		text(msgAt(-100, 3, 2*time.Hour), "timeout again"),
	}))

	t.Run("limit keeps the newest", func(t *testing.T) {
		got, err := s.Search(ctx, "timeout", []int64{-100}, time.Time{}, time.Time{}, 2)
		require.NoError(t, err)
		assert.Equal(t, []int64{3, 2}, hitIDs(got))
	})

	t.Run("from bound is inclusive", func(t *testing.T) {
		got, err := s.Search(ctx, "timeout", []int64{-100}, base.Add(time.Hour), time.Time{}, 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{3, 2}, hitIDs(got))
	})

	t.Run("to bound is inclusive", func(t *testing.T) {
		got, err := s.Search(ctx, "timeout", []int64{-100}, time.Time{}, base.Add(time.Hour), 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{2, 1}, hitIDs(got))
	})

	t.Run("both bounds", func(t *testing.T) {
		got, err := s.Search(ctx, "timeout", []int64{-100},
			base.Add(30*time.Minute), base.Add(90*time.Minute), 0)
		require.NoError(t, err)
		assert.Equal(t, []int64{2}, hitIDs(got))
	})

	t.Run("range with a like fallback query", func(t *testing.T) {
		got, err := s.Search(ctx, "imeout", []int64{-100}, base.Add(time.Hour), time.Time{}, 1)
		require.NoError(t, err)
		assert.Equal(t, []int64{3}, hitIDs(got))
	})

	t.Run("empty range", func(t *testing.T) {
		got, err := s.Search(ctx, "timeout", []int64{-100}, base.Add(-48*time.Hour), base.Add(-24*time.Hour), 0)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

// TestStore_Search_editedMessage is the corruption case from the other side: an edit must not stay
// findable by its old text.
func TestStore_Search_editedMessage(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)

	m := text(msgAt(-100, 1, 0), "agent refused the connection")
	require.NoError(t, s.UpsertMessage(ctx, m))

	m.Text = "agent is locked"
	m.EditedAt = m.Sent.Add(time.Minute)
	require.NoError(t, s.UpsertMessage(ctx, m))

	got, err := s.Search(ctx, "refused", []int64{-100}, time.Time{}, time.Time{}, 0)
	require.NoError(t, err)
	assert.Empty(t, got, "the pre-edit text is still findable")

	got, err = s.Search(ctx, "locked", []int64{-100}, time.Time{}, time.Time{}, 0)
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, hitIDs(got))
}

func TestStore_Search_longTextSnippet(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)

	filler := strings.Repeat("padding ", 40)
	require.NoError(t, s.UpsertMessage(ctx, text(msgAt(-100, 1, 0), filler+"needle "+filler)))

	t.Run("fts snippet is trimmed around the match", func(t *testing.T) {
		got, err := s.Search(ctx, "needle", []int64{-100}, time.Time{}, time.Time{}, 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Contains(t, got[0].Snippet, "needle")
		assert.Less(t, len(got[0].Snippet), len(got[0].Text))
		assert.True(t, strings.HasPrefix(got[0].Snippet, ellipsis), got[0].Snippet)
		assert.True(t, strings.HasSuffix(got[0].Snippet, ellipsis), got[0].Snippet)
	})

	t.Run("like snippet is trimmed around the match", func(t *testing.T) {
		got, err := s.Search(ctx, "eedl", []int64{-100}, time.Time{}, time.Time{}, 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Contains(t, got[0].Snippet, "eedl")
		assert.Len(t, []rune(got[0].Snippet), 2*likeSnippetPad+len("eedl")+2*len([]rune(ellipsis)))
	})
}

func TestFtsQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "single term", query: "refused", want: `"refused"`},
		{name: "two terms", query: "connection refused", want: `"connection" "refused"`},
		{name: "colon is quoted away", query: "error: foo", want: `"error:" "foo"`},
		{name: "leading dash kept inside the quotes", query: "nxagentd -v", want: `"nxagentd" "-v"`},
		{name: "percent", query: "50%", want: `"50%"`},
		{name: "quoted phrase stays one term", query: `"cannot open" db`, want: `"cannot open" "db"`},
		{name: "unbalanced quote", query: `"cannot open`, want: `"cannot open"`},
		{name: "inner quote splits terms", query: `say"what`, want: `"say" "what"`},
		{name: "punctuation only", query: "-- ?!", want: ""},
		{name: "empty", query: "", want: ""},
		{name: "mixed punctuation and words", query: "* foo )", want: `"foo"`},
		{name: "extra whitespace", query: "  foo \t bar\n", want: `"foo" "bar"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ftsQuery(tt.query))
		})
	}
}

func TestEscapeLike(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{name: "plain", in: "foo", want: "foo"},
		{name: "percent", in: "50%", want: `50\%`},
		{name: "underscore", in: "a_b", want: `a\_b`},
		{name: "backslash", in: `a\b`, want: `a\\b`},
		{name: "all of them", in: `%_\`, want: `\%\_\\`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, escapeLike(tt.in))
		})
	}
}

// TestStore_Search_wildcardsAreLiteral proves the LIKE fallback does not treat wildcards as syntax.
func TestStore_Search_wildcardsAreLiteral(t *testing.T) {
	ctx := t.Context()
	s := testStore(t)
	require.NoError(t, s.UpsertBatch(ctx, []Message{
		text(msgAt(-100, 1, 0), "value is 50% of the limit"),
		text(msgAt(-100, 2, time.Minute), "value is 5 of the limit"),
	}))

	got, err := s.Search(ctx, "0% of", []int64{-100}, time.Time{}, time.Time{}, 0)
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, hitIDs(got), "the percent sign matched anything")
}

func TestLikeSnippet(t *testing.T) {
	tests := []struct{ name, text, query, want string }{
		{name: "short text is returned whole", text: "hello world", query: "world", want: "hello world"},
		{name: "no match keeps the head", text: "hello world", query: "zzz", want: "hello world"},
		{name: "empty text", text: "", query: "x", want: ""},
		{
			name: "match at the end", text: strings.Repeat("x", likeSnippetPad+10) + "needle",
			query: "needle", want: ellipsis + strings.Repeat("x", likeSnippetPad) + "needle",
		},
		{
			name: "match at the start", text: "needle" + strings.Repeat("x", likeSnippetPad+10),
			query: "needle", want: "needle" + strings.Repeat("x", likeSnippetPad) + ellipsis,
		},
		{name: "utf8 is not cut mid rune", text: "тест иголка тест", query: "иголка", want: "тест иголка тест"},
		{
			name:  "cyrillic padding is windowed in runes",
			text:  strings.Repeat("я", likeSnippetPad+10) + "иголка" + strings.Repeat("э", likeSnippetPad+10),
			query: "иголка",
			want: ellipsis + strings.Repeat("я", likeSnippetPad) + "иголка" +
				strings.Repeat("э", likeSnippetPad) + ellipsis,
		},
		{
			// ToLower shortens İ from two bytes to one, so a byte offset would slip left
			name:  "match found past runes that shrink when lowercased",
			text:  strings.Repeat("İ", likeSnippetPad+10) + "needle" + strings.Repeat("z", likeSnippetPad+10),
			query: "needle",
			want: ellipsis + strings.Repeat("İ", likeSnippetPad) + "needle" +
				strings.Repeat("z", likeSnippetPad) + ellipsis,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, likeSnippet(tt.text, tt.query))
		})
	}
}

func text(m Message, s string) Message {
	m.Text = s
	return m
}

func hitIDs(hits []SearchHit) []int64 {
	if len(hits) == 0 {
		return nil
	}
	res := make([]int64, len(hits))
	for i, h := range hits {
		res[i] = h.MessageID
	}
	return res
}
