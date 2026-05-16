// Package render provides Telegram-aware text/template rendering with
// MarkdownV2 and HTML escape helpers.
package render

import "strings"

// markdownV2Specials lists every character that Telegram MarkdownV2
// considers reserved and therefore requires backslash escaping (per the
// Telegram Bot API docs).
const markdownV2Specials = "_*[]()~`>#+-=|{}.!\\"

// EscapeMarkdownV2 returns s with every MarkdownV2 reserved character
// preceded by a backslash. Safe to apply to arbitrary user-supplied data.
func EscapeMarkdownV2(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		if isMarkdownV2Special(r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isMarkdownV2Special(r rune) bool {
	if r > 127 {
		return false
	}
	for _, sp := range markdownV2Specials {
		if r == sp {
			return true
		}
	}
	return false
}

// EscapeHTML escapes only the three characters Telegram's HTML parse mode
// requires: <, >, &. Quotes and apostrophes are NOT touched.
func EscapeHTML(s string) string {
	if s == "" {
		return s
	}
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(s)
}
