package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

const maxText = 4000

type reviewView struct {
	Acct    string `json:"acct"`
	Role    string `json:"role"`
	Verdict string `json:"verdict"`
	Counts  bool   `json:"counts"`
}

type requestView struct {
	Service       string         `json:"service"`
	ServiceName   string         `json:"service_name"`
	ID            string         `json:"id"`
	Pending       bool           `json:"pending"`
	Username      string         `json:"username"`
	Email         string         `json:"email"`
	Reason        string         `json:"reason"`
	Created       string         `json:"created"`
	Round         int            `json:"round"`
	Escalated     bool           `json:"escalated"`
	Awaiting      string         `json:"awaiting"`
	Undeliverable bool           `json:"undeliverable"` // the question e-mail bounced
	Decision      string         `json:"decision"`
	DecidedBy     string         `json:"decided_by"`
	Checks        []module.Check `json:"checks"`
	Reviews       []reviewView   `json:"reviews"`
	Tally         tally          `json:"tally"`
	Comments      []comment      `json:"comments"`
	MyVote        string         `json:"my_vote"`
	CanReview     bool           `json:"can_review"`
	CanDecide     bool           `json:"can_decide"`
	CanDeescalate bool           `json:"can_deescalate"`
	WouldApprove  bool           `json:"would_approve"` // my approval completes the quorum
	WouldReject   bool           `json:"would_reject"`  // my rejection completes the reject quorum
	SilentReject  bool           `json:"silent_reject"`
	SendsWelcome  bool           `json:"sends_welcome"`
	NoReason      bool           `json:"no_reason"` // the app's form asks for no reason
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request, sess session) (*service, bool) {
	svc, ok := s.services[r.PathValue("service")]
	if !ok || !svc.profile.canView(sess.Role) {
		s.renderError(w, r, http.StatusNotFound, "not_found")
		return nil, false
	}
	return svc, true
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request, sess session) {
	svc, ok := s.lookup(w, r, sess)
	if !ok {
		return
	}
	v := s.view(r, sess, "queue")
	rv, err := s.buildRequest(r.Context(), sess, svc, r.PathValue("id"), v.T["date_format"])
	switch {
	case errors.Is(err, errNotFound):
		s.renderError(w, r, http.StatusNotFound, "not_found")
		return
	case err != nil:
		s.moduleFailed(w, r, svc, err)
		return
	}
	v.Req = rv
	if k := r.URL.Query().Get("error"); k != "" {
		if msg, ok := v.T["form_"+k]; ok {
			v.Error = msg
		}
	}
	s.render(w, "request.html", http.StatusOK, v)
}

var errNotFound = errors.New("not found")

