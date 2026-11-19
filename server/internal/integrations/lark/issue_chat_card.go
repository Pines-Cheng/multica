package lark

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/multica-ai/multica/server/internal/issueartifact"
)

type issueCardLink struct {
	key   string
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
	title      string
	status     string
	assignee   string
	assistance string
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
			title := item.title
			if item.assistance != "" {
				title += " · " + item.assistance
			}
			rows = append(rows, issueCardColumns(
				map[string]any{"tag": "markdown", "content": title},
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
	return ordered
}

func validIssueCardURL(raw string) bool {
	return issueartifact.ValidURL(raw)
}

func issueCardMarkdownLink(link issueCardLink) string {
	label := issueCardMarkdownText(strings.NewReplacer("\n", " ", "\r", " ").Replace(link.label))
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
