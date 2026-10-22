package telegram

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzSplitTextPreservesInput(f *testing.F) {
	for _, s := range []string{"", "обычный текст", strings.Repeat("👩‍💻é 🇷🇺 ", 900), strings.Repeat("x", 8193), "<&>"} {
		f.Add(s, uint16(4096))
	}
	f.Fuzz(func(t *testing.T, s string, budget uint16) {
		if !utf8.ValidString(s) || len(s) > 32768 {
			t.Skip()
		}
		limit := 64 + int(budget)%4033
		parts := splitText(s, limit)
		if strings.Join(parts, "") != s {
			t.Fatal("text lost")
		}
		for _, part := range parts {
			if !utf8.ValidString(part) || part == "" || utf16Length(part) > limit {
				t.Fatalf("invalid split for limit %d", limit)
			}
		}
	})
}
