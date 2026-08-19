// Package tghtml renders the markdown subset models write into Telegram HTML.
//
// Only tags Telegram's HTML parse mode understands are emitted, and every rule fails toward literal
// text: a marker that cannot be matched with confidence stays as it was typed. A valid-but-wrong
// entity reaches the customer as corruption no fallback can catch, a stray asterisk is just noise.
package tghtml

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// maxRun is the longest asterisk run that can still be a delimiter; four or more are literal.
	maxRun   = 3
	minFence = 3
)

var (
	fenceLang = regexp.MustCompile(`^[A-Za-z0-9+#._-]+$`)
	// heading needs the space after the hashes, so #!/bin/sh is not a heading. The level is not
	// captured: telegram has no heading entity, so every level renders as bold.
	heading = regexp.MustCompile(`^#{1,6} (.+)$`)

	textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	// boldStripper drops the bold tags the inline pass emitted; only generated tags can match,
	// typed ones are escaped by then.
	boldStripper = strings.NewReplacer("<b>", "", "</b>", "")

	// linkSchemes are the protocols telegram accepts in an href. It answers 400 to anything else,
	// which would degrade the whole reply to plain text, so an unrecognized target stays literal.
	linkSchemes = []string{"https://", "http://", "tg://", "mailto:", "tel:"}
)

// Render converts text to Telegram HTML: fenced and inline code, bold, italic, links and setext-less
// headings; everything else is escaped and passed through. The result is always balanced and fully
// escaped, so it is safe to send with parse_mode=HTML.
func Render(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if lang, size, ok := fenceStart(lines[i]); ok {
			var body []string
			for i++; i < len(lines) && !isFenceEnd(lines[i], size); i++ {
				body = append(body, lines[i])
			}
			out = append(out, renderFence(lang, body))
			continue
		}
		if m := heading.FindStringSubmatch(lines[i]); m != nil {
			// the whole line is bold, so a **run** inside it can only nest a second <b>
			out = append(out, "<b>"+boldStripper.Replace(renderInline(strings.TrimRight(m[1], " \t")))+"</b>")
			continue
		}
		out = append(out, renderInline(lines[i]))
	}
	return strings.Join(out, "\n")
}

// fenceStart reports whether the line opens a code fence, along with its language tag if it carries
// a usable one and the length of the run that opened it. A backtick in the info string means the
// line is an inline span, not an opener: ```jcmd``` written on its own line must not swallow
// everything after it into the block.
func fenceStart(line string) (lang string, size int, ok bool) {
	t := strings.TrimSpace(line)
	rest := strings.TrimLeft(t, "`")
	size = len(t) - len(rest)
	if size < minFence {
		return "", 0, false
	}
	info := strings.TrimSpace(rest)
	if strings.ContainsRune(info, '`') {
		return "", 0, false
	}
	if !fenceLang.MatchString(info) {
		return "", size, true
	}
	return info, size, true
}

// isFenceEnd reports whether the line is a bare fence at least as long as the run that opened the
// block, commonmark's rule: a ```` block is how a reply quotes ``` fences, and closing it on the
// first inner one would spill the rest of the body out of the block as prose.
func isFenceEnd(line string, size int) bool {
	t := strings.TrimSpace(line)
	return len(t) >= size && strings.Trim(t, "`") == ""
}

func renderFence(lang string, body []string) string {
	content := textEscaper.Replace(strings.Join(body, "\n"))
	if lang == "" {
		return "<pre>" + content + "</pre>"
	}
	return `<pre><code class="language-` + lang + `">` + content + "</code></pre>"
}

type tokenKind int

const (
	tokenText tokenKind = iota
	tokenCode
	tokenLink
	tokenStars
)

type token struct {
	kind  tokenKind
	text  string // literal text, code span content or link text
	url   string
	run   int  // length of an asterisk run
	prev  rune // rune before the run, 0 at the start of the segment
	prev2 rune // rune before prev, 0 when prev opens the segment
	next  rune // rune after the run, 0 at the end of the segment
}

