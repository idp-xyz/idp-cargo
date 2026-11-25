package mcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// cursorTTL is how long a cursor can be used after it was given.
const cursorTTL = 15 * time.Minute

// minCursorKey is the shortest HMAC key the server starts with.
const minCursorKey = 32

// cursorBody is what a cursor holds: where FIN's next page starts, how many rows the chain has delivered, what the
// chain is about (the legal entity of list_loss_jobs, the job and the state filter of list_job_cost_reviews) and when
// it expires. It is opaque to the model; the HMAC around it binds it to the tool and to the caller.
type cursorBody struct {
	Tool    string `json:"t"`
	After   string `json:"a"`
	Rows    int    `json:"n"`
	Scope   string `json:"s"`
	State   string `json:"f,omitempty"`
	Expires int64  `json:"e"`
}

// errCursor is every cursor that is not good: another caller's, another request's, expired, changed or not a cursor.
var errCursor = errors.New("cursor refused")

type cursors struct {
	key []byte
	now func() time.Time
}

var b64 = base64.RawURLEncoding

// mac signs the body for one caller: the CargoPai tenant and the subject are part of what is signed, not of the cursor.
func (c cursors) mac(a access, encoded string) []byte {
	h := hmac.New(sha256.New, c.key)
	h.Write([]byte("idp-cargo fin-mcp cursor v1\x00" + a.tenant + "\x00" + a.actor + "\x00" + encoded))
	return h.Sum(nil)
}

// seal makes the cursor for the caller.
func (c cursors) seal(a access, body cursorBody) (string, error) {
	body.Expires = c.now().Add(cursorTTL).Unix()
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	encoded := b64.EncodeToString(raw)
	return encoded + "." + b64.EncodeToString(c.mac(a, encoded)), nil
}

// open returns what the caller's cursor holds if it was made for this caller, tool and scope (and state), has not
// expired and is unchanged.
func (c cursors) open(a access, token string, want cursorBody) (cursorBody, error) {
	encoded, sig, ok := strings.Cut(token, ".")
	if !ok {
		return cursorBody{}, errCursor
	}
	given, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(given, c.mac(a, encoded)) {
		return cursorBody{}, errCursor
	}
	raw, err := b64.DecodeString(encoded)
	if err != nil {
		return cursorBody{}, errCursor
	}
	var body cursorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return cursorBody{}, errCursor
	}
	if body.Tool != want.Tool || body.Scope != want.Scope || body.State != want.State || body.After == "" || body.Rows < 0 || body.Rows >= rowCap ||
		!c.now().Before(time.Unix(body.Expires, 0)) {
		return cursorBody{}, errCursor
	}
	return body, nil
}
