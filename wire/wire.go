// Package wire is the contract between aprova's http module and an app's agent
// (aprova-agent, or anything that speaks it). Every request is signed with a
// secret both sides share:
//
//	X-Aprova-Timestamp: unix seconds
//	X-Aprova-Signature: hex(HMAC-SHA256(secret, method "\n" path "\n" timestamp "\n" hex(SHA256(body))))
//
// The agent refuses a request more than MaxSkew away from its clock. Endpoints:
//
//	GET  /v1/caps                    -> Caps
//	GET  /v1/pending                 -> Pending
//	GET  /v1/requests/{id}           -> One (404: unknown)
//	POST /v1/requests/{id}/approve   -> 204 (409: no longer pending)
//	POST /v1/requests/{id}/reject    -> 204 (409: no longer pending); never messages the applicant
//
// Errors are {"error": "..."} with a 4xx or 5xx status.
package wire

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

const (
	HeaderTimestamp = "X-Aprova-Timestamp"
	HeaderSignature = "X-Aprova-Signature"
	MaxSkew         = time.Minute
	// MinSecret is the shortest shared secret either side accepts.
	MinSecret = 32
)

// ID is what a request id may look like on the wire: it ends up in paths and
// in arguments to the app's own tools.
var ID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Request struct {
	ID       string    `json:"id"`
	Username string    `json:"username"`
	Email    string    `json:"email"`
	Reason   string    `json:"reason"`
	Created  time.Time `json:"created"`
}

type Caps struct {
	// SilentReject: rejecting sends nothing to the applicant.
	SilentReject bool `json:"silent_reject"`
	// SendsWelcome: approving makes the app send its own welcome.
	SendsWelcome bool `json:"sends_welcome"`
}

type Pending struct {
	Requests []Request `json:"requests"`
}

type One struct {
	Request Request `json:"request"`
	Pending bool    `json:"pending"`
}

type Error struct {
	Error string `json:"error"`
}

// Signature computes the signature of one request.
func Signature(secret []byte, method, path, timestamp string, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(method + "\n" + path + "\n" + timestamp + "\n" + hex.EncodeToString(sum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

// Sign adds the headers to req, whose body is body.
func Sign(req *http.Request, secret []byte, body []byte, now time.Time) {
	ts := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, Signature(secret, req.Method, req.URL.EscapedPath(), ts, body))
}

var (
	ErrUnsigned = errors.New("missing signature")
	ErrSkew     = errors.New("timestamp too far from the agent's clock")
	ErrBadSig   = errors.New("bad signature")
)

// Verify checks a received request.
func Verify(r *http.Request, secret []byte, body []byte, now time.Time) error {
	ts, sig := r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderSignature)
	if ts == "" || sig == "" {
		return ErrUnsigned
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrSkew
	}
	if d := now.Sub(time.Unix(sec, 0)); d > MaxSkew || d < -MaxSkew {
		return ErrSkew
	}
	want := Signature(secret, r.Method, r.URL.EscapedPath(), ts, body)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return ErrBadSig
	}
	return nil
}
