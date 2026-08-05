package tghtml

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRender_Blocks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "just a sentence", "just a sentence"},
		{"empty input", "", ""},
		{"escapes markup characters", "a < b & c > d", "a &lt; b &amp; c &gt; d"},
		{"escapes typed tags", "<b>not bold</b>", "&lt;b&gt;not bold&lt;/b&gt;"},
		{"crlf normalised", "first\r\nsecond", "first\nsecond"},
		{"fence without language", "```\nls -la\n```", "<pre>ls -la</pre>"},
		{
			"fence with language",
			"```sh\nls -la\n```",
			`<pre><code class="language-sh">ls -la</code></pre>`,
		},
		{
			"fence language with punctuation kept",
			"```c#\nvar x;\n```",
			`<pre><code class="language-c#">var x;</code></pre>`,
		},
		{"invalid language token dropped", "```my lang!\nx\n```", "<pre>x</pre>"},
		{
			"one line fence is a code span, not an opener",
			"```jcmd```\nrun it now",
			"``<code>jcmd</code>``\nrun it now",
		},
		{
			"fence text after the backticks is never swallowed",
			"```systemctl restart nxagentd``` fixes it\nand nothing follows",
			"``<code>systemctl restart nxagentd</code>`` fixes it\nand nothing follows",
		},
		{"fence content escaped, markdown inert", "```\na < *b* & `c`\n```", "<pre>a &lt; *b* &amp; `c`</pre>"},
		{"multiline fence", "```\none\ntwo\n```", "<pre>one\ntwo</pre>"},
		{"unclosed fence closes at eof", "```\nline one\nline two", "<pre>line one\nline two</pre>"},
		{"empty fence", "```\n```", "<pre></pre>"},
		{
			"longer fence survives inner fences",
			"````\n```sh\nls -la\n```\n````\nafter",
			"<pre>```sh\nls -la\n```</pre>\nafter",
		},
		{
			"longer fence with language survives inner fences",
			"````md\n```\nx\n```\n````",
			`<pre><code class="language-md">` + "```\nx\n```" + "</code></pre>",
		},
		{"unclosed longer fence closes at eof", "````\n```\nx", "<pre>```\nx</pre>"},
		{"longer run closes a shorter fence", "```\nx\n`````\nafter", "<pre>x</pre>\nafter"},
		{"text around fence", "before\n```\nx\n```\nafter", "before\n<pre>x</pre>\nafter"},
		{"heading becomes bold", "# Title", "<b>Title</b>"},
		{"deepest heading becomes bold", "###### Title", "<b>Title</b>"},
		{"heading renders inline markup", "## Run `jcmd`", "<b>Run <code>jcmd</code></b>"},
		{"bold heading does not nest bold", "# **Title**", "<b>Title</b>"},
		{"partly bold heading does not nest bold", "## **Steps** to fix", "<b>Steps to fix</b>"},
		{"composite heading keeps italic", "# ***Title***", "<b><i>Title</i></b>"},
		{
			"link in heading keeps its href",
			"# see [docs](https://x.io)",
			`<b>see <a href="https://x.io">docs</a></b>`,
		},
		{"shebang is not a heading", "#!/bin/sh", "#!/bin/sh"},
		{"hash without space is not a heading", "#tag here", "#tag here"},
		{"seven hashes is not a heading", "####### deep", "####### deep"},
		{"list markers stay literal", "- one\n- two", "- one\n- two"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Render(tt.in))
		})
	}
}

// TestRender_DecisionTable pins the delimiter-run semantics agreed in the plan; every row is a
// decision, not an accident.
func TestRender_DecisionTable(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"composite run of three", "***both***", "<b><i>both</i></b>"},
		{"fused close", "**bold *italic***", "<b>bold <i>italic</i></b>"},
		{"runs are atomic", "*a **b** c*", "<i>a <b>b</b> c</i>"},
		{"kwargs residual", "**a **b** c**", "<b>a **b</b> c**"},
		{"three then two is literal", "***x**", "***x**"},
		{"two then three is literal", "**x***", "**x***"},
		{"run of four is literal", "****x****", "****x****"},
		{"double star kwargs literal", "use **kwargs and **args", "use **kwargs and **args"},
		{"parenthesised kwargs literal", "use f(**kwargs) and g(**args)", "use f(**kwargs) and g(**args)"},
		{"parenthesised triple star literal", "pass f(***x) then g(***y)", "pass f(***x) then g(***y)"},
		{"kwargs does not close a later bold", "f(**kwargs) then **note** it", "f(**kwargs) then <b>note</b> it"},
		{"intraword run is literal", "x = a**b**c", "x = a**b**c"},
		{"backticks tokenized first", "**see `a**b` here**", "<b>see <code>a**b</code> here</b>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Render(tt.in))
		})
	}
}