// buildRequest is everything known about one request, for the page and the API.
func (s *Server) buildRequest(ctx context.Context, sess session, svc *service, id, dateFormat string) (requestView, error) {
	q, pending, err := svc.mod.Get(ctx, actorFor(sess, svc), id)
	if err != nil {
		return requestView{}, err
	}
	st, err := s.db.state(ctx, svc.id, id)
	if err != nil {
		return requestView{}, err
	}
	if !pending && st.Decision == "" && st.Username == "" {
		return requestView{}, errNotFound
	}
	caps := svc.mod.Caps()
	rv := requestView{Service: svc.id, ServiceName: svc.cfg.Name, ID: id, Pending: pending && st.Decision == "",
		Username: st.Username, Round: st.Round, Escalated: st.Escalated, Undeliverable: st.Undeliverable,
		Decision: st.Decision, DecidedBy: st.DecidedBy, SilentReject: caps.SilentReject, SendsWelcome: caps.SendsWelcome, NoReason: caps.NoReason}
	// The app's data about an account is shown only while it is a pending request:
	// the module's credentials can read any account, not just applicants.
	if rv.Pending {
		rv.Username, rv.Email, rv.Reason = q.Username, q.Email, q.Reason
		if !q.Created.IsZero() {
			rv.Created = q.Created.In(s.cfg.loc).Format(dateFormat)
		}
	}
	if st.AwaitingSince != nil {
		rv.Awaiting = st.AwaitingSince.In(s.cfg.loc).Format(dateFormat)
	}
	if rv.Pending {
		rv.Checks = s.checks.run(ctx, q, caps.NoReason)
	}
	reviews, err := reviewsOf(ctx, s.db.pool, svc.id, id, st.Round)
	if err == nil {
		rv.Comments, err = s.db.comments(ctx, svc.id, id)
	}
	if err != nil {
		return requestView{}, err
	}
	canWrite := svc.profile.canReview(sess.Role) || svc.profile.canBulkReject(sess.Role)
	for i := range rv.Comments {
		c := &rv.Comments[i]
		c.Mine = canWrite && c.AuthorID == sess.AccountID && slices.Contains(editableKinds, c.Kind)
	}
	p := svc.profile
	rv.Tally = p.count(st.Escalated, reviews, rv.Checks)
	for _, rr := range reviews {
		counts := rr.Verdict == verdictApprove && p.canReview(rr.ReviewerRole) && (!st.Escalated || hasRole(rr.ReviewerRole, p.EscalateTo))
		rv.Reviews = append(rv.Reviews, reviewView{Acct: rr.ReviewerAcct, Role: rr.ReviewerRole, Verdict: rr.Verdict, Counts: counts})
		if rr.ReviewerID == sess.AccountID {
			rv.MyVote = rr.Verdict
		}
	}
	rv.CanReview = rv.Pending && p.canReview(sess.Role)
	rv.CanDecide = rv.Pending && p.canDecide(sess.Role, st.Escalated)
	rv.CanDeescalate = rv.Pending && st.Escalated && p.canDeescalate(sess.Role)
	mine := func(verdict string) tally {
		v := append(slices.Clone(reviews), review{ReviewerID: sess.AccountID, ReviewerRole: sess.Role, Verdict: verdict})
		return p.count(st.Escalated, latest(v), rv.Checks)
	}
	eligible := !st.Escalated || hasRole(sess.Role, p.EscalateTo)
	if rv.CanReview && rv.MyVote != verdictApprove {
		rv.WouldApprove = mine(verdictApprove).Ready() && eligible
	}
	if rv.CanReview && rv.MyVote != verdictReject {
		rv.WouldReject = mine(verdictReject).ReadyReject() && eligible
	}
	return rv, nil
}

