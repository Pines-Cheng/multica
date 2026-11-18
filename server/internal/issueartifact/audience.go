package issueartifact

import (
	"errors"
	"net/mail"
	"strings"
)

// Audience identifies readers using identities portable across Lark apps.
// Open IDs are resolved by the publishing runtime's own Lark identity.
type Audience struct {
	Readers []Reader `json:"readers"`
}

type Reader struct {
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
}

func (a Audience) Validate() error {
	if len(a.Readers) == 0 {
		return errors.New("no document readers could be resolved")
	}
	for _, r := range a.Readers {
		switch r.MemberType {
		case "openchat":
			if !strings.HasPrefix(r.MemberID, "oc_") || len(r.MemberID) <= 3 {
				return errors.New("invalid task chat identity")
			}
		case "email":
			address, err := mail.ParseAddress(r.MemberID)
			if err != nil || address.Address != r.MemberID {
				return errors.New("document reader needs a complete email address")
			}
		default:
			return errors.New("unsupported document reader identity")
		}
	}
	return nil
}
