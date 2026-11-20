package feishu

import (
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestCardMarkdown(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"**Current:** `opus`", "**Current:** `opus`"},
		{"Usage: `/model <number>` or `/model <model_name>`", "Usage: /model &lt;number&gt; or /model &lt;model&#95;name&gt;"},
		{"Usage: /cron add <expr> <prompt>", "Usage: /cron add &lt;expr&gt; &lt;prompt&gt;"},
		{"x < y and y > z", "x &lt; y and y &gt; z"},
		{"> quoted <tag>\n> > nested", "> quoted &lt;tag&gt;\n> > nested"},
		{"``a`<b>`` and `*/5 * * * *`", "a`&lt;b&gt; and `*/5 * * * *`"},
		{"unclosed ` <x>", "unclosed ` &lt;x&gt;"},
		{"before <a>\n```\nif a < b { return <c> }\n```\nafter <d>", "before &lt;a&gt;\n```\nif a < b { return <c> }\n```\nafter &lt;d&gt;"},
		{"`<em>*bold*</em>`", "&lt;em&gt;&#42;bold&#42;&lt;/em&gt;"},
	} {
		if got := cardMarkdown(tc.in); got != tc.want {
			t.Errorf("cardMarkdown(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// The card a /model reply builds keeps its usage placeholders on Feishu.
func TestRenderCardEscapesPlaceholders(t *testing.T) {
	card := core.NewCard().Title("Model", "blue").
		Markdown("Usage: `/model <number>`").
		ListItem("`<id>` item", "Pick", "act:/x").
		Build()
	out := renderCard(card, "")
	if strings.Contains(out, "\\u003cnumber\\u003e") || strings.Contains(out, "<number>") ||
		!strings.Contains(out, "/model \\u0026lt;number\\u0026gt;") || !strings.Contains(out, "\\u0026lt;id\\u0026gt; item") {
		t.Fatalf("card = %s", out)
	}
}
