package lark

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPClient_IssueReceiptDeduplication(t *testing.T) {
	for _, target := range []ReplyTarget{{}, {MessageID: "om_original", InThread: true}} {
		t.Run(target.MessageID, func(t *testing.T) {
			fake := newLarkFake(t)
			fake.stubToken("receipt_token", 7200)
			path := "/open-apis/im/v1/messages"
			if target.IsSet() {
				path += "/om_original/reply"
			}
			fake.mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["uuid"] != "stable-request-id" {
					t.Errorf("receipt dedup key missing: %+v", body)
				}
				if target.IsSet() && body["reply_in_thread"] != true {
					t.Errorf("receipt left its topic: %+v", body)
				}
				writeJSON(w, map[string]any{"code": 0, "data": map[string]string{"message_id": "om_receipt"}})
			})
			_, err := newTestClient(fake, time.Now).SendInteractiveCard(context.Background(), SendCardParams{
				InstallationID: testCreds(), ChatID: "oc_issue", ReplyTarget: target,
				DeduplicationID: "stable-request-id", CardJSON: `{"schema":"2.0"}`,
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLarkOutcomeReplier_MembershipDenialRespondsOnlyToRequests(t *testing.T) {
	api := &stubAPIClientWithRecorder{configured: true}
	replier := NewLarkOutcomeReplier(OutcomeReplierConfig{
		APIClient: api, BindingSvc: &BindingTokenService{}, Credentials: stubCredentialsResolver{secret: "s"},
		Queries: stubReplierQueries{}, AppURL: "https://multica.test", Logger: newDiscardLogger(),
	})
	msg := InboundMessage{ChatID: "oc_chat", ChatType: ChatTypeGroup, MessageID: "om_request"}
	result := DispatchResult{Outcome: OutcomeDropped, DropReason: DropReasonNonWorkspaceMember}
	inst := Installation{AppID: "cli_test"}
	replier.Reply(context.Background(), inst, msg, result)
	if len(api.interactiveOut) != 0 {
		t.Fatal("ordinary conversation received a denial card")
	}
	msg.AddressedToBot = true
	replier.Reply(context.Background(), inst, msg, result)
	if len(api.interactiveOut) != 1 || !strings.Contains(unescapeCardJSON(t, api.interactiveOut[0].CardJSON), "访问权限") {
		t.Fatalf("explicit request was silently dropped: %+v", api.interactiveOut)
	}
}
