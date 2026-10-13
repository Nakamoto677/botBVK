package telegram

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ordinary Bot API: 4096 text / 1024 caption after entity parsing.
// UTF-16 budgets are conservative for astral emoji. Plain text is split before
// formatting; HTML cards are bounded before escaping instead of cutting tags.
const messageLimit = 4096
const captionLimit = 1024

func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}
func splitText(s string, limit int) []string {
	if s == "" {
		return nil
	}
	var parts []string
	for utf16Length(s) > limit {
		end, used, lastSpace := 0, 0, 0
		previous := rune(0)
		regional := 0
		for off, r := range s {
			units := 1
			if r > 0xffff {
				units = 2
			}
			continuation := unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d || previous == 0x200d || r >= 0x1f3fb && r <= 0x1f3ff || r >= 0xfe00 && r <= 0xfe0f
			if r >= 0x1f1e6 && r <= 0x1f1ff {
				continuation = regional%2 == 1
				regional++
			} else {
				regional = 0
			}
			if used+units > limit {
				break
			}
			// end records only whole clusters, never a surrogate/combining/ZWJ boundary.
			if off > 0 && !continuation {
				end = off
			}
			used += units
			previous = r
			if unicode.IsSpace(r) {
				lastSpace = off + utf8.RuneLen(r)
			}
		}
		if lastSpace > 0 && lastSpace <= end {
			end = lastSpace
		}
		if end == 0 { // pathological cluster longer than the API limit
			_, size := utf8.DecodeRuneInString(s)
			end = size
		}
		parts = append(parts, s[:end])
		s = s[end:]
	}
	if s != "" {
		parts = append(parts, s)
	}
	return parts
}
func previewText(s string, limit int) string {
	if utf16Length(s) <= limit {
		return s
	}
	parts := splitText(s, limit-64)
	return strings.TrimRight(parts[0], " \n") + "\n… Предпросмотр сокращён. Полный текст сохранится в истории."
}
