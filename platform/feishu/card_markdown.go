package feishu

import "strings"

// cardMarkdown makes text written for a card's markdown element display as
// written. Feishu drops anything shaped like an HTML tag ("/model <number>"
// shows as "/model"), inside inline code as well, and inside code it shows
// character references literally. So outside fenced code blocks "<" and ">"
// become references, and an inline code span that holds them is shown as
// plain text (with its own markdown characters escaped). A ">" that starts
// a line is a quote and stays. Fenced code blocks are left alone.
func cardMarkdown(s string) string {
	if !strings.ContainsAny(s, "<>") {
		return s
	}
	lines := strings.Split(s, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if !fenced {
			lines[i] = cardMarkdownLine(line)
		}
	}
	return strings.Join(lines, "\n")
}

var (
	cardAngles   = strings.NewReplacer("<", "&lt;", ">", "&gt;")
	cardCodeText = strings.NewReplacer("<", "&lt;", ">", "&gt;", "*", "&#42;", "_", "&#95;", "~", "&#126;")
)

func cardMarkdownLine(line string) string {
	if !strings.ContainsAny(line, "<>") {
		return line
	}
	// Quote markers: "> ", "> > ", with any indentation.
	rest := strings.TrimLeft(line, " \t")
	for strings.HasPrefix(rest, ">") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	var b strings.Builder
	b.WriteString(line[:len(line)-len(rest)])
	for rest != "" {
		i := strings.IndexByte(rest, '`')
		if i < 0 {
			b.WriteString(cardAngles.Replace(rest))
			break
		}
		b.WriteString(cardAngles.Replace(rest[:i]))
		n := i
		for n < len(rest) && rest[n] == '`' {
			n++
		}
		delim := rest[i:n]
		j := strings.Index(rest[n:], delim)
		if j < 0 { // no closing backticks: not code
			b.WriteString(delim)
			rest = rest[n:]
			continue
		}
		code := rest[n : n+j]
		if strings.ContainsAny(code, "<>") {
			b.WriteString(cardCodeText.Replace(code))
		} else {
			b.WriteString(delim + code + delim)
		}
		rest = rest[n+j+len(delim):]
	}
	return b.String()
}
