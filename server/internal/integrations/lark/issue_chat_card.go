package lark

import (
	"encoding/json"
	"fmt"
	"strings"
)

// issueChatCardJSON renders a mirrored timeline entry as a schema-2.0 card.
// Every mirror is a card, not a text message, so a later comment edit or
// delete can patch it in place. A relayed entry (posted by the steward on
// someone's behalf) carries the author as the header title.
func issueChatCardJSON(title, subtitle, markdown string) (string, error) {
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"update_multi": true,
			"summary":      map[string]any{"content": cardSummary(title, markdown)},
		},
		"body": map[string]any{
			"elements": []any{
				map[string]any{"tag": "markdown", "content": markdownImageLinks(markdown)},
			},
		},
	}
	if title != "" {
		header := map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": title},
			"template": "grey",
		}
		if subtitle != "" {
			header["subtitle"] = map[string]any{"tag": "plain_text", "content": subtitle}
		}
		card["header"] = header
	}
	out, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("lark issue chat: encode card: %w", err)
	}
	return string(out), nil
}

// cardSummary is the one-line preview Lark shows in the chat list.
func cardSummary(title, markdown string) string {
	line := strings.TrimSpace(markdown)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if r := []rune(line); len(r) > 60 {
		line = string(r[:60]) + "…"
	}
	if title != "" {
		return title + "：" + line
	}
	return line
}
