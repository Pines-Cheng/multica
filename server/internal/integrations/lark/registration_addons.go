package lark

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// registrationAddons are the scopes requested on top of Lark's PersonalAgent
// base template, which already grants chat creation. Issue-bound chats need
// the steward bot to read every person's message in the group (not only
// @-mentions), to add members after creation, and to pin the issue as a
// chat tab and its progress card to the top of the chat.
var registrationAddons = []string{
	"im:message.group_msg",
	"im:chat.members:write_only",
	"im:chat.tabs:write_only",
	"im:chat.top_notice:write_only",
}

// encodeRegistrationAddons encodes tenant scopes the way the upstream SDK
// encodes Options.Addons: gzip-compressed JSON, base64url without padding.
// Lark pre-fills them on the confirmation page the QR opens.
func encodeRegistrationAddons(tenantScopes []string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"scopes": map[string][]string{"tenant": tenantScopes},
	})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		return "", fmt.Errorf("lark registration: gzip addons: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("lark registration: gzip addons: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}
