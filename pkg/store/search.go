package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// SearchHit is a search result: the matching message plus a short excerpt around the match.
type SearchHit struct {
	Message
	Snippet string
}

const (
	// ftsSnippet is a fixed expression — fts5 auxiliary functions take their arguments as literals.
	ftsSnippet = `snippet(messages_fts, 0, '', '', '…', 15)`
	// likeSnippetPad is how many characters of context the LIKE fallback keeps on both sides.
	likeSnippetPad = 60
	ellipsis       = "…"
)

// Search finds messages matching query in the given chats, newest first. Both time bounds are
// inclusive, a zero bound drops it, and a limit of 0 or less means no limit.
//
// An FTS5 MATCH runs first, narrowed to the messages that literally carry whatever punctuation the
// query spells out — tokenization throws it away, so "50%" alone would match every "50". The query
// then falls back to a LIKE scan when the input has nothing full text can match on, when the match
// errors out, or when it finds nothing: tokenization never matches inside a word, which people do
// search for. Only when the literal reading finds nothing anywhere does the loose MATCH get a
// turn, so a query the message spells differently ("don't" against "don’t") still lands.
func (s *Store) Search(ctx context.Context, query string, chatIDs []int64,
	from, to time.Time, limit int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(chatIDs) == 0 {
		return nil, nil
	}

	match, literals := ftsQuery(query), literalTerms(query)
	if match != "" {
		hits, err := s.searchFTS(ctx, match, literals, chatIDs, from, to, limit)
		if err == nil && len(hits) > 0 {
			return hits, nil
		}
	}

	// LIKE matches the query as a literal substring, so the phrase quotes have to go: they are
	// syntax on the fts path, not something the message text carries.
	needle := strings.TrimSpace(strings.ReplaceAll(query, `"`, ""))
	if needle == "" {
		return nil, nil
	}
	hits, err := s.searchLike(ctx, needle, chatIDs, from, to, limit)
	if err != nil || len(hits) > 0 || len(literals) == 0 {
		return hits, err
	}
	// nothing matches the punctuation literally; a term carrying it always leaves fts something to
	// match on, so match is non-empty here
	return s.searchFTS(ctx, match, nil, chatIDs, from, to, limit)
}

func (s *Store) searchFTS(ctx context.Context, match string, literals []string, chatIDs []int64,
	from, to time.Time, limit int) ([]SearchHit, error) {
	in, chatArgs := inClause(chatIDs)
	q := `SELECT ` + messageColumns + `, ` + ftsSnippet + `
		FROM messages_fts f JOIN messages m ON m.id = f.rowid
		WHERE messages_fts MATCH ? AND m.chat_id IN ` + in
	args := append([]any{match}, chatArgs...)

	q += strings.Repeat(` AND m.text LIKE ? ESCAPE '\'`, len(literals))
	for _, t := range literals {
		args = append(args, "%"+escapeLike(t)+"%")
	}

	cond, rangeArgs := sentRange(from, to)
	q += cond + ` ORDER BY m.sent DESC, m.id DESC LIMIT ?`
	args = append(append(args, rangeArgs...), sqlLimit(limit))

	return s.querySnippets(ctx, q, args...)
}

func (s *Store) searchLike(ctx context.Context, query string, chatIDs []int64,
	from, to time.Time, limit int) ([]SearchHit, error) {
	in, chatArgs := inClause(chatIDs)
	q := `SELECT ` + messageColumns + ` FROM messages m
		WHERE m.text LIKE ? ESCAPE '\' AND m.chat_id IN ` + in
	args := append([]any{"%" + escapeLike(query) + "%"}, chatArgs...)

	cond, rangeArgs := sentRange(from, to)
	q += cond + ` ORDER BY m.sent DESC, m.id DESC LIMIT ?`
	args = append(append(args, rangeArgs...), sqlLimit(limit))

	msgs, err := s.queryMessages(ctx, q, args...)
	if err != nil {
		return nil, err
	}

	hits := make([]SearchHit, 0, len(msgs))
	for _, m := range msgs {
		hits = append(hits, SearchHit{Message: m, Snippet: likeSnippet(m.Text, query)})
	}
	return hits, nil
}

