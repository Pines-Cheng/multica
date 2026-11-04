package lark

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

func TestArtifactTabs(t *testing.T) {
	metadata := []byte(`{"Bits":"https://bits.example/3","other":"https://other.example","bad":"javascript:alert(1)"}`)
	artifacts := []byte(`{"new-kind":{"title":"压测报告","url":"https://docs.example/report","kind":"load-test-v2"},"design":{"title":"技术方案","url":"https://docs.example/design"}}`)
	got := artifactTabs(metadata, artifacts)
	want := []ChatURLTab{
		{Key: "artifact:design", Name: "技术方案", URL: "https://docs.example/design"},
		{Key: "artifact:new-kind", Name: "压测报告", URL: "https://docs.example/report"},
		{Key: "metadata:Bits", Name: "Bits", URL: "https://bits.example/3"},
		{Key: "metadata:other", Name: "other", URL: "https://other.example"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tabs = %+v, want %+v", got, want)
	}
	for _, raw := range []string{"", "null", "[]", "bad", `{"PRD":3,"MR":"javascript:alert(1)","Bits":"https://","PPE":"/relative","Meego":"https://bad.example/has space"}`} {
		if got := artifactTabs([]byte(raw), nil); len(got) != 0 {
			t.Errorf("artifactTabs(%q) = %+v, want none", raw, got)
		}
	}
}

func TestArtifactTabs_LongAndRepeatedTitles(t *testing.T) {
	got := artifactTabs(nil, []byte(`{"a":{"title":"这是一份很长很长的技术方案文档","url":"https://docs.example/a"},"b":{"title":"这是一份很长很长的技术方案文档","url":"https://docs.example/b"},"c":{"title":"另一份文档","url":"https://docs.example/c"}}`))
	seen := map[string]bool{}
	if len(got) != 3 {
		t.Fatalf("tabs = %+v", got)
	}
	for _, tab := range got {
		if len(tab.Name) > 30 || !utf8.ValidString(tab.Name) || seen[tab.Name] {
			t.Fatalf("invalid or duplicate tab title: %+v", got)
		}
		seen[tab.Name] = true
	}
}

func TestArtifactTabs_ExplicitRecordReplacesMetadataKeyAfterURLChange(t *testing.T) {
	got := artifactTabs([]byte(`{"report":"https://docs.example/old","other":"https://docs.example/other"}`),
		[]byte(`{"report":{"title":"最新报告","url":"https://docs.example/new","kind":"new-kind"}}`))
	if len(got) != 2 || got[0].URL != "https://docs.example/new" || got[0].Name != "最新报告" || got[1].Name != "other" {
		t.Fatalf("explicitly registered key must not expose a stale metadata URL: %+v", got)
	}
}
