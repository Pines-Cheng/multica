package lark

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type forwardedCommentAPI struct {
	*enricherFakeClient
	media *fakeSender
}

func (f *forwardedCommentAPI) DownloadMessageResource(ctx context.Context, creds InstallationCredentials, p DownloadResourceParams) (DownloadedResource, error) {
	return f.media.DownloadMessageResource(ctx, creds, p)
}

func TestForwardedCommentSyncPreservesNestedMessagesAndRetriesOnlyMissingMedia(t *testing.T) {
	api := &forwardedCommentAPI{newEnricherFake(), &fakeSender{
		downloaded:       DownloadedResource{Data: []byte("attachment"), ContentType: "image/png"},
		downloadErrByKey: map[string]error{"file": errors.New("temporary failure")},
	}}
	api.userNames = map[string]string{"ou_alice": "Alice", "ou_bob": "Bob"}
	api.byID["om_forward"] = []LarkMessage{
		{MessageID: "om_forward", MessageType: "merge_forward"},
		{MessageID: "om_nested", MessageType: "merge_forward", SenderID: "ou_bob", CreateTime: "2000"},
		{MessageID: "om_post", MessageType: "post", SenderID: "ou_alice", CreateTime: "1000", Content: `{"content":[[{"tag":"text","text":"before"},{"tag":"img","image_key":"image"},{"tag":"text","text":"after"},{"tag":"file","file_key":"file","file_name":"report.pdf"}]]}`},
	}
	api.byID["om_nested"] = []LarkMessage{
		{MessageID: "om_nested", MessageType: "merge_forward"},
		{MessageID: "om_text", MessageType: "text", SenderID: "ou_bob", CreateTime: "3000", Content: `{"text":"@_user_1 use /issue, not a fresh command"}`, Mentions: []LarkMessageMention{{Key: "@_user_1", Name: "Coder"}}},
	}
	storage := &fakeMediaStorage{}
	r := NewFeishuIssueChatRouter(nil, nil, api, fakeCreds{secret: "plain"}, newDiscardLogger())
	r.SetMediaResolver(NewFeishuMediaResolver(api, fakeCreds{secret: "plain"}, storage, &fakeMediaLedger{}, newDiscardLogger()))
	lm := InboundMessage{MessageID: "om_forward", MessageType: "merge_forward"}
	inst := testMediaInstallation(t)
	first, err := r.PrepareForwardedComment(context.Background(), inst, lm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<details>", "Alice", "Bob", "1970-01-01T00:00:01Z", "before", "after", "report.pdf", "@Coder", "Retry sync"} {
		if !strings.Contains(first.Content, want) {
			t.Fatalf("missing %q in %s", want, first.Content)
		}
	}
	if strings.Index(first.Content, "Alice") > strings.Index(first.Content, "Bob") || strings.Contains(first.Content, "mention://") {
		t.Fatalf("message order or historical mention changed: %s", first.Content)
	}
	if len(first.Attachments) != 1 || len(api.media.downloadCalls) != 2 {
		t.Fatalf("attachments=%d downloads=%d", len(first.Attachments), len(api.media.downloadCalls))
	}
	delete(api.media.downloadErrByKey, "file")
	second, err := r.PrepareForwardedComment(context.Background(), inst, lm, first.SyncState)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second.Content, "Retry sync") || len(second.Attachments) != 1 || len(api.media.downloadCalls) != 3 {
		t.Fatalf("retry lost successes or retained failure: %+v, downloads=%d", second, len(api.media.downloadCalls))
	}
	if !strings.Contains(second.Content, uuidString(first.Attachments[0].ID)) {
		t.Fatalf("retry replaced successful image: %s", second.Content)
	}
	if api.media.downloadCalls[0].MessageID != "om_forward" || api.media.downloadCalls[2].MessageID != "om_forward" {
		t.Fatalf("resources must use the outer forward ID; child IDs return File not in msg: %+v", api.media.downloadCalls)
	}
	third, err := r.PrepareForwardedComment(context.Background(), inst, lm, second.SyncState)
	if err != nil || len(third.Attachments) != 0 || third.Content != second.Content {
		t.Fatalf("completed retry changed content: %+v, err=%v", third, err)
	}
}

