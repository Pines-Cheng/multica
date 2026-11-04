package issueartifact

import (
	"strings"
	"testing"
)

func TestArtifactValidation(t *testing.T) {
	a := Artifact{Title: " 新的交付类型 ", URL: "https://docs.example/new", Kind: " any-new-kind "}
	if err := a.Validate(); err != nil || a.Title != "新的交付类型" || a.Kind != "any-new-kind" {
		t.Fatalf("open classification and display title: %+v (%v)", a, err)
	}
	for _, a := range []Artifact{
		{Title: " ", URL: "https://docs.example"},
		{Title: "line\nbreak", URL: "https://docs.example"},
		{Title: strings.Repeat("文", 121), URL: "https://docs.example"},
		{Title: "report", URL: "javascript:alert(1)"},
		{Title: "report", URL: "/relative"},
		{Title: "report", URL: "https://"},
		{Title: "report", URL: "https://docs.example/has space"},
		{Title: "report", URL: "https://docs.example", Kind: strings.Repeat("x", 65)},
	} {
		if err := a.Validate(); err == nil {
			t.Errorf("accepted invalid artifact: %+v", a)
		}
	}
}

func TestRecordsUseTitlesAndStableKeysWithoutKindCatalog(t *testing.T) {
	got := Records([]byte(`{"z":{"title":"验收记录","url":"https://docs.example/z"},"a":{"title":"架构图","url":"https://docs.example/a","kind":"brand-new-kind"},"bad":{"title":"","url":"https://docs.example"}}`))
	if len(got) != 2 || got[0].Key != "a" || got[0].Title != "架构图" || got[0].Kind != "brand-new-kind" || got[1].Kind != "" {
		t.Fatalf("records = %+v", got)
	}
	for _, raw := range []string{"", "null", "[]", "not JSON"} {
		if got := Records([]byte(raw)); len(got) != 0 || got == nil {
			t.Errorf("Records(%q) = %+v", raw, got)
		}
	}
}
