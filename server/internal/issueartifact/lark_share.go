package issueartifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// LarkCommand is injected so tests never use an installed CLI or account.
type LarkCommand func(context.Context, ...string) ([]byte, error)

// ShareGeneratedDocument runs as the document creator, before publishing its
// link. It only adds missing readers; existing edit/manage grants survive.
func ShareGeneratedDocument(ctx context.Context, run LarkCommand, link string, audience Audience) error {
	if err := audience.Validate(); err != nil {
		return err
	}
	if !IsLarkDocumentURL(link) {
		return errors.New("--generated requires a Feishu/Lark document URL")
	}
	call := func(out any, args ...string) error {
		raw, err := run(ctx, append(args, "--as", "user", "--format", "json")...)
		var envelope struct {
			OK    bool            `json:"ok"`
			Data  json.RawMessage `json:"data"`
			Error struct {
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		}
		if decodeErr := json.Unmarshal(raw, &envelope); decodeErr != nil {
			if err != nil {
				return fmt.Errorf("lark-cli: %w", err)
			}
			return errors.New("lark-cli returned malformed JSON")
		}
		if !envelope.OK || err != nil {
			return fmt.Errorf("lark-cli failed: %s %s (exit: %v)", envelope.Error.Message, envelope.Error.Hint, err)
		}
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return errors.New("lark-cli returned no result")
		}
		return json.Unmarshal(envelope.Data, out)
	}
	var doc struct {
		Token string `json:"token"`
		Type  string `json:"type"`
	}
	if err := call(&doc, "drive", "+inspect", "--url", link); err != nil {
		return err
	}
	switch doc.Type {
	case "doc", "docx", "sheet", "bitable", "file", "mindnote", "slides":
	default:
		return errors.New("unsupported generated document type")
	}
	if doc.Token == "" {
		return errors.New("lark-cli did not resolve the document token")
	}
	readers := map[Reader]bool{}
	// Resolve all people before changing any permissions. An exact full email
	// match is required; never guess from a display name or an email prefix.
	for _, reader := range audience.Readers {
		if reader.MemberType == "email" {
			var result struct {
				HasMore bool `json:"has_more"`
				Users   []struct {
					OpenID          string `json:"open_id"`
					Email           string `json:"email"`
					EnterpriseEmail string `json:"enterprise_email"`
				} `json:"users"`
			}
			if err := call(&result, "contact", "+search-user", "--query", reader.MemberID); err != nil {
				return err
			}
			matches := map[string]bool{}
			for _, u := range result.Users {
				if strings.EqualFold(u.Email, reader.MemberID) || strings.EqualFold(u.EnterpriseEmail, reader.MemberID) {
					if strings.HasPrefix(u.OpenID, "ou_") {
						matches[u.OpenID] = true
					}
				}
			}
			if result.HasMore || len(matches) != 1 {
				return fmt.Errorf("cannot uniquely resolve document reader %s", reader.MemberID)
			}
			for id := range matches {
				reader = Reader{MemberType: "openid", MemberID: id}
			}
		}
		readers[reader] = true
	}
	type member struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
		Perm       string `json:"perm"`
	}
	params, _ := json.Marshal(map[string]string{"type": doc.Type})
	list := func() (map[Reader]bool, error) {
		var result struct {
			Items []member `json:"items"`
		}
		if err := call(&result, "api", "GET", "/open-apis/drive/v1/permissions/"+url.PathEscape(doc.Token)+"/members", "--params", string(params)); err != nil {
			return nil, err
		}
		if result.Items == nil {
			return nil, errors.New("lark-cli returned no collaborator list")
		}
		granted := map[Reader]bool{}
		for _, m := range result.Items {
			if m.Perm == "view" || m.Perm == "edit" || m.Perm == "full_access" {
				granted[Reader{MemberType: m.MemberType, MemberID: m.MemberID}] = true
			}
		}
		return granted, nil
	}
	granted, err := list()
	if err != nil {
		return err
	}
	changed := false
	for reader := range readers {
		if granted[reader] {
			continue
		}
		var result struct {
			MemberID   string `json:"member_id"`
			MemberType string `json:"member_type"`
			Perm       string `json:"perm"`
			Partial    bool   `json:"partial"`
		}
		if err := call(&result, "drive", "+member-add", "--token", doc.Token, "--type", doc.Type, "--member-id", reader.MemberID, "--member-type", reader.MemberType, "--perm", "view", "--need-notification=false", "--yes"); err != nil {
			return err
		}
		if result.Partial {
			return errors.New("document reader grant was only partially applied")
		}
		changed = true
	}
	if changed {
		granted, err = list()
		if err != nil {
			return err
		}
	}
	for reader := range readers {
		if !granted[reader] {
			return fmt.Errorf("document access verification failed for %s", reader.MemberID)
		}
	}
	return nil
}

func IsLarkDocumentURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	trusted := false
	for _, domain := range []string{"feishu.cn", "larksuite.com", "larkoffice.com", "doubao.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			trusted = true
		}
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if !trusted || len(parts) != 2 || parts[1] == "" {
		return false
	}
	switch parts[0] {
	case "doc", "docx", "sheets", "base", "bitable", "file", "mindnotes", "slides", "wiki":
		return true
	default:
		return false
	}
}
