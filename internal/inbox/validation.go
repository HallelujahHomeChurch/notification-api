package inbox

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var messageKey = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)

func validActor(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && id == u.String()
}
func (r Resource) valid() bool {
	if !safeID.MatchString(r.ID) {
		return false
	}
	switch r.Type {
	case "org_join_request":
		return validActor(r.ID) && validActor(r.UnitID) && r.Series == ""
	case "recording":
		return r.UnitID == "" && r.Series == ""
	case "bulletin":
		return r.UnitID == "" && (r.Series == "general" || r.Series == "children")
	}
	return false
}
func validWrite(producer string, w Write) bool {
	if !validActor(w.RecipientAccountID) || !w.Resource.valid() || w.EventVersion < 1 || w.OccurredAt.IsZero() || len(w.LogicalKey) == 0 || len(w.LogicalKey) > 200 || strings.TrimSpace(w.LogicalKey) != w.LogicalKey || strings.ContainsAny(w.LogicalKey, "\x00\r\n") {
		return false
	}
	switch producer {
	case "operations-api":
		return w.Resource.Type == "org_join_request" && (w.Type == "org_join.requested" || w.Type == "org_join.approved" || w.Type == "org_join.rejected" || w.Type == "org_join.cancelled")
	case "engagement-api":
		return w.Type == w.Resource.Type+".published" && (w.Resource.Type == "recording" || w.Resource.Type == "bulletin")
	}
	return false
}
func validProjection(p Projection, r Resource) bool {
	if !messageKey.MatchString(p.TitleKey) || !messageKey.MatchString(p.MessageKey) || len(p.Parameters) > 10 {
		return false
	}
	for k, v := range p.Parameters {
		if !messageKey.MatchString(k) || !utf8.ValidString(v) || len(v) > 500 || strings.ContainsAny(v, "\x00\r") {
			return false
		}
	}
	if p.Action == nil {
		return true
	}
	a := p.Action
	if a.Resource != r || !r.valid() {
		return false
	}
	switch a.Type {
	case "join_request_review", "join_request_result":
		if r.Type != "org_join_request" || a.Locale != "" {
			return false
		}
	case "recording":
		if r.Type != "recording" || a.Locale != "" {
			return false
		}
	case "bulletin":
		if r.Type != "bulletin" {
			return false
		}
		switch a.Locale {
		case "zh-Hant", "zh-Hans", "en", "ja", "ko":
		default:
			return false
		}
	default:
		return false
	}
	return true
}

func validLocale(locale string) bool {
	switch locale {
	case "zh-Hant", "zh-Hans", "en", "ja", "ko":
		return true
	}
	return false
}