func TestRender_Inline(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"inline code", "run `nxagentd -v` now", "run <code>nxagentd -v</code> now"},
		{"code content escaped", "`a < b`", "<code>a &lt; b</code>"},
		{"empty code span literal", "``", "``"},
		{"unterminated backtick literal", "a ` b", "a ` b"},
		{"bold", "**bold**", "<b>bold</b>"},
		{"italic", "*italic*", "<i>italic</i>"},
		{"code span inside bold", "**run `jcmd`**", "<b>run <code>jcmd</code></b>"},
		{"code span beats bold", "`**not bold**`", "<code>**not bold**</code>"},
		{"bold across whole line with text", "say **this** now", "say <b>this</b> now"},
		{"opener after punctuation", "(**bold**)", "(<b>bold</b>)"},
		{"unmatched opener literal", "**dangling", "**dangling"},
		{"unmatched closer literal", "dangling**", "dangling**"},
		{"inner emphasis survives unmatched outer", "*a **b** c", "*a <b>b</b> c"},
		{"underscores are never emphasis", "set file_unique_id and chat_id", "set file_unique_id and chat_id"},
		{"sql star untouched", "SELECT * FROM t", "SELECT * FROM t"},
		{"shell globs untouched", "chmod 755 * && chown *", "chmod 755 * &amp;&amp; chown *"},
		{"arithmetic untouched", "2 * 3 * 4", "2 * 3 * 4"},
		{"star before space is not an opener", "a * b", "a * b"},
		{"glob paths do not pair", "rm /tmp/*.log /var/*.log", "rm /tmp/*.log /var/*.log"},
		{"glob pair with colons", "chown *:* /var/log", "chown *:* /var/log"},
		{"sql star pair untouched", "SELECT *,* FROM t", "SELECT *,* FROM t"},
		{"vararg does not open", "foo(*args) and *bar*", "foo(*args) and <i>bar</i>"},
		{"glob does not eat a later italic", "see /var/log/*.log and *this*", "see /var/log/*.log and <i>this</i>"},
		{"single star after punctuation stays literal", "see (*note*) below", "see (*note*) below"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Render(tt.in))
		})
	}
}