// latest keeps the last vote of each reviewer.
func latest(rs []review) []review {
	last := map[string]int{}
	for i, r := range rs {
		last[r.ReviewerID] = i
	}
	var out []review
	for i, r := range rs {
		if last[r.ReviewerID] == i {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) moduleFailed(w http.ResponseWriter, r *http.Request, svc *service, err error) {
	if errors.Is(err, module.ErrUnauthorized) {
		s.renderError(w, r, http.StatusForbidden, "unauthorized")
		return
	}
	log.Printf("%s: %v", svc.id, err)
	s.renderError(w, r, http.StatusBadGateway, "instance")
}

func text(r *http.Request, field string) (string, bool) {
	v := strings.TrimSpace(r.PostFormValue(field))
	return v, utf8.RuneCountInString(v) <= maxText && utf8.ValidString(v)
}

var errFailed = errors.New("action failed")

// outcome is what an action reports back to the page.
type outcome struct {
	done      string // flash key
	formError string // shown on the request page
	notify    string // Mattermost text, sent after the commit
	mailTo    string // send the rejection e-mail after the commit
}

func (s *Server) handleAct(kind string) authed {
	return func(w http.ResponseWriter, r *http.Request, sess session) {
		svc, ok := s.lookup(w, r, sess)
		if !ok {
			return
		}
		id := r.PathValue("id")
		back := "/r/" + url.PathEscape(svc.id) + "/" + url.PathEscape(id)
		body, ok := text(r, "body")
		if !ok {
			http.Redirect(w, r, back+"?error=too_long", http.StatusSeeOther)
			return
		}
		out, err := s.act(r.Context(), sess, svc, id, kind, body, r.PostFormValue("verdict"))
		switch {
		case errors.Is(err, errForbidden):
			s.renderError(w, r, http.StatusForbidden, "forbidden")
		case errors.Is(err, module.ErrUnauthorized):
			s.renderError(w, r, http.StatusForbidden, "unauthorized")
		case err != nil:
			log.Printf("%s %s: %v", kind, svc.id, err)
			s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
		case out.formError != "":
			// Back to the form, where the message shows next to the field.
			http.Redirect(w, r, back+"?error="+url.QueryEscape(out.formError)+"#review", http.StatusSeeOther)
		case out.done == "approved" || out.done == "rejected" || out.done == "mail_failed" || out.done == "no_mail":
			http.Redirect(w, r, "/queue?done="+out.done, http.StatusSeeOther)
		default:
			http.Redirect(w, r, back+"?done="+out.done, http.StatusSeeOther)
		}
	}
}

// act is one moderator action, shared by the web pages and the API.
func (s *Server) act(ctx context.Context, sess session, svc *service, id, kind, body, verdict string) (outcome, error) {
	if !svc.profile.canView(sess.Role) {
		return outcome{}, errForbidden
	}
	if !svc.profile.canReview(sess.Role) && !svc.profile.canBulkReject(sess.Role) {
		return outcome{}, errForbidden
	}
	q, pending, err := svc.mod.Get(ctx, actorFor(sess, svc), id)
	if err != nil {
		return outcome{}, err
	}
	if !pending {
		// Only a request aprova already knows is marked; any other id stays unknown.
		s.markGone(ctx, svc, id)
		return outcome{done: "gone"}, nil
	}
	checks := s.checks.run(ctx, q, svc.mod.Caps().NoReason)
	var out outcome
	err = s.db.withRequest(ctx, svc.id, id, q.Username, func(tx pgx.Tx, st reqState) error {
		if st.Decision != "" {
			out.done = "already"
			return nil
		}
		var err error
		out, err = s.apply(ctx, tx, kind, sess, svc, st, q, checks, body, verdict)
		return err
	})
	if errors.Is(err, errFailed) {
		err = nil
	}
	if err != nil {
		return outcome{}, err
	}
	s.after(ctx, sess, svc, q, &out)
	return out, nil
}

var errForbidden = errors.New("forbidden")

// editComment fixes the text of the moderator's own comment. Someone who lost
// the role can't, and the old text goes to the audit log.
func (s *Server) editComment(ctx context.Context, sess session, svc *service, id, commentID, body string) (string, error) {
	if !svc.profile.canView(sess.Role) || !(svc.profile.canReview(sess.Role) || svc.profile.canBulkReject(sess.Role)) {
		return "", errForbidden
	}
	cid, err := strconv.ParseInt(commentID, 10, 64)
	if err != nil || cid <= 0 {
		return "", errNotYours
	}
	if body == "" {
		return "empty", nil
	}
	e := auditEntry{ActorID: sess.AccountID, ActorAcct: sess.Acct, Profile: svc.profile.id, Service: svc.id, RequestID: id}
	return "", s.db.editComment(ctx, e, cid, body)
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request, sess session) {
	svc, ok := s.lookup(w, r, sess)
	if !ok {
		return
	}
	id := r.PathValue("id")
	back := "/r/" + url.PathEscape(svc.id) + "/" + url.PathEscape(id)
	body, ok := text(r, "body")
	if !ok {
		http.Redirect(w, r, back+"?error=too_long#discussion", http.StatusSeeOther)
		return
	}
	formErr, err := s.editComment(r.Context(), sess, svc, id, r.PostFormValue("comment"), body)
	switch {
	case errors.Is(err, errForbidden), errors.Is(err, errNotYours):
		s.renderError(w, r, http.StatusForbidden, "forbidden")
	case err != nil:
		log.Printf("edit %s: %v", svc.id, err)
		s.renderError(w, r, http.StatusServiceUnavailable, "unavailable")
	case formErr != "":
		http.Redirect(w, r, back+"?error="+formErr+"#discussion", http.StatusSeeOther)
	default:
		http.Redirect(w, r, back+"?done=edited#discussion", http.StatusSeeOther)
	}
}

// apply does one action with the request locked. It returns errFailed with a
// formError for a refusal the moderator can fix, and rolls everything back.
func (s *Server) apply(ctx context.Context, tx pgx.Tx, kind string, sess session, svc *service, st reqState, q module.Request,
	checks []module.Check, body, verdict string) (outcome, error) {
	p := svc.profile
	fail := func(key string) (outcome, error) { return outcome{formError: key}, errFailed }
	entry := func(action, detail, result string) auditEntry {
		return auditEntry{ActorID: sess.AccountID, ActorAcct: sess.Acct, Profile: p.id, Service: svc.id, RequestID: st.RequestID,
			Username: q.Username, Action: action, Detail: detail, Result: result}
	}
	switch kind {
	case "comment":
		if !p.canReview(sess.Role) {
			return outcome{}, errForbidden
		}
		if body == "" {
			return fail("empty")
		}
		return outcome{done: "commented"}, addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindComment, body)

	case "review":
		if !p.canReview(sess.Role) {
			return outcome{}, errForbidden
		}
		switch verdict {
		case verdictApprove:
		case verdictReject:
			if body == "" {
				return fail("reason_required")
			}
		case verdictInfo:
			if body == "" {
				return fail("question_required")
			}
		default:
			return fail("bad_verdict")
		}
		out := outcome{done: "reviewed"}
		if verdict == verdictInfo {
			if err := s.askApplicant(ctx, tx, p, svc, st, q, body); err != nil {
				log.Printf("info mail: %v", err)
				return fail("mail")
			}
			if err := setAwaiting(ctx, tx, st, true); err != nil {
				return outcome{}, err
			}
			if err := addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindInfoQuestion, body); err != nil {
				return outcome{}, err
			}
			out.done = "asked"
		} else if body != "" {
			if err := addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindReview+"_"+verdict, body); err != nil {
				return outcome{}, err
			}
		}
		if err := putReview(ctx, tx, st, review{ReviewerID: sess.AccountID, ReviewerAcct: sess.Acct, ReviewerRole: sess.Role, Verdict: verdict}); err != nil {
			return outcome{}, err
		}
		if err := audit(ctx, tx, entry("review_"+verdict, "", "ok")); err != nil {
			return outcome{}, err
		}
		reviews, err := reviewsOf(ctx, tx, svc.id, st.RequestID, st.Round)
		if err != nil {
			return outcome{}, err
		}
		t := p.count(st.Escalated, reviews, checks)
		if t.Conflict && p.Review.OnConflict == "escalate" && !st.Escalated {
			st.Escalated = true
			if err := setEscalated(ctx, tx, st, true); err != nil {
				return outcome{}, err
			}
			if err := addComment(ctx, tx, st, "", "", kindSystem, "conflict"); err != nil {
				return outcome{}, err
			}
			if err := audit(ctx, tx, entry("escalate_conflict", "", "ok")); err != nil {
				return outcome{}, err
			}
			out.notify = "escalated_conflict"
			t = p.count(true, reviews, checks)
		}
		mine := !st.Escalated || hasRole(sess.Role, p.EscalateTo)
		if verdict == verdictApprove && mine && t.Ready() {
			// The vote that completes the quorum approves, with this moderator's credentials.
			var who []string
			for _, rr := range reviews {
				if rr.Verdict == verdictApprove {
					who = append(who, "@"+rr.ReviewerAcct)
				}
			}
			detail := "quorum: " + strings.Join(who, " + ")
			res, err := s.execute(ctx, tx, svc, sess, st, "approve")
			if err != nil {
				return outcome{}, err
			}
			if err := audit(ctx, tx, entry("approve_quorum", detail, res)); err != nil {
				return outcome{}, err
			}
			if res == "gone" {
				return outcome{done: "gone"}, nil
			}
			out.done = "approved"
		}
		if verdict == verdictReject && mine && t.ReadyReject() {
			// The vote that completes the reject quorum rejects, with this
			// moderator's credentials, exactly as the approval quorum approves.
			var who []string
			for _, rr := range reviews {
				if rr.Verdict == verdictReject {
					who = append(who, "@"+rr.ReviewerAcct)
				}
			}
			reason := "reject quorum: " + strings.Join(who, " + ")
			return s.reject(ctx, tx, sess, svc, st, q, reason, "reject_quorum", entry)
		}
		return out, nil

	case "approve":
		if !p.canDecide(sess.Role, st.Escalated) {
			return outcome{}, errForbidden
		}
		reviews, err := reviewsOf(ctx, tx, svc.id, st.RequestID, st.Round)
		if err != nil {
			return outcome{}, err
		}
		// Going past a reject, or a failed check, needs a word on why.
		if t := p.count(st.Escalated, reviews, checks); (t.Blocked || !t.ChecksOK) && body == "" {
			return fail("note_required")
		}
		if body != "" {
			if err := addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindApproveNote, body); err != nil {
				return outcome{}, err
			}
		}
		res, err := s.execute(ctx, tx, svc, sess, st, "approve")
		if err != nil {
			return outcome{}, err
		}
		if err := audit(ctx, tx, entry("approve", "", res)); err != nil {
			return outcome{}, err
		}
		if res == "gone" {
			return outcome{done: "gone"}, nil
		}
		return outcome{done: "approved"}, nil

	case "reject":
		if !p.canDecide(sess.Role, st.Escalated) {
			return outcome{}, errForbidden
		}
		if body == "" {
			return fail("reason_required")
		}
		return s.reject(ctx, tx, sess, svc, st, q, body, "reject", entry)

	case "escalate":
		if !p.canReview(sess.Role) {
			return outcome{}, errForbidden
		}
		if st.Escalated {
			return outcome{done: "escalated"}, nil
		}
		if body == "" {
			return fail("why_required")
		}
		if err := setEscalated(ctx, tx, st, true); err != nil {
			return outcome{}, err
		}
		if err := addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindEscalate, body); err != nil {
			return outcome{}, err
		}
		return outcome{done: "escalated", notify: "escalated"}, audit(ctx, tx, entry("escalate", "", "ok"))

	case "deescalate":
		if !p.canDeescalate(sess.Role) {
			return outcome{}, errForbidden
		}
		if err := setEscalated(ctx, tx, st, false); err != nil {
			return outcome{}, err
		}
		if err := addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindDeescalate, body); err != nil {
			return outcome{}, err
		}
		return outcome{done: "deescalated"}, audit(ctx, tx, entry("deescalate", "", "ok"))

	case "answer":
		// The applicant's answer, pasted from the mailbox. It may start a new round.
		if !p.canReview(sess.Role) {
			return outcome{}, errForbidden
		}
		if st.AwaitingSince == nil {
			return fail("not_waiting")
		}
		if body == "" {
			return fail("empty")
		}
		if err := addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindInfoAnswer, body); err != nil {
			return outcome{}, err
		}
		if err := setAwaiting(ctx, tx, st, false); err != nil {
			return outcome{}, err
		}
		if *p.Review.ResetOnNewInfo {
			if err := newRound(ctx, tx, st); err != nil {
				return outcome{}, err
			}
		}
		return outcome{done: "answered"}, audit(ctx, tx, entry("answer", "", "ok"))
	}
	return outcome{}, errForbidden
}

