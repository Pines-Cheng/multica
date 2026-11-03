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
		Elements []decodedIssueCardElement `json:"elements"`
	} `json:"body"`
}

type decodedIssueCardElement struct {
	Tag       string                    `json:"tag"`
	Content   string                    `json:"content"`
	TextAlign string                    `json:"text_align"`
	Columns   []decodedIssueCardElement `json:"columns"`
	Elements  []decodedIssueCardElement `json:"elements"`
	Text      struct {
		Content string `json:"content"`
	} `json:"text"`
	Behaviors []struct {
		Type       string `json:"type"`
		DefaultURL string `json:"default_url"`
	} `json:"behaviors"`
}

func cardMarkdown(element decodedIssueCardElement) string {
	text := element.Content
	for _, column := range element.Columns {
		text += "\n" + cardMarkdown(column)
	}
	for _, child := range element.Elements {
		text += "\n" + cardMarkdown(child)
	}
	return text
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

func TestIssueCard_LightSectionsKeepProgressAndAllArtifactLinks(t *testing.T) {
	links := metadataCardLinks([]byte(`{"Bits":"https://bits.example/task","MR":"https://code.example/mr","Meego":"https://meego.example/story","PPE":"https://ppe.example/?env=test"," prd ":"https://docs.example/spec(4)#section","自测报告":"https://tests.example/report"}`))
	raw, err := marshalIssueCard(issueCardContent{
		progress: []issueCardProgress{
			{title: "[开发](https://multica.example/dev/issues/DEV-30)", status: "已完成", assignee: "Developer & Reviewer"},
			{title: "自定义阶段", status: "人工确认"},
		},
		links: links, action: issueCardLink{label: "查看任务", url: "https://multica.example/dev/issues/DEV-29"},
		footnotes: []string{progressHint},
	})
	if err != nil {
		t.Fatal(err)
	}
	card := decodeIssueCard(t, raw)
	if len(card.Body.Elements) != 4 || card.Body.Elements[2].Tag != "button" {
		t.Fatalf("progress and artifacts must precede the unchanged task action: %s", raw)
	}
	progress := cardMarkdown(card.Body.Elements[0])
	for _, want := range []string{"**子任务进度**", "[开发](https://multica.example/dev/issues/DEV-30)", "已完成", "Developer &amp; Reviewer", "自定义阶段", "人工确认"} {
		if !strings.Contains(progress, want) {
			t.Errorf("progress lost %q: %s", want, progress)
		}
	}
	section := card.Body.Elements[1]
	if section.Tag != "column_set" || len(section.Columns) != 1 {
		t.Fatalf("artifacts must form a single text section: %+v", section)
	}
	rows := section.Columns[0].Elements
	if len(rows) != 4 || rows[0].Content != "**关键产物**" {
		t.Fatalf("expected a heading and three rows: %+v", rows)
	}
	for i, label := range []string{"需求文档 · PRD", "需求详情 · Meego", "开发任务 · Bits", "代码变更 · MR", "PPE 环境", "自测报告"} {
		row := rows[1+i/2]
		if len(row.Columns) != 2 || len(row.Columns[i%2].Elements) != 1 {
			t.Fatalf("artifact text must remain in two columns: %+v", row)
		}
		link := row.Columns[i%2].Elements[0]
		if link.Tag != "markdown" || !strings.Contains(link.Content, "["+label+"](") {
			t.Errorf("artifact %s is not a text link: %+v", label, link)
		}
	}
	artifacts := cardMarkdown(section)
	for _, link := range links {
		encodedURL := strings.NewReplacer("(", "%28", ")", "%29").Replace(link.url)
		if !strings.Contains(artifacts, encodedURL) {
			t.Errorf("artifact URL lost: %s", link.url)
		}
	}
	if strings.Contains(raw, "background_style") || strings.Contains(raw, "callback") || strings.Count(raw, `"tag":"button"`) != 1 {
		t.Fatalf("light sections must not introduce fills, buttons or callbacks: %s", raw)
	}
	for _, invalid := range [][]issueCardLink{nil, {{label: "PRD", url: "javascript:alert(1)"}}} {
		empty, err := marshalIssueCard(issueCardContent{links: invalid})
		if err != nil || strings.Contains(empty, "关键产物") {
			t.Fatalf("no valid links must omit the artifact section: %s (%v)", empty, err)
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
