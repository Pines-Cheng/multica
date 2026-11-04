package lark

import (
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"sort"
	"strings"
)

type issueCardLink struct {
	label string
	url   string
}

type issueCardContent struct {
	title     string
	subtitle  string
	summary   string
	template  string
	sections  []string
	progress  []issueCardProgress
	action    issueCardLink
	links     []issueCardLink
	footnotes []string
}

type issueCardProgress struct {
	title    string
	status   string
	assignee string
}

// issueChatCardJSON renders a mirrored timeline entry as a schema-2.0 card.
// Every mirror is a card, not a text message, so a later comment edit or
// delete can patch it in place. A relayed entry (posted by the steward on
// someone's behalf) carries the author as the header title.
func issueChatCardJSON(title, subtitle, markdown string) (string, error) {
	return issueChatCardJSONWithTemplate(title, subtitle, markdown, "grey")
}

// issueChatCardJSONWithTemplate is issueChatCardJSON with the header colour
// chosen by the caller.
func issueChatCardJSONWithTemplate(title, subtitle, markdown, template string) (string, error) {
	return marshalIssueCard(issueCardContent{
		title: title, subtitle: subtitle, summary: cardSummary(title, markdown),
		template: template, sections: []string{markdown},
	})
}

func marshalIssueCard(content issueCardContent) (string, error) {
	elements := make([]any, 0, len(content.sections)+3)
	for _, section := range content.sections {
		if strings.TrimSpace(section) != "" {
			elements = append(elements, map[string]any{"tag": "markdown", "content": markdownImageLinks(section)})
		}
	}
	if len(content.progress) > 0 {
		rows := []any{map[string]any{"tag": "markdown", "content": "**子任务进度**"}}
		for _, item := range content.progress {
			detail := html.EscapeString(item.status)
			if item.assignee != "" {
				detail += " · <font color='grey'>" + html.EscapeString(item.assignee) + "</font>"
			}
			rows = append(rows, issueCardColumns(
				map[string]any{"tag": "markdown", "content": item.title},
				map[string]any{"tag": "markdown", "content": detail, "text_align": "right"},
			))
		}
		elements = append(elements, issueCardSection(rows))
	}
	links := issueCardArtifactLinks(content.links)
	if len(links) > 0 {
		rows := []any{map[string]any{"tag": "markdown", "content": "**相关资料**"}}
		for i := 0; i < len(links); i += 2 {
			columns := []any{map[string]any{"tag": "markdown", "content": issueCardMarkdownLink(links[i])}}
			if i+1 < len(links) {
				columns = append(columns, map[string]any{"tag": "markdown", "content": issueCardMarkdownLink(links[i+1])})
			}
			rows = append(rows, issueCardColumns(columns...))
		}
		elements = append(elements, issueCardSection(rows))
	}
	if validIssueCardURL(content.action.url) {
		elements = append(elements, map[string]any{
			"tag": "button", "type": "primary",
			"text":      map[string]any{"tag": "plain_text", "content": content.action.label},
			"behaviors": []any{map[string]any{"type": "open_url", "default_url": content.action.url}},
		})
	}
	for _, note := range content.footnotes {
		if note != "" {
			elements = append(elements, map[string]any{"tag": "markdown", "content": "<font color='grey'>" + html.EscapeString(note) + "</font>", "text_size": "notation"})
		}
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"summary":      map[string]any{"content": content.summary},
		},
		"body": map[string]any{
			"direction": "vertical",
			"padding":   "16px 16px 16px 16px",
			"elements":  elements,
		},
	}
	if content.title != "" {
		header := map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": content.title},
			"template": content.template,
			"padding":  "12px 16px 12px 16px",
		}
		if content.subtitle != "" {
			header["subtitle"] = map[string]any{"tag": "plain_text", "content": content.subtitle}
		}
		card["header"] = header
	}
	out, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("lark issue chat: encode card: %w", err)
	}
	return string(out), nil
}

// Sections use spacing and alignment only; they have no background or border.
func issueCardSection(elements []any) map[string]any {
	return map[string]any{
		"tag": "column_set", "flex_mode": "none",
		"columns": []any{map[string]any{
			"tag": "column", "width": "weighted", "weight": 1,
			"vertical_spacing": "4px", "elements": elements,
		}},
	}
}

func issueCardColumns(elements ...any) map[string]any {
	columns := make([]any, 0, len(elements))
	for _, element := range elements {
		columns = append(columns, map[string]any{
			"tag": "column", "width": "weighted", "weight": 1, "elements": []any{element},
		})
	}
	return map[string]any{"tag": "column_set", "flex_mode": "none", "horizontal_spacing": "12px", "columns": columns}
}

func issueCardArtifactLinks(links []issueCardLink) []issueCardLink {
	ordered := make([]issueCardLink, 0, len(links))
	for _, link := range links {
		if validIssueCardURL(link.url) {
			ordered = append(ordered, link)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		_, a := issueCardArtifactLabel(ordered[i].label)
		_, b := issueCardArtifactLabel(ordered[j].label)
		return a < b
	})
	for i := range ordered {
		ordered[i].label, _ = issueCardArtifactLabel(ordered[i].label)
	}
	return ordered
}

func issueCardArtifactLabel(label string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "prd":
		return "需求文档 · PRD", 0
	case "techdesign":
		return "技术方案", 1
	case "meego":
		return "需求详情 · Meego", 2
	case "bits":
		return "开发任务 · Bits", 3
	case "mr":
		return "代码变更 · MR", 4
	case "ppe":
		return "PPE 环境", 5
	default:
		return label, 6
	}
}

func validIssueCardURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && !strings.ContainsAny(raw, "\r\n\t ")
}

func issueCardMarkdownLink(link issueCardLink) string {
	label := strings.NewReplacer("[", "", "]", "", "\n", " ", "\r", " ").Replace(link.label)
	url := strings.NewReplacer("(", "%28", ")", "%29").Replace(link.url)
	return fmt.Sprintf("[%s](%s)", label, url)
}

// Task titles remain plain text when displayed in the Markdown card body.
func issueCardMarkdownText(value string) string {
	return html.EscapeString(strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "~", "\\~",
		"[", "\\[", "]", "\\]", "#", "\\#", "!", "\\!", "|", "\\|",
		"-", "\\-", "+", "\\+", ".", "\\.",
	).Replace(value))
}

// cardSummary is the one-line preview Lark shows in the chat list.
func cardSummary(title, markdown string) string {
	line := strings.TrimSpace(markdown)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if r := []rune(line); len(r) > 60 {
		line = string(r[:60]) + "..."
	}
	if title != "" {
		return title + "：" + line
	}
	return line
}
