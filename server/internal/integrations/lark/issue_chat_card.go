package lark

import (
	"encoding/json"
	"fmt"
	"net/url"
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
	action    issueCardLink
	links     []issueCardLink
	footnotes []string
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
	if validIssueCardURL(content.action.url) {
		elements = append(elements, map[string]any{
			"tag": "button", "type": "primary",
			"text":      map[string]any{"tag": "plain_text", "content": content.action.label},
			"behaviors": []any{map[string]any{"type": "open_url", "default_url": content.action.url}},
		})
	}
	var links []string
	for _, link := range content.links {
		if validIssueCardURL(link.url) {
			links = append(links, issueCardMarkdownLink(link))
		}
	}
	if len(links) > 0 {
		elements = append(elements, map[string]any{"tag": "markdown", "content": strings.Join(links, " · ")})
	}
	for _, note := range content.footnotes {
		if note != "" {
			elements = append(elements, map[string]any{"tag": "markdown", "content": note, "text_size": "notation"})
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

func validIssueCardURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && !strings.ContainsAny(raw, "\r\n\t ")
}

func issueCardMarkdownLink(link issueCardLink) string {
	label := strings.NewReplacer("[", "", "]", "", "\n", " ", "\r", " ").Replace(link.label)
	url := strings.NewReplacer("(", "%28", ")", "%29").Replace(link.url)
	return fmt.Sprintf("[%s](%s)", label, url)
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
