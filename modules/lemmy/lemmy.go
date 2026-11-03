// Package lemmy approves sign-ups on a Lemmy instance (registration mode
// "require application"), tested against 0.19.
//
// It needs the JWT of a dedicated admin account (Lemmy has no smaller role for
// this). Lemmy e-mails the applicant itself both when approved and when
// rejected, and can't be told not to, so the module declares it and aprova
// skips its own rejection e-mail. It never sends a deny_reason: the applicant
// gets Lemmy's generic text, never a moderator's note.
package lemmy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

func init() { module.Register("lemmy", New) }

const (
	responseLimit = 8 << 20
	maxPages      = 20
	pageSize      = 50
	pathBase      = "/api/v3/admin/registration_application"
)

// The request id is the applicant's person id, so one call finds it.
var idPattern = regexp.MustCompile(`^[0-9]{1,18}$`)

type lemmy struct {
	base   string
	token  string
	client *http.Client
}

// New builds the module for one instance. Only auth: token is possible.
func New(c module.Config) (module.Module, error) {
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://127.0.0.1") {
		return nil, errors.New("lemmy: url must be https")
	}
	if c.Auth != "token" || c.Token == "" {
		return nil, errors.New("lemmy: needs auth: token and the JWT of an admin account")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &lemmy{base: strings.TrimRight(c.URL, "/"), token: c.Token, client: client}, nil
}

func (l *lemmy) Caps() module.Caps {
	// Lemmy 0.19 e-mails the applicant on approve and on reject.
	return module.Caps{SilentReject: false, SendsWelcome: true}
}

type application struct {
	RegistrationApplication struct {
		ID        int64   `json:"id"`
		Answer    string  `json:"answer"`
		AdminID   *int64  `json:"admin_id"`
		Published lemmyTS `json:"published"`
	} `json:"registration_application"`
	CreatorLocalUser struct {
		Email         *string `json:"email"`
		EmailVerified *bool   `json:"email_verified"`
	} `json:"creator_local_user"`
	Creator struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"creator"`
}

func (a application) pending() bool { return a.RegistrationApplication.AdminID == nil }

func (a application) request() module.Request {
	r := module.Request{
		ID:       strconv.FormatInt(a.Creator.ID, 10),
		Username: a.Creator.Name,
		Reason:   a.RegistrationApplication.Answer,
		Created:  time.Time(a.RegistrationApplication.Published),
	}
	if a.CreatorLocalUser.Email != nil {
		r.Email = *a.CreatorLocalUser.Email
	}
	if v := a.CreatorLocalUser.EmailVerified; v != nil {
		r.Checks = append(r.Checks, module.Check{Name: "email_verified", OK: *v})
	}
	return r
}

// lemmyTS reads Lemmy's timestamps, with or without a zone or fraction.
type lemmyTS time.Time

func (t *lemmyTS) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999-07"} {
		if v, err := time.Parse(layout, s); err == nil {
			*t = lemmyTS(v)
			return nil
		}
	}
	return nil
}

type apiError struct {
	Error string `json:"error"`
}

// call does one request. Lemmy answers most refusals with 400 and a code.
func (l *lemmy) call(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	r := io.LimitReader(resp.Body, responseLimit)
	if resp.StatusCode/100 != 2 {
		var e apiError
		json.NewDecoder(r).Decode(&e)
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden,
			e.Error == "not_an_admin" || e.Error == "incorrect_login" || e.Error == "not_logged_in":
			return module.ErrUnauthorized
		case resp.StatusCode == http.StatusNotFound, e.Error == "couldnt_find_registration_application":
			return module.ErrGone
		case e.Error != "":
			return fmt.Errorf("lemmy: status %d: %s", resp.StatusCode, e.Error)
		default:
			return fmt.Errorf("lemmy: status %d", resp.StatusCode)
		}
	}
	if out != nil {
		return json.NewDecoder(r).Decode(out)
	}
	io.Copy(io.Discard, r)
	return nil
}

func (l *lemmy) Pending(ctx context.Context, _ module.Actor) ([]module.Request, error) {
	var out []module.Request
	for page := 1; page <= maxPages; page++ {
		var list struct {
			RegistrationApplications []application `json:"registration_applications"`
		}
		q := url.Values{"unread_only": {"true"}, "limit": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}
		if err := l.call(ctx, http.MethodGet, pathBase+"/list?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, a := range list.RegistrationApplications {
			if a.pending() {
				out = append(out, a.request())
			}
		}
		if len(list.RegistrationApplications) < pageSize {
			break
		}
	}
	return out, nil
}

// get reads the application of one person.
func (l *lemmy) get(ctx context.Context, id string) (application, error) {
	var resp struct {
		RegistrationApplication application `json:"registration_application"`
	}
	err := l.call(ctx, http.MethodGet, pathBase+"?"+url.Values{"person_id": {id}}.Encode(), nil, &resp)
	return resp.RegistrationApplication, err
}

func (l *lemmy) Get(ctx context.Context, _ module.Actor, id string) (module.Request, bool, error) {
	if !idPattern.MatchString(id) {
		return module.Request{}, false, nil
	}
	a, err := l.get(ctx, id)
	if errors.Is(err, module.ErrGone) {
		return module.Request{}, false, nil
	} else if err != nil {
		return module.Request{}, false, err
	}
	return a.request(), a.pending(), nil
}

func (l *lemmy) decide(ctx context.Context, id string, approve bool) error {
	if !idPattern.MatchString(id) {
		return module.ErrGone
	}
	a, err := l.get(ctx, id)
	if err != nil {
		return err
	}
	if !a.pending() {
		return module.ErrGone
	}
	// No deny_reason, ever: the applicant gets Lemmy's generic text.
	body := struct {
		ID      int64 `json:"id"`
		Approve bool  `json:"approve"`
	}{a.RegistrationApplication.ID, approve}
	err = l.call(ctx, http.MethodPut, pathBase+"/approve", body, nil)
	if err == nil || errors.Is(err, module.ErrUnauthorized) {
		return err
	}
	// Lemmy commits first and e-mails after; a failed e-mail still returns an
	// error. If the decision is in, it went through.
	if again, gerr := l.get(ctx, id); gerr == nil && !again.pending() {
		return nil
	}
	return err
}

func (l *lemmy) Approve(ctx context.Context, _ module.Actor, id string) error {
	return l.decide(ctx, id, true)
}

func (l *lemmy) Reject(ctx context.Context, _ module.Actor, id string) error {
	return l.decide(ctx, id, false)
}
