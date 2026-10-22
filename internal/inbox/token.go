package inbox

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"time"
)

type token struct {
	Locale     string    `json:"locale,omitempty"`
	Actor      string    `json:"actor"`
	Purpose    string    `json:"purpose"`
	Filter     string    `json:"filter,omitempty"`
	Sequence   int64     `json:"sequence"`
	IssuedAt   int64     `json:"iat"`
	ExpiresAt  int64     `json:"exp"`
	BeforeTime time.Time `json:"beforeTime,omitempty"`
	BeforeID   string    `json:"beforeId,omitempty"`
}
type tokens struct {
	active string
	keys   map[string][]byte
}

func newTokens(active string, keys map[string][]byte) tokens { return tokens{active, keys} }
func (t tokens) mac(key []byte, payload string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("notification-inbox-tokens-v1\x00" + payload))
	return h.Sum(nil)
}
func (t tokens) sign(v token) string {
	raw, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	id := base64.RawURLEncoding.EncodeToString([]byte(t.active))
	msg := id + "." + payload
	return msg + "." + base64.RawURLEncoding.EncodeToString(t.mac(t.keys[t.active], msg))
}
func (t tokens) verify(raw, actor, purpose string, now time.Time) (token, error) {
	fail := func() (token, error) { return token{}, ErrInvalid }
	if len(raw) > 4096 {
		return fail()
	}
	p := strings.Split(raw, ".")
	if len(p) != 3 {
		return fail()
	}
	id, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return fail()
	}
	key := t.keys[string(id)]
	sig, err := base64.RawURLEncoding.DecodeString(p[2])
	if err != nil || len(key) < 32 || !hmac.Equal(sig, t.mac(key, p[0]+"."+p[1])) {
		return fail()
	}
	body, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil {
		return fail()
	}
	var v token
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(&struct{}{}) != io.EOF {
		return fail()
	}
	if v.Actor != actor || !validActor(actor) || v.Purpose != purpose || v.Sequence < 0 || v.IssuedAt > now.Unix() || v.ExpiresAt <= now.Unix() || v.ExpiresAt-v.IssuedAt != 900 {
		return fail()
	}
	switch purpose {
	case "read":
		if v.Locale != "" || v.Filter != "" || v.BeforeID != "" || !v.BeforeTime.IsZero() {
			return fail()
		}
	case "cursor":
		if !validLocale(v.Locale) || (v.Filter != "all" && v.Filter != "unread") || !validActor(v.BeforeID) || v.BeforeTime.IsZero() {
			return fail()
		}
	default:
		return fail()
	}
	return v, nil
}
