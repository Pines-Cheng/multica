package lark

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestReplaceMentionKeys_LongestKeyFirst(t *testing.T) {
	got := replaceMentionKeys("@_user_1 and @_user_10 please", map[string]string{
		"@_user_1":  "[@A](mention://agent/a)",
		"@_user_10": "@B",
	})
	want := "[@A](mention://agent/a) and @B please"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMentionLabel_CannotBreakMentionSyntax(t *testing.T) {
	if got := mentionLabel(" Bad](name\n "); got != "Bad (name" {
		t.Fatalf("got %q", got)
	}
}

func TestDecoder_CapturesSenderIdentityAndMentions(t *testing.T) {
	raw := []byte(`{
		"type":"event_callback",
		"header":{"event_id":"e","event_type":"im.message.receive_v1","app_id":"a"},
		"event":{
			"sender":{"sender_id":{"open_id":"ou_user","union_id":"on_user"},"sender_type":"user"},
			"message":{
				"message_id":"m","chat_id":"c","chat_type":"group",
				"message_type":"text","content":"{\"text\":\"@_user_1 hi\"}",
				"mentions":[{"key":"@_user_1","name":"Coder","id":{"open_id":"ou_bot","union_id":"on_bot"}}]
			}
		}
	}`)
	msg, ok, err := NewLarkJSONFrameDecoder().Decode(raw, Installation{})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if msg.SenderType != "user" || msg.SenderUnionID != "on_user" {
		t.Fatalf("sender identity not captured: %+v", msg)
	}
	if len(msg.Mentions) != 1 || msg.Mentions[0] != (InboundMention{Key: "@_user_1", Name: "Coder", OpenID: "ou_bot", UnionID: "on_bot"}) {
		t.Fatalf("mentions = %+v", msg.Mentions)
	}
	if got := channelMessageFromLark(msg).Source.SenderStableID; got != "on_user" {
		t.Fatalf("SenderStableID = %q, want on_user", got)
	}
}

func decodeAddons(t *testing.T, encoded string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	return out
}

// The QR carries the extra scopes issue chats need, encoded the way the
// upstream SDK encodes Options.Addons, and the existing app id when the
// agent already owns a bot so Lark updates that app instead of creating one.
func TestDecorateQRCodeURL_AddonsAndClientID(t *testing.T) {
	qr, err := decorateQRCodeURL("https://accounts.feishu.cn/qr?code=x", "multica", "", "cli_existing")
	if err != nil {
		t.Fatalf("decorate: %v", err)
	}
	u, _ := url.Parse(qr)
	q := u.Query()
	if q.Get("clientID") != "cli_existing" {
		t.Fatalf("clientID = %q", q.Get("clientID"))
	}
	addons := decodeAddons(t, q.Get("addons"))
	scopes, _ := addons["scopes"].(map[string]any)
	tenant, _ := scopes["tenant"].([]any)
	got := map[string]bool{}
	for _, s := range tenant {
		got[s.(string)] = true
	}
	for _, want := range []string{"im:message.group_msg", "im:chat.members:write_only"} {
		if !got[want] {
			t.Errorf("addons missing scope %q: %v", want, tenant)
		}
	}

	fresh, err := decorateQRCodeURL("https://accounts.feishu.cn/qr?code=x", "multica", "", "")
	if err != nil {
		t.Fatalf("decorate: %v", err)
	}
	fu, _ := url.Parse(fresh)
	if fu.Query().Has("clientID") {
		t.Fatal("a first install must not name an app to update")
	}
}

func TestCommentSyncJob_SkipsCommentsFromChannel(t *testing.T) {
	payload := map[string]any{
		"comment":               map[string]any{"id": "c1", "issue_id": "i1", "content": "hi"},
		CommentChannelOriginKey: CommentChannelOrigin{ChannelType: "feishu", MessageID: "om_1"},
	}
	if _, ok := commentSyncJobFromEvent(events.Event{Type: protocol.EventCommentCreated, Payload: payload}); ok {
		t.Fatal("a comment written from the chat must not be mirrored back")
	}
	delete(payload, CommentChannelOriginKey)
	job, ok := commentSyncJobFromEvent(events.Event{Type: protocol.EventCommentCreated, Payload: payload})
	if !ok || job.comment.ID != "c1" || job.comment.Content != "hi" {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
}

func TestCommentSyncJob_DeletedEvent(t *testing.T) {
	job, ok := commentSyncJobFromEvent(events.Event{Type: protocol.EventCommentDeleted, Payload: map[string]any{
		"comment_id": "c1", "issue_id": "i1",
	}})
	if !ok || job.comment.ID != "c1" || job.comment.IssueID != "i1" {
		t.Fatalf("job = %+v ok=%v", job, ok)
	}
}

func TestIssueChatCard_RelayedEntryNamesAuthor(t *testing.T) {
	card, err := issueChatCardJSON("Ada", "来自 Multica", "line one\nline two")
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	var doc struct {
		Header struct {
			Title struct {
				Content string `json:"content"`
			} `json:"title"`
		} `json:"header"`
		Config struct {
			UpdateMulti bool `json:"update_multi"`
			Summary     struct {
				Content string `json:"content"`
			} `json:"summary"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(card), &doc); err != nil {
		t.Fatalf("json: %v", err)
	}
	if doc.Header.Title.Content != "Ada" || !doc.Config.UpdateMulti || doc.Config.Summary.Content != "Ada：line one" {
		t.Fatalf("card = %s", card)
	}
	own, _ := issueChatCardJSON("", "", "hi")
	if strings.Contains(own, `"header"`) {
		t.Fatal("an agent speaking through its own bot needs no header")
	}
}
