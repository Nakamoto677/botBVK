// Package groupintake parses explicit support triggers from a company group
// chat. It is deliberately free of Telegram and database dependencies so the
// trigger grammar can be tested and changed independently of the adapter.
package groupintake

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"bot-bvk/internal/domain"
)

// Trigger is the parsed meaning of one group message. Ambiguous is set when the
// author named more than one department in the same message; Department is then
// empty and the caller must ask for one tag instead of guessing.
type Trigger struct {
	Department domain.Department
	Urgent     bool
	Mentioned  bool
	Ambiguous  bool
	Text       string
}

func (t Trigger) HasDepartment() bool { return t.Department.Valid() }

// Parse finds department hashtags, urgency tags and the bot mention in text.
// Matched tokens and the bot mention are removed from Text; unknown tokens stay
// untouched. Tag comparison is case-insensitive and works with Cyrillic and
// Latin aliases.
func Parse(text string, tags map[domain.Department][]string, botUsername string) Trigger {
	lookup := make(map[string]domain.Department)
	for department, aliases := range tags {
		for _, alias := range aliases {
			lookup[strings.ToLower(strings.TrimSpace(alias))] = department
		}
	}
	urgent := make(map[string]bool)
	for _, tag := range domain.UrgencyTags() {
		urgent[strings.ToLower(tag)] = true
	}
	username := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(botUsername), "@"))

	var result Trigger
	matched := make(map[domain.Department]bool)
	var kept strings.Builder
	last := 0
	previous := rune(0)
	hasPrevious := false

	for offset := 0; offset < len(text); {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if (r == '#' || r == '@') && isBoundary(previous, hasPrevious) {
			end := offset + size
			for end < len(text) {
				next, nextSize := utf8.DecodeRuneInString(text[end:])
				if !isTagRune(next) {
					break
				}
				end += nextSize
			}
			if end > offset+size {
				token := strings.ToLower(text[offset:end])
				switch r {
				case '#':
					if department, ok := lookup[token]; ok {
						matched[department] = true
						kept.WriteString(text[last:offset])
						kept.WriteByte(' ')
						last = end
						previous, hasPrevious = ' ', true
						offset = end
						continue
					}
					if urgent[token] {
						result.Urgent = true
						kept.WriteString(text[last:offset])
						kept.WriteByte(' ')
						last = end
						previous, hasPrevious = ' ', true
						offset = end
						continue
					}
				case '@':
					if username != "" && token == "@"+username {
						result.Mentioned = true
						kept.WriteString(text[last:offset])
						kept.WriteByte(' ')
						last = end
						previous, hasPrevious = ' ', true
						offset = end
						continue
					}
				}
			}
		}
		previous, hasPrevious = r, true
		offset += size
	}
	kept.WriteString(text[last:])
	result.Text = normalizeSpaces(strings.TrimSpace(kept.String()))

	switch len(matched) {
	case 0:
	case 1:
		for department := range matched {
			result.Department = department
		}
	default:
		result.Ambiguous = true
	}
	return result
}

func normalizeSpaces(text string) string {
	var out strings.Builder
	space := false
	for _, r := range text {
		if r == ' ' || r == '\t' {
			space = true
			continue
		}
		if space && out.Len() > 0 && r != '\n' {
			out.WriteByte(' ')
		}
		space = false
		out.WriteRune(r)
	}
	return out.String()
}

func isTagRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// A tag starts at a word boundary. Slash and colon are excluded so fragments in
// URLs and custom Telegram links are never treated as support requests.
func isBoundary(previous rune, hasPrevious bool) bool {
	if !hasPrevious {
		return true
	}
	if isTagRune(previous) {
		return false
	}
	return previous != '/' && previous != ':'
}
