package lark

import (
	"reflect"
	"testing"
)

func TestArtifactTabs(t *testing.T) {
	got := artifactTabs([]byte(`{"prd":"https://docs.example/spec#section"," Meego ":"https://meego.example/1","MR":"https://code.example/2","Bits":"https://bits.example/3","PPE":"https://ppe.example/?env=test","other":"https://other.example","bad":"javascript:alert(1)"}`))
	want := []ChatURLTab{
		{Name: "Bits", URL: "https://bits.example/3"},
		{Name: "MR", URL: "https://code.example/2"},
		{Name: "Meego", URL: "https://meego.example/1"},
		{Name: "PPE", URL: "https://ppe.example/?env=test"},
		{Name: "PRD", URL: "https://docs.example/spec#section"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tabs = %+v, want %+v", got, want)
	}
	for _, raw := range []string{"", "null", "[]", "bad", `{"PRD":3,"MR":"javascript:alert(1)","Bits":"https://","PPE":"/relative","Meego":"https://bad.example/has space"}`} {
		if got := artifactTabs([]byte(raw)); len(got) != 0 {
			t.Errorf("artifactTabs(%q) = %+v, want none", raw, got)
		}
	}
	got = artifactTabs([]byte(`{"PRD":"https://docs.example/a","prd":"https://docs.example/b"}`))
	if len(got) != 1 {
		t.Fatalf("duplicate metadata keys must produce one canonical tab: %+v", got)
	}
}
