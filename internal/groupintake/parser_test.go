package groupintake

import (
	"testing"

	"bot-bvk/internal/domain"
)

func TestParseDepartmentTags(t *testing.T) {
	tags := map[domain.Department][]string{
		domain.OneC:         {"#1c", "#1с"},
		domain.IT:           {"#it", "#ит"},
		domain.Installation: {"#монтаж"},
		domain.Field:        {"#выезд"},
	}
	for _, tc := range []struct {
		name string
		text string
		want domain.Department
		out  string
	}{
		{"latin", "#1C Не открывается 1С", domain.OneC, "Не открывается 1С"},
		{"cyrillic", "#1с Не печатает", domain.OneC, "Не печатает"},
		{"it uppercase", "#IT VPN", domain.IT, "VPN"},
		{"cyrillic it", "#ит VPN", domain.IT, "VPN"},
		{"middle", "Проблема #монтаж с кабелем", domain.Installation, "Проблема с кабелем"},
		{"field", "#выезд завтра", domain.Field, "завтра"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.text, tags, "")
			if got.Department != tc.want || got.Text != tc.out || got.Ambiguous {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestParseUrgentAndMention(t *testing.T) {
	tags := map[domain.Department][]string{domain.OneC: {"#1c"}}
	got := Parse("@SupportBot #1C #срочно Не открывается 1С", tags, "supportbot")
	if !got.Mentioned || !got.Urgent || got.Department != domain.OneC || got.Text != "Не открывается 1С" {
		t.Fatalf("got %+v", got)
	}
	got = Parse("@SupportBot #urgent нужна помощь", map[domain.Department][]string{domain.IT: {"#it"}}, "SupportBot")
	if !got.Mentioned || !got.Urgent || got.HasDepartment() || got.Text != "нужна помощь" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseMentionWithoutTag(t *testing.T) {
	got := Parse("@SupportBot помогите", nil, "supportbot")
	if !got.Mentioned || got.HasDepartment() || got.Text != "помогите" {
		t.Fatalf("got %+v", got)
	}
	if Parse("@SupportBot помогите", nil, "").Mentioned {
		t.Fatal("mention matched without a bot username")
	}
}

func TestParseAmbiguousDepartments(t *testing.T) {
	got := Parse("#1c #it проблема", map[domain.Department][]string{domain.OneC: {"#1c"}, domain.IT: {"#it"}}, "")
	if !got.Ambiguous || got.HasDepartment() || got.Text != "проблема" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseRepeatedDepartmentIsNotAmbiguous(t *testing.T) {
	got := Parse("#1c #1с проблема", map[domain.Department][]string{domain.OneC: {"#1c", "#1с"}}, "")
	if got.Ambiguous || got.Department != domain.OneC || got.Text != "проблема" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseLeavesUnknownAndURLFragments(t *testing.T) {
	for _, text := range []string{
		"#999 проблема",
		"https://example.org/page#1c",
		"почта@example.org",
	} {
		got := Parse(text, map[domain.Department][]string{domain.OneC: {"#1c"}}, "bot")
		if got.HasDepartment() || got.Mentioned || got.Ambiguous || got.Text != text {
			t.Fatalf("%q -> %+v", text, got)
		}
	}
}

func TestParseEmptyAndTagsOnly(t *testing.T) {
	got := Parse("", map[domain.Department][]string{domain.OneC: {"#1c"}}, "bot")
	if got.Text != "" || got.HasDepartment() {
		t.Fatalf("got %+v", got)
	}
	got = Parse("#1c #срочно", map[domain.Department][]string{domain.OneC: {"#1c"}}, "bot")
	if got.Department != domain.OneC || !got.Urgent || got.Text != "" {
		t.Fatalf("got %+v", got)
	}
}
