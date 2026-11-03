package lark

import (
	"encoding/json"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type decodedIssueCard struct {
	Schema string `json:"schema"`
	Config struct {
		UpdateMulti bool `json:"update_multi"`
		Summary     struct {
			Content string `json:"content"`
		} `json:"summary"`
	} `json:"config"`
	Header struct {
		Template string `json:"template"`
		Title    struct {
			Content string `json:"content"`
		} `json:"title"`
		Subtitle struct {
			Content string `json:"content"`
		} `json:"subtitle"`
	} `json:"header"`
	Body struct {
		Elements []struct {
			Tag     string `json:"tag"`
			Content string `json:"content"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
			Behaviors []struct {
				Type       string `json:"type"`
				DefaultURL string `json:"default_url"`
			} `json:"behaviors"`
		} `json:"elements"`
	} `json:"body"`
}

func decodeIssueCard(t *testing.T, raw string) decodedIssueCard {
	t.Helper()
	var card decodedIssueCard
	if err := json.Unmarshal([]byte(raw), &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	return card
}

func TestIssueCard_LinkButtonsOnlyNavigate(t *testing.T) {
	title := strings.Repeat("项目空间搜索", 20)
	raw, err := marshalIssueCard(issueCardContent{
		title: title, subtitle: "DEV-23 · 评审中 · Ada", summary: "评审中 · DEV-23", template: "orange",
		sections: []string{"交付内容", "自测结果"},
		action:   issueCardLink{label: "查看交付", url: "https://multica.example/dev/issues/DEV-23"},
		links:    []issueCardLink{{label: "需求文档", url: "https://docs.example/spec(4)"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	card := decodeIssueCard(t, raw)
	if card.Schema != "2.0" || !card.Config.UpdateMulti || card.Header.Title.Content != title || card.Config.Summary.Content != "评审中 · DEV-23" {
		t.Fatalf("card must remain patchable and retain its full title: %s", raw)
	}
	buttons := 0
	for _, element := range card.Body.Elements {
		if element.Tag == "button" {
			buttons++
			if element.Text.Content != "查看交付" || len(element.Behaviors) != 1 || element.Behaviors[0].Type != "open_url" || element.Behaviors[0].DefaultURL != "https://multica.example/dev/issues/DEV-23" {
				t.Fatalf("button must navigate without a callback: %+v", element)
			}
		}
	}
	if buttons != 1 || strings.Contains(raw, "callback") || !strings.Contains(raw, "https://docs.example/spec%284%29") {
		t.Fatalf("expected one main action and a valid secondary link: %s", raw)
	}
}

func TestMetadataCardLinks_OnlyUsesExistingWebLinks(t *testing.T) {
	links := metadataCardLinks([]byte(`{"PRD":"https://docs.example/spec(4)","Meego":"https://meego.example/1","note":"plain","number":3,"empty":"","bad":"https://","relative":"/reports/1","script":"javascript:alert(1)","":"https://example.com"}`))
	if len(links) != 2 || links[0].label != "Meego" || links[1].label != "PRD" {
		t.Fatalf("unexpected document links: %+v", links)
	}
	for _, raw := range []string{"", `{broken`, `{"report":null}`} {
		if got := metadataCardLinks([]byte(raw)); len(got) != 0 {
			t.Fatalf("empty or malformed metadata must not invent links: %+v", got)
		}
	}
}

func TestIssueCardStatus_UsesCustomNamesWithoutInventingReview(t *testing.T) {
	catalog := map[string]db.IssueStatus{
		"human_review": {Name: "人工确认", Category: "started"},
		"released":     {Name: "已发布", Category: "done"},
	}
	for _, tc := range []struct{ key, label, template string }{
		{"todo", "待办", "grey"},
		{"in_progress", "进行中", "blue"},
		{"in_review", "评审中", "orange"},
		{"blocked", "已阻塞", "red"},
		{"done", "已完成", "green"},
		{"cancelled", "已取消", "grey"},
		{"human_review", "人工确认", "blue"},
		{"released", "已发布", "green"},
		{"future_status", "future_status", "grey"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			label, template := issueCardStatus(tc.key, catalog)
			if label != tc.label || template != tc.template {
				t.Fatalf("status display = %q/%q, want %q/%q", label, template, tc.label, tc.template)
			}
		})
	}
}