// tokenize splits a line into literals, code spans, links and asterisk runs. Code spans and links
// are taken left to right before asterisks, so a marker inside them is never a delimiter.
func tokenize(s string) []token {
	var toks []token
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			toks = append(toks, token{kind: tokenText, text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch s[i] {
		case '`':
			if end := strings.IndexByte(s[i+1:], '`'); end > 0 {
				flush()
				toks = append(toks, token{kind: tokenCode, text: s[i+1 : i+1+end]})
				i += end + 2
				continue
			}
		case '[':
			if text, url, size, ok := parseLink(s[i:]); ok {
				flush()
				toks = append(toks, token{kind: tokenLink, text: text, url: url})
				i += size
				continue
			}
		case '*':
			n := 1
			for i+n < len(s) && s[i+n] == '*' {
				n++
			}
			flush()
			prev, prev2 := prevRunes(s, i)
			toks = append(toks, token{kind: tokenStars, run: n, prev: prev, prev2: prev2, next: nextRune(s, i+n)})
			i += n
			continue
		}
		lit.WriteByte(s[i])
		i++
	}
	flush()
	return toks
}

// parseLink matches [text](url) at the start of s: the text runs to the first ], which must be
// followed by (, and the url to the ) that balances it. Anything else — a url that is never
// closed, one carrying whitespace, one telegram would not accept — is not a link.
func parseLink(s string) (text, url string, size int, ok bool) {
	end := strings.IndexByte(s, ']')
	if end < 2 || end+1 >= len(s) || s[end+1] != '(' {
		return "", "", 0, false
	}
	tail := urlEnd(s[end+2:])
	if tail < 1 {
		return "", "", 0, false
	}
	url = s[end+2 : end+2+tail]
	if strings.ContainsFunc(url, unicode.IsSpace) || !hasScheme(url) {
		return "", "", 0, false
	}
	return s[1:end], url, end + tail + 3, true
}

// urlEnd returns the offset of the ) closing a link url, or -1 when it is never closed. Pairs
// inside the url are balanced, so a wikipedia link keeps its parentheses instead of being cut into
// an href nobody can follow.
func urlEnd(s string) int {
	depth := 0
	for i := range len(s) {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

func hasScheme(url string) bool {
	for _, scheme := range linkSchemes {
		if len(url) > len(scheme) && strings.EqualFold(url[:len(scheme)], scheme) {
			return true
		}
	}
	return false
}

func prevRunes(s string, i int) (prev, prev2 rune) {
	if i == 0 {
		return 0, 0
	}
	prev, size := utf8.DecodeLastRuneInString(s[:i])
	if i-size == 0 {
		return prev, 0
	}
	prev2, _ = utf8.DecodeLastRuneInString(s[:i-size])
	return prev, prev2
}

func nextRune(s string, i int) rune {
	if i >= len(s) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r
}

// frame is an open emphasis context; its buffer holds the already rendered inner content, ready to
// be wrapped when the closing run arrives or spilled out literally when it never does.
type frame struct {
	run int
	buf strings.Builder
}

type renderer struct {
	root   strings.Builder
	stack  []*frame
	inLink bool // code spans stay literal inside link text, see render
}

func renderInline(s string) string {
	var r renderer
	r.render(tokenize(s))
	return r.root.String()
}

// renderLinkText renders the text of a link. Bold and italic may wrap a link or sit inside it, but
// telegram's entity model forbids a code entity inside a link entity ("all other entities can't
// contain each other") and resolves the clash by dropping one of the two — a link whose text is
// entirely one code span loses the url outright, with no 400 and nothing to fall back to. So the
// span keeps its backticks here and the href survives.
func renderLinkText(s string) string {
	r := renderer{inLink: true}
	r.render(tokenize(s))
	return r.root.String()
}

func (r *renderer) render(toks []token) {
	for _, tk := range toks {
		switch tk.kind {
		case tokenText:
			r.write(textEscaper.Replace(tk.text))
		case tokenCode:
			if r.inLink {
				r.write(textEscaper.Replace("`" + tk.text + "`"))
				continue
			}
			r.write("<code>" + textEscaper.Replace(tk.text) + "</code>")
		case tokenLink:
			r.write(`<a href="` + attrEscaper.Replace(tk.url) + `">` + renderLinkText(tk.text) + "</a>")
		case tokenStars:
			r.stars(tk)
		}
	}
	for len(r.stack) > 0 {
		f := r.pop()
		r.write(stars(f.run) + f.buf.String())
	}
}

func (r *renderer) stars(tk token) {
	n, opens, closes := tk.run, canOpen(tk), canClose(tk)
	switch {
	case closes && n == maxRun && r.fused():
		r.closeTop()
		r.closeTop()
	case closes && len(r.stack) > 0 && r.stack[len(r.stack)-1].run == n:
		r.closeTop()
	case opens && !r.isOpen(n):
		r.stack = append(r.stack, &frame{run: n})
	default:
		r.write(stars(n))
	}
}

// canClose reports whether the run may end an open context: it must sit against the content it
// wraps, and a run wedged between punctuation and a word closes nothing — the second ** of
// `f(**kwargs) and g(**args)` faces `(` on the left and a letter on the right, and pairing it with
// the first would hand the customer f(kwargs): valid html, no 400, no fallback.
func canClose(tk token) bool {
	if tk.run > maxRun || tk.prev == 0 || unicode.IsSpace(tk.prev) || lone(tk.run, tk.prev) {
		return false
	}
	return !isPunct(tk.prev) || tk.next == 0 || unicode.IsSpace(tk.next) || isPunct(tk.next)
}

// canOpen reports whether the run may start a context: content must follow it, and what precedes it
// must be nothing, whitespace, or punctuation that is not itself glued to a word.
func canOpen(tk token) bool {
	if tk.run > maxRun || tk.next == 0 || unicode.IsSpace(tk.next) || lone(tk.run, tk.next) {
		return false
	}
	return tk.prev == 0 || unicode.IsSpace(tk.prev) ||
		(isPunct(tk.prev) && !lone(tk.run, tk.prev) && !wedged(tk))
}

// fused reports whether the two innermost frames are a ** and a * in either order, the only case
// where one run of three closes two contexts.
func (r *renderer) fused() bool {
	if len(r.stack) < 2 {
		return false
	}
	// runs are 1, 2 or 3, so summing to three is the ** and * pair and nothing else
	return r.stack[len(r.stack)-1].run+r.stack[len(r.stack)-2].run == maxRun
}

// isOpen reports whether a run of n is already covered by an open context, in which case it cannot
// open another one. A composite run of three covers both bold and italic.
func (r *renderer) isOpen(n int) bool {
	if n == maxRun {
		return len(r.stack) > 0
	}
	for _, f := range r.stack {
		if f.run == n || f.run == maxRun {
			return true
		}
	}
	return false
}

func (r *renderer) closeTop() {
	f := r.pop()
	r.write(openTag(f.run) + f.buf.String() + closeTag(f.run))
}

func (r *renderer) pop() *frame {
	f := r.stack[len(r.stack)-1]
	r.stack = r.stack[:len(r.stack)-1]
	return f
}

func (r *renderer) write(s string) {
	if n := len(r.stack); n > 0 {
		r.stack[n-1].buf.WriteString(s)
		return
	}
	r.root.WriteString(s)
}

func openTag(run int) string {
	switch run {
	case 1:
		return "<i>"
	case 2:
		return "<b>"
	default:
		return "<b><i>"
	}
}

func closeTag(run int) string {
	switch run {
	case 1:
		return "</i>"
	case 2:
		return "</b>"
	default:
		return "</i></b>"
	}
}

func stars(n int) string { return strings.Repeat("*", n) }

func isPunct(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) }

// lone reports whether a single asterisk facing r must stay literal. A run of one is what shell
// globs, paths and varargs are made of, and commonmark's flanking rules happily pair `/tmp/*.log`
// with `/var/*.log`, or `f(*args)` with a later `*word*`, into emphasis: well formed html that
// silently drops the stars from a command the customer is meant to run, which no fallback can
// catch. Demanding a non-punctuation rune on both flanks of a single star leaves `*.*`, `*:*`,
// `/*.log` and `(*args` literal; the cost is that `(*italic*)` no longer emphasizes, which is the
// side of the trade this package always takes. Runs of two and three are rare in command text, so
// they keep the plain rule and `(**bold**)` still works.
func lone(n int, flank rune) bool { return n == 1 && isPunct(flank) }

// wedged reports whether the punctuation in front of a run is itself glued to a word, the shape of
// f(**kwargs) and dict(**a): punctuation may still front a parenthetical — "see (**bold**)" — but
// only when nothing is attached to its left. Opening inside a call pairs the vararg with the next
// run on the line and deletes both from a command the customer is meant to paste, which is why the
// run stays literal here even though commonmark, whose delimiter matching would rather pair the
// later run with its own partner, opens it.
func wedged(tk token) bool { return tk.prev2 != 0 && !unicode.IsSpace(tk.prev2) }