func TestForwardedCommentSyncRetriesReadFailuresAndPreservesDeletedMessages(t *testing.T) {
	api := &forwardedCommentAPI{newEnricherFake(), &fakeSender{}}
	api.errByID["om_forward"] = errors.New("permission denied")
	r := NewFeishuIssueChatRouter(nil, nil, api, fakeCreds{secret: "plain"}, newDiscardLogger())
	lm := InboundMessage{MessageID: "om_forward", MessageType: "merge_forward"}
	first, err := r.PrepareForwardedComment(context.Background(), testMediaInstallation(t), lm, nil)
	if err != nil || !strings.Contains(first.Content, "Could not read forwarded messages") || !strings.Contains(first.Content, "Retry sync") {
		t.Fatalf("missing read failure: %+v, %v", first, err)
	}
	delete(api.errByID, "om_forward")
	api.byID["om_forward"] = []LarkMessage{
		{MessageID: "om_forward", MessageType: "merge_forward"},
		{MessageID: "om_deleted", MessageType: "text", Deleted: true, SenderName: "Alice", Content: `{"text":"deleted secret"}`},
		{MessageID: "om_mention", MessageType: "text", SenderName: "Bob", Content: `{"text":"[@Coder](mention://agent/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa) /issue historical"}`},
	}
	second, err := r.PrepareForwardedComment(context.Background(), testMediaInstallation(t), lm, first.SyncState)
	if err != nil || strings.Contains(second.Content, "Retry sync") || strings.Contains(second.Content, "deleted secret") || strings.Contains(second.Content, "mention://") || !strings.Contains(second.Content, "Deleted message") {
		t.Fatalf("read retry or historical content: %+v, %v", second, err)
	}
	if len(api.userCalls) != 0 {
		t.Fatalf("server-provided names must avoid contact lookup: %+v", api.userCalls)
	}
}

func TestForwardedCommentSyncUsesInlineTreeAndAllMediaKinds(t *testing.T) {
	api := &forwardedCommentAPI{newEnricherFake(), &fakeSender{downloaded: DownloadedResource{Data: []byte("bytes"), ContentType: "application/octet-stream"}}}
	api.byID["om_forward"] = []LarkMessage{
		{MessageID: "om_forward", MessageType: "merge_forward"},
		{MessageID: "om_nested", UpperMessageID: "om_forward", MessageType: "merge_forward", SenderName: "Alice"},
		{MessageID: "om_audio", UpperMessageID: "om_nested", MessageType: "audio", SenderName: "Bob", Content: `{"file_key":"audio","file_name":"voice.opus"}`},
		{MessageID: "om_video", UpperMessageID: "om_nested", MessageType: "media", SenderName: "Bob", Content: `{"file_key":"video","file_name":"clip.mp4"}`},
		{MessageID: "om_video", UpperMessageID: "om_nested", MessageType: "media", SenderName: "Bob", Content: `{"file_key":"video","file_name":"clip.mp4"}`},
	}
	r := NewFeishuIssueChatRouter(nil, nil, api, fakeCreds{secret: "plain"}, newDiscardLogger())
	r.SetMediaResolver(NewFeishuMediaResolver(api, fakeCreds{secret: "plain"}, &fakeMediaStorage{}, &fakeMediaLedger{}, newDiscardLogger()))
	got, err := r.PrepareForwardedComment(context.Background(), testMediaInstallation(t), InboundMessage{MessageID: "om_forward", MessageType: "merge_forward"}, nil)
	if err != nil || len(got.Attachments) != 2 || strings.Count(got.Content, "clip.mp4") != 1 || strings.Count(got.Content, "<details>") != 2 {
		t.Fatalf("inline tree/media: %+v, %v", got, err)
	}
	if len(api.calls) != 1 || len(api.media.downloadCalls) != 2 {
		t.Fatalf("duplicate fetch: reads=%v downloads=%v", api.calls, api.media.downloadCalls)
	}
	for _, call := range api.media.downloadCalls {
		if call.MessageID != "om_forward" {
			t.Fatalf("nested attachment must use its outer visible forward: %+v", call)
		}
	}
}

func TestForwardedCardUsesReadOnlyTextAndMediaSnapshot(t *testing.T) {
	raw := `{"header":{"title":{"tag":"plain_text","content":"Card title"}},"elements":[{"tag":"div","text":{"tag":"lark_md","content":"Card body"}},{"tag":"img","img_key":"card_image"},{"tag":"action","actions":[{"tag":"button","text":{"tag":"plain_text","content":"View"},"url":"https://example.test/conversation","value":{"command":"must not execute"}}]}]}`
	child := inboundForwardedMessage(LarkMessage{MessageID: "om_card", MessageType: "interactive", Content: raw})
	inst := testMediaInstallation(t)
	mediaID := inst.ID
	resources := mediaResourcesFromMessage(child)
	if len(resources) != 1 || resources[0].messageID != "om_card" || resources[0].key != "card_image" {
		t.Fatalf("card media: %+v", resources)
	}
	body := issueCommentMediaMarkdown(child, inst, mediaID, map[string]string{mediaObjectKey(inst, mediaID, resources[0]): "![](card-image)"})
	for _, want := range []string{"Card title", "Card body", "![](card-image)", "View", "https://example.test/conversation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "must not execute") {
		t.Fatalf("card callback escaped into snapshot: %s", body)
	}
}