func (s *Store) querySnippets(ctx context.Context, query string, args ...any) ([]SearchHit, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var res []SearchHit
	for rows.Next() {
		var snip string
		m, err := scanMessage(snippetScanner{rows: rows, snippet: &snip})
		if err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		res = append(res, SearchHit{Message: m, Snippet: snip})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search hits: %w", err)
	}
	return res, nil
}

// snippetScanner lets scanMessage read a row that carries a trailing snippet column.
type snippetScanner struct {
	rows    scanner
	snippet *string
}

func (s snippetScanner) Scan(dest ...any) error {
	return s.rows.Scan(append(dest, s.snippet)...) //nolint:wrapcheck // caller adds context
}

// sentRange renders the inclusive sent-range conditions of a query on the messages alias m.
func sentRange(from, to time.Time) (string, []any) {
	var (
		q    string
		args []any
	)
	if !from.IsZero() {
		q += ` AND m.sent >= ?`
		args = append(args, formatTime(from))
	}
	if !to.IsZero() {
		q += ` AND m.sent <= ?`
		args = append(args, formatTime(to))
	}
	return q, args
}

// ftsQuery turns free-form input into an fts5 MATCH expression: every term becomes a quoted string
// so punctuation cannot be read as query syntax, double-quoted phrases stay phrases, and terms
// without a single letter or digit are dropped. An empty result means fts has nothing to match on.
func ftsQuery(raw string) string {
	var terms []string
	for _, t := range splitTerms(raw) {
		if !hasWordChar(t) {
			continue
		}
		terms = append(terms, `"`+t+`"`)
	}
	return strings.Join(terms, " ")
}

// splitTerms splits on whitespace, keeping double-quoted runs together as one term. Quotes
// themselves never reach a term, so terms are safe to wrap in quotes for fts.
func splitTerms(raw string) []string {
	var (
		terms   []string
		cur     strings.Builder
		inQuote bool
	)
	flush := func() {
		if cur.Len() > 0 {
			terms = append(terms, cur.String())
			cur.Reset()
		}
	}
	for _, r := range raw {
		switch {
		case r == '"':
			flush()
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return terms
}

func hasWordChar(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
}

// literalTerms picks the terms whose punctuation the tokenizer discards: "50%" indexes as "50" and
// "-v" as "v", so a MATCH on them alone answers a different question than the one asked. These
// terms are re-checked as literal substrings alongside the MATCH. Spaces do not count — they only
// occur inside a quoted phrase, where adjacency is exactly what fts is being asked for.
func literalTerms(raw string) []string {
	var res []string
	for _, t := range splitTerms(raw) {
		if hasWordChar(t) && strings.ContainsFunc(t, isDroppedPunct) {
			res = append(res, t)
		}
	}
	return res
}

func isDroppedPunct(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsSpace(r)
}

// escapeLike neutralizes the LIKE wildcards so the pattern matches the query literally.
func escapeLike(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '%' || r == '_' || r == '\\' {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// likeSnippet cuts an excerpt around the first case-insensitive occurrence of the query, standing
// in for what fts5 snippet() produces on the match path.
func likeSnippet(text, query string) string {
	runes := []rune(text)
	start := 0
	// lowercasing maps rune to rune, so a rune offset in lower is one in text too — but a byte
	// offset is not, ToLower is free to change the encoded length (İ → i)
	lower := strings.ToLower(text)
	if i := strings.Index(lower, strings.ToLower(query)); i > 0 {
		start = utf8.RuneCountInString(lower[:i])
	}

	end := start + len([]rune(query)) + likeSnippetPad
	start -= likeSnippetPad

	prefix, suffix := ellipsis, ellipsis
	if start <= 0 {
		start, prefix = 0, ""
	}
	if end >= len(runes) {
		end, suffix = len(runes), ""
	}
	return prefix + string(runes[start:end]) + suffix
}
