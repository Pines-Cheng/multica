package issueartifact

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Artifact describes a published deliverable. Kind is an open classification;
// presentation and publishing must not depend on a catalog of known kinds.
type Artifact struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Kind  string `json:"kind,omitempty"`
}

type Record struct {
	Key string `json:"key"`
	Artifact
}

var keyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)

func ValidateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return errors.New("artifact key must match ^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$")
	}
	return nil
}

func ValidURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && !strings.ContainsAny(raw, "\r\n\t ")
}

func (a *Artifact) Validate() error {
	a.Title = strings.TrimSpace(a.Title)
	a.Kind = strings.TrimSpace(a.Kind)
	if a.Title == "" || utf8.RuneCountInString(a.Title) > 120 || strings.ContainsAny(a.Title, "\r\n\t") {
		return errors.New("artifact title must be a single line of 1–120 characters")
	}
	if len(a.URL) > 2048 || !ValidURL(a.URL) {
		return errors.New("artifact url must be an HTTP(S) URL of at most 2048 bytes")
	}
	if utf8.RuneCountInString(a.Kind) > 64 {
		return errors.New("artifact kind must be at most 64 characters")
	}
	return nil
}

// Records returns a stable order without interpreting kind or internal keys.
func Records(raw []byte) []Record {
	var items map[string]Artifact
	if json.Unmarshal(raw, &items) != nil {
		return []Record{}
	}
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]Record, 0, len(items))
	for _, key := range keys {
		a := items[key]
		if ValidateKey(key) == nil && a.Validate() == nil {
			result = append(result, Record{Key: key, Artifact: a})
		}
	}
	return result
}
