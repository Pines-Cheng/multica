package issueartifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestShareGeneratedDocumentVerifiesReadersAndPreservesEditors(t *testing.T) {
	audience := Audience{Readers: []Reader{{"email", "alice@example.com"}, {"email", "bob@example.com"}, {"email", "alice@example.com"}}}
	grants := map[string]string{"ou_alice": "edit"}
	adds := 0
	run := func(_ context.Context, args ...string) ([]byte, error) {
		var data any
		switch args[1] {
		case "+inspect":
			data = map[string]any{"token": "document", "type": "docx"}
		case "+search-user":
			email := flagValue(args, "--query")
			data = map[string]any{"users": []any{map[string]string{"open_id": "ou_" + strings.Split(email, "@")[0], "enterprise_email": email}}}
		case "GET":
			items := []any{}
			for id, perm := range grants {
				items = append(items, map[string]string{"member_type": "openid", "member_id": id, "perm": perm})
			}
			data = map[string]any{"items": items}
		case "+member-add":
			id := flagValue(args, "--member-id")
			if id != "ou_bob" || flagValue(args, "--perm") != "view" || flagValue(args, "--as") != "user" {
				t.Fatalf("unexpected grant: %v", args)
			}
			grants[id] = "view"
			adds++
			data = map[string]any{"member_id": id, "perm": "view"}
		default:
			t.Fatalf("unexpected call: %v", args)
		}
		return json.Marshal(map[string]any{"ok": true, "data": data})
	}
	for range 2 {
		if err := ShareGeneratedDocument(context.Background(), run, "https://example.feishu.cn/wiki/source", audience); err != nil {
			t.Fatal(err)
		}
	}
	if adds != 1 || grants["ou_alice"] != "edit" {
		t.Fatalf("repeated or downgraded grants: %d, %v", adds, grants)
	}
}

func TestShareGeneratedDocumentDoesNotPublishUnverifiedAccess(t *testing.T) {
	for _, failure := range []string{"denied", "partial", "missing grant", "malformed list", "missing result", "malformed envelope", "exit failure"} {
		t.Run(failure, func(t *testing.T) {
			lists := 0
			run := func(_ context.Context, args ...string) ([]byte, error) {
				if args[1] == "+inspect" {
					return []byte(`{"ok":true,"data":{"token":"doc","type":"docx"}}`), nil
				}
				if args[1] == "GET" {
					lists++
					if failure == "malformed list" {
						return []byte(`{"ok":true,"data":{}}`), nil
					}
					return []byte(`{"ok":true,"data":{"items":[]}}`), nil
				}
				if args[1] != "+member-add" {
					t.Fatalf("unexpected call %v", args)
				}
				switch failure {
				case "denied":
					return []byte(`{"ok":false,"error":{"message":"permission denied"}}`), nil
				case "partial":
					return []byte(`{"ok":true,"data":{"partial":true}}`), nil
				case "missing result":
					return []byte(`{"ok":true}`), nil
				case "malformed envelope":
					return []byte(`not-json`), nil
				case "exit failure":
					return nil, errors.New("missing lark-cli")
				default:
					return []byte(`{"ok":true,"data":{}}`), nil
				}
			}
			err := ShareGeneratedDocument(context.Background(), run, "https://example.larkoffice.com/docx/doc", Audience{Readers: []Reader{{"openchat", "oc_task"}}})
			if err == nil {
				t.Fatal("unverified reader reported as success")
			}
			if failure == "missing grant" && lists != 2 {
				t.Fatal("grant was not read back")
			}
		})
	}
}

func TestShareGeneratedDocumentRejectsAmbiguousPeopleBeforeGrants(t *testing.T) {
	for _, users := range []string{`[]`, `[{"open_id":"ou_a","email":"other@example.com"}]`, `[{"open_id":"ou_a","email":"a@example.com"},{"open_id":"ou_b","email":"a@example.com"}]`} {
		run := func(_ context.Context, args ...string) ([]byte, error) {
			if args[1] == "+inspect" {
				return []byte(`{"ok":true,"data":{"token":"doc","type":"docx"}}`), nil
			}
			if args[1] == "+search-user" {
				return []byte(fmt.Sprintf(`{"ok":true,"data":{"users":%s}}`, users)), nil
			}
			t.Fatalf("changed permissions before resolving readers: %v", args)
			return nil, nil
		}
		if err := ShareGeneratedDocument(context.Background(), run, "https://example.feishu.cn/docx/doc", Audience{Readers: []Reader{{"email", "a@example.com"}}}); err == nil {
			t.Fatal("accepted unresolved reader")
		}
	}
}

func TestArtifactAudienceRejectsMalformedResponses(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"readers":null}`, `{"readers":[]}`, `{"readers":[{}]}`, `{"readers":[{"member_type":"future","member_id":"x"}]}`, `{"readers":[{"member_type":"email","member_id":"Alice <a@example.com>"}]}`} {
		var a Audience
		if err := json.Unmarshal([]byte(raw), &a); err == nil && a.Validate() == nil {
			t.Fatalf("accepted malformed audience: %s", raw)
		}
	}
}

func TestLarkDocumentURLBoundary(t *testing.T) {
	for _, link := range []string{"https://x.feishu.cn/docx/abc", "https://x.larkoffice.com/wiki/abc", "https://x.larksuite.com/sheets/abc", "https://doubao.com/docx/abc"} {
		if !IsLarkDocumentURL(link) {
			t.Errorf("rejected %s", link)
		}
	}
	for _, link := range []string{"https://evil.test/docx/abc", "https://feishu.cn.evil.test/docx/abc", "https://x.feishu.cn/drive/folder/abc", "http://x.feishu.cn/docx/abc", "https://user@x.feishu.cn/docx/abc"} {
		if IsLarkDocumentURL(link) {
			t.Errorf("accepted %s", link)
		}
	}
}

func flagValue(args []string, flag string) string {
	for i := range args {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