// reject keeps the internal reason and leaves the e-mail for after the commit.
// action is the audit action: "reject" for a moderator's decision, or
// "reject_quorum" when a reject quorum rejected on its own.
func (s *Server) reject(ctx context.Context, tx pgx.Tx, sess session, svc *service, st reqState, q module.Request, reason, action string,
	entry func(string, string, string) auditEntry) (outcome, error) {
	if err := addComment(ctx, tx, st, sess.AccountID, sess.Acct, kindRejectReason, reason); err != nil {
		return outcome{}, err
	}
	res, err := s.execute(ctx, tx, svc, sess, st, "reject")
	if err != nil {
		return outcome{}, err
	}
	if err := audit(ctx, tx, entry(action, reason, res)); err != nil {
		return outcome{}, err
	}
	if res == "gone" {
		return outcome{done: "gone"}, nil
	}
	out := outcome{done: "rejected"}
	switch {
	case !svc.mod.Caps().SilentReject:
		// The app already told the person; a second e-mail would be confusing.
	case s.mailer == nil || svc.profile.Mail.From == "":
		out.done = "no_mail"
	case q.Email != "":
		out.mailTo = q.Email
	}
	return out, nil
}

// execute calls the app. A request that disappeared meanwhile is "gone", and
// nothing else changes.
func (s *Server) execute(ctx context.Context, tx pgx.Tx, svc *service, sess session, st reqState, action string) (string, error) {
	actor := actorFor(sess, svc)
	var err error
	if action == "approve" {
		err = svc.mod.Approve(ctx, actor, st.RequestID)
	} else {
		err = svc.mod.Reject(ctx, actor, st.RequestID)
	}
	if errors.Is(err, module.ErrGone) {
		return "gone", setDecision(ctx, tx, st, "gone", "")
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", action, svc.id, err)
	}
	decision := "approved"
	if action == "reject" {
		decision = "rejected"
	}
	return "ok", setDecision(ctx, tx, st, decision, sess.Acct)
}

