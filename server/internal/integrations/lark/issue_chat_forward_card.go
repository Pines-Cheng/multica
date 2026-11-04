package lark

import "encoding/json"

// Forwarded cards are read-only snapshots. Convert their readable elements to
// post spans so text and embedded media share the comment attachment pipeline;
// callbacks and other card actions are never replayed.
func forwardedCardPost(raw string) (string, bool) {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return "", false
	}
	var paragraphs [][]larkPostSpan
	var visit func(any)
	visit = func(v any) {
		switch item := v.(type) {
		case string:
			if item != "" {
				paragraphs = append(paragraphs, []larkPostSpan{{Tag: "text", Text: item}})
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		case map[string]any:
			str := func(key string) string { s, _ := item[key].(string); return s }
			if key := firstNonEmpty(str("img_key"), str("image_key")); key != "" {
				paragraphs = append(paragraphs, []larkPostSpan{{Tag: "img", ImageKey: key}})
				visit(item["alt"])
				return
			}
			if key := str("file_key"); key != "" {
				tag := "file"
				if str("tag") == "video" || str("tag") == "media" {
					tag = "media"
				}
				paragraphs = append(paragraphs, []larkPostSpan{{Tag: tag, FileKey: key, FileName: firstNonEmpty(str("file_name"), str("name"))}})
				return
			}
			// These are content-bearing fields in both legacy and schema 2 cards.
			// Explicit ordering avoids Go map iteration changing a stored transcript.
			for _, key := range []string{"header", "title", "body", "elements", "columns", "fields", "text", "content", "actions", "url"} {
				visit(item[key])
			}
		}
	}
	visit(value)
	if len(paragraphs) == 0 {
		return "", false
	}
	post, err := json.Marshal(larkPostContent{Content: paragraphs})
	return string(post), err == nil
}