func TestRender_Links(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple link", "[docs](https://x.io)", `<a href="https://x.io">docs</a>`},
		{
			"query string ampersand escaped",
			"[docs](https://x.io/?a=1&b=2)",
			`<a href="https://x.io/?a=1&amp;b=2">docs</a>`,
		},
		{
			"quote in url escaped",
			`[docs](https://x.io/a"b)`,
			`<a href="https://x.io/a&quot;b">docs</a>`,
		},
		{"markup in link text rendered", "[**docs**](https://x.io)", `<a href="https://x.io"><b>docs</b></a>`},
		{"code span in link text stays literal", "[`jcmd`](https://x.io)", "<a href=\"https://x.io\">`jcmd`</a>"},
		{
			"code span inside bold link text stays literal",
			"[**run `jcmd`**](https://x.io)",
			"<a href=\"https://x.io\"><b>run `jcmd`</b></a>",
		},
		{
			"literal code span in link text escaped",
			"[`a<b`](https://x.io)",
			"<a href=\"https://x.io\">`a&lt;b`</a>",
		},
		{"code span outside the link keeps its tags", "`jcmd` [docs](https://x.io)", "<code>jcmd</code> <a href=\"https://x.io\">docs</a>"},
		{"link inside bold", "**see [docs](https://x.io)**", `<b>see <a href="https://x.io">docs</a></b>`},
		{
			"parens inside the url are balanced",
			"[a](https://x.io/a(b)c)",
			`<a href="https://x.io/a(b)c">a</a>`,
		},
		{
			"wikipedia style url survives whole",
			"see [the wiki](https://en.wikipedia.org/wiki/Foo_(bar)) now",
			`see <a href="https://en.wikipedia.org/wiki/Foo_(bar)">the wiki</a> now`,
		},
		{"unbalanced paren in url is literal", "[a](https://x.io/a(b", "[a](https://x.io/a(b"},
		{"mailto link", "[us](mailto:support@x.io)", `<a href="mailto:support@x.io">us</a>`},
		{"relative target is literal", "[log](/var/log/nxagentd.log)", "[log](/var/log/nxagentd.log)"},
		{"schemeless target is literal", "[docs](x.io/docs)", "[docs](x.io/docs)"},
		{"script target is literal", "[click](javascript:alert(1))", "[click](javascript:alert(1))"},
		{"bracket deeper in text is literal", "[a]b](c)", "[a]b](c)"},
		{"unterminated link is literal", "[a](b", "[a](b"},
		{"missing paren is literal", "[a] (b)", "[a] (b)"},
		{"empty text is literal", "[](https://x.io)", "[](https://x.io)"},
		{"empty url is literal", "[a]()", "[a]()"},
		{"url with space is literal", "[a](https://x.io/a b)", "[a](https://x.io/a b)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Render(tt.in))
		})
	}
}

// TestRender_Adversarial guards against quadratic run matching and against ever emitting a
// mis-nested tag, whatever the input. The inputs run to the reply limit, where a scanner that
// backtracked would show up as a hang rather than as microseconds.
func TestRender_Adversarial(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"delimiter soup", strings.Repeat("*[`", 1400)},
		{"asterisks only", strings.Repeat("*", 4096)},
		{"alternating runs", strings.Repeat("**a*b", 820)},
		{"brackets only", strings.Repeat("[", 4096)},
		{"backticks only", strings.Repeat("`", 4096)},
		{"nested links", strings.Repeat("[", 1000) + "a" + strings.Repeat("](https://x.io)", 1000)},
		{"fence openers", strings.Repeat("```sh\n", 600)},
		{"headings", strings.Repeat("# **a*\n", 600)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertWellFormed(t, Render(tt.in))
		})
	}
}

var anyTag = regexp.MustCompile(`</?([a-z]+)[^>]*>`)

// assertWellFormed walks the emitted tags with a stack: counting opens against closes would accept
// interleaved <b><i></b></i>, which telegram would reject and no fallback could catch.
func assertWellFormed(t *testing.T, out string) {
	t.Helper()
	var stack []string
	for _, m := range anyTag.FindAllStringSubmatch(out, -1) {
		if !strings.HasPrefix(m[0], "</") {
			stack = append(stack, m[1])
			continue
		}
		require.NotEmpty(t, stack, "closing </%s> with nothing open in %.80q", m[1], out)
		require.Equal(t, stack[len(stack)-1], m[1], "mis-nested </%s> in %.80q", m[1], out)
		stack = stack[:len(stack)-1]
	}
	assert.Empty(t, stack, "unclosed tags in %.80q", out)
}

// TestRender_RealReply is the reply from the production screenshot that started this work.
func TestRender_RealReply(t *testing.T) {
	in := "**Thread dump of the stuck agent**\n" +
		"Find the pid with `jps -l`, then run:\n" +
		"```sh\njcmd <pid> Thread.print > /tmp/dump.txt\n```\n" +
		"Please attach the file, see [the docs](https://netxms.org/docs?a=1&b=2) for details."
	want := "<b>Thread dump of the stuck agent</b>\n" +
		"Find the pid with <code>jps -l</code>, then run:\n" +
		`<pre><code class="language-sh">jcmd &lt;pid&gt; Thread.print &gt; /tmp/dump.txt</code></pre>` + "\n" +
		`Please attach the file, see <a href="https://netxms.org/docs?a=1&amp;b=2">the docs</a> for details.`
	assert.Equal(t, want, Render(in))
}