// askApplicant e-mails the question and remembers its Message-ID and the
// applicant's address (hashed) so the answer can find its way back.
func (s *Server) askApplicant(ctx context.Context, tx pgx.Tx, p *Profile, svc *service, st reqState, q module.Request, question string) error {
	m, err := s.trackedMail(p, svc, q, "info", question)
	if err != nil {
		return err
	}
	return s.sendTracked(ctx, tx, svc, st, q, m)
}

// trackedMail builds an e-mail to the applicant that their reply (or its bounce)
// can be matched back to.
func (s *Server) trackedMail(p *Profile, svc *service, q module.Request, kind, question string) (Message, error) {
	if s.mailer == nil || p.Mail.From == "" {
		return Message{}, errNoMail
	}
	if q.Email == "" {
		return Message{}, errors.New("no e-mail")
	}
	m, err := applicantMail(p, kind, mailData{Username: q.Username, Service: svc.cfg.Name, Question: question}, q.Email)
	if err != nil {
		return Message{}, err
	}
	m.MessageID = newMessageID(p.Mail.From)
	return m, nil
}

// sendTracked records the message in asked (so readReplies attaches the answer
// and readBounces flags a bounce) and sends it.
func (s *Server) sendTracked(ctx context.Context, tx pgx.Tx, svc *service, st reqState, q module.Request, m Message) error {
	if _, err := tx.Exec(ctx, `INSERT INTO asked (message_id, email_hash, service, request_id, username, round)
		VALUES ($1, $2, $3, $4, $5, $6)`, m.MessageID, emailHash(q.Email), svc.id, q.ID, q.Username, st.Round); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return s.mailer.Send(ctx, m)
}

// after does what must not run inside the transaction: e-mail and Mattermost.
func (s *Server) after(ctx context.Context, sess session, svc *service, q module.Request, out *outcome) {
	ctx = context.WithoutCancel(ctx)
	if out.mailTo != "" {
		m, err := applicantMail(svc.profile, "reject", mailData{Username: q.Username, Service: svc.cfg.Name}, out.mailTo)
		if err == nil {
			mctx, cancel := context.WithTimeout(ctx, time.Minute)
			err = s.mailer.Send(mctx, m)
			cancel()
		}
		result := "ok"
		if err != nil {
			log.Printf("reject mail: %v", err)
			result, out.done = "failed", "mail_failed"
		}
		s.auditAlone(ctx, auditEntry{ActorID: sess.AccountID, ActorAcct: sess.Acct, Profile: svc.profile.id, Service: svc.id,
			RequestID: q.ID, Username: q.Username, Action: "reject_mail", Result: result})
	}
	if out.notify != "" {
		t := strs(s.cfg.Lang)
		link := s.cfg.BaseURL + "/r/" + url.PathEscape(svc.id) + "/" + url.PathEscape(q.ID)
		msg := fmt.Sprintf(t["mm_"+out.notify], "@"+sess.Acct, svc.cfg.Name, q.Username, link)
		if err := s.mm.post(ctx, svc.profile.mattermost, msg); err != nil {
			log.Printf("mattermost: %v", err)
		}
		if d := svc.profile.MastodonDM; d != nil {
			s.dm(ctx, svc.profile, d.EscalationRecipients, msg, "escalation\x00"+svc.id+"\x00"+q.ID+"\x00"+out.notify)
		}
	}
}

func (s *Server) auditAlone(ctx context.Context, e auditEntry) {
	tx, err := s.db.pool.Begin(ctx)
	if err == nil {
		if err = audit(ctx, tx, e); err == nil {
			err = tx.Commit(ctx)
		}
		tx.Rollback(ctx)
	}
	if err != nil {
		log.Printf("audit: %v", err)
	}
}

func (s *Server) markGone(ctx context.Context, svc *service, id string) {
	if _, err := s.db.pool.Exec(ctx, `UPDATE requests SET decision = 'gone', decided_at = now(), updated_at = now()
		WHERE service = $1 AND request_id = $2 AND decision = ''`, svc.id, id); err != nil {
		log.Printf("gone: %v", err)
	}
}

func (s *Server) handleBulkReject(w http.ResponseWriter, r *http.Request, sess session) {
	svc, ok := s.services[r.PostFormValue("service")]
	if !ok || !svc.profile.canBulkReject(sess.Role) {
		s.renderError(w, r, http.StatusForbidden, "forbidden")
		return
	}
	reason, ok := text(r, "body")
	if !ok || reason == "" {
		http.Redirect(w, r, "/queue?done=bulk&error=reason_required", http.StatusSeeOther)
		return
	}
	ids := r.PostForm["id"]
	if len(ids) > 100 {
		ids = ids[:100]
	}
	actor := actorFor(sess, svc)
	failed := 0
	for _, id := range ids {
		q, pending, err := svc.mod.Get(r.Context(), actor, id)
		if err != nil {
			log.Printf("bulk reject %s: %v", id, err)
			failed++
			continue
		}
		if !pending {
			continue
		}
		var out outcome
		err = s.db.withRequest(r.Context(), svc.id, id, q.Username, func(tx pgx.Tx, st reqState) error {
			if st.Decision != "" || !svc.profile.canDecide(sess.Role, st.Escalated) {
				return nil
			}
			entry := func(action, detail, result string) auditEntry {
				return auditEntry{ActorID: sess.AccountID, ActorAcct: sess.Acct, Profile: svc.profile.id, Service: svc.id, RequestID: id,
					Username: q.Username, Action: "bulk_" + action, Detail: detail, Result: result}
			}
			var err error
			out, err = s.reject(r.Context(), tx, sess, svc, st, q, reason, "reject", entry)
			return err
		})
		if err != nil {
			log.Printf("bulk reject %s: %v", id, err)
			failed++
			continue
		}
		s.after(r.Context(), sess, svc, q, &out)
	}
	if failed > 0 {
		http.Redirect(w, r, "/queue?error=bulk_failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/queue?done=bulk", http.StatusSeeOther)
}

// roundView is one round of the discussion: its comments, and whether it is the
// one whose reviews count now.
type roundView struct {
	N        int
	Current  bool
	Comments []comment
}

// Rounds splits the discussion by round, in order. The current round is always
// there, empty when nobody wrote in it yet, so the page shows that earlier
// reviews stopped counting. Lines with no round (system notes) join the round
// they follow.
func (r requestView) Rounds() []roundView {
	var out []roundView
	for _, c := range r.Comments {
		n := c.Round
		if n < 1 {
			n = 1
			if len(out) > 0 {
				n = out[len(out)-1].N
			}
		}
		if len(out) == 0 || out[len(out)-1].N != n {
			out = append(out, roundView{N: n})
		}
		out[len(out)-1].Comments = append(out[len(out)-1].Comments, c)
	}
	if r.Round > 0 && (len(out) == 0 || out[len(out)-1].N < r.Round) {
		out = append(out, roundView{N: r.Round})
	}
	for i := range out {
		out[i].Current = out[i].N == r.Round
	}
	return out
}
