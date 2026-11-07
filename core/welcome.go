package core

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

// welcomeMaxAge keeps the welcome e-mail to new requests: turning it on does not
// write to a backlog of old ones, and a request that waited this long has had
// a human look at it anyway.
const welcomeMaxAge = 72 * time.Hour

// welcomed remembers, per replica, requests already handled, so the queue page
// and the minute ticker do not open a transaction for them again. The database
// (a "welcome" comment, under the request's row lock) is what really decides.
var welcomed sync.Map

// welcomeAll greets new requests on every service that can be listed without a
// moderator signed in (auth token, or digest_token_env). auth: moderator
// services without one are greeted when a moderator opens the queue.
func (s *Server) welcomeAll(ctx context.Context) {
	for _, sid := range sortedKeys(s.services) {
		svc := s.services[sid]
		if !svc.profile.Mail.Welcome || svc.cfg.digestToken == "" {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		reqs, err := svc.mod.Pending(lctx, module.Actor{Token: svc.cfg.digestToken})
		cancel()
		if err != nil {
			log.Printf("welcome %s: %v", sid, err)
			continue
		}
		s.welcome(ctx, svc, reqs)
	}
}

// welcome sends the welcome e-mail to each request that is new: recent, with an
// e-mail, undecided and with nothing on it yet (no comment, review or question).
func (s *Server) welcome(ctx context.Context, svc *service, reqs []module.Request) {
	p := svc.profile
	if !p.Mail.Welcome || s.mailer == nil || p.Mail.From == "" {
		return
	}
	for _, q := range reqs {
		if !welcomeDue(q, time.Now()) {
			continue
		}
		key := svc.id + "\x00" + q.ID
		if _, done := welcomed.Load(key); done {
			continue
		}
		sent := false
		err := s.db.withRequest(ctx, svc.id, q.ID, q.Username, func(tx pgx.Tx, st reqState) error {
			if st.Decision != "" {
				return nil
			}
			var touched bool
			if err := tx.QueryRow(ctx, `SELECT
				EXISTS (SELECT 1 FROM comments WHERE service = $1 AND request_id = $2) OR
				EXISTS (SELECT 1 FROM reviews WHERE service = $1 AND request_id = $2) OR
				EXISTS (SELECT 1 FROM asked WHERE service = $1 AND request_id = $2)`,
				svc.id, q.ID).Scan(&touched); err != nil {
				return err
			}
			if touched {
				return nil
			}
			m, err := s.trackedMail(p, svc, q, "welcome", "")
			if err != nil {
				return err
			}
			if err := s.sendTracked(ctx, tx, svc, st, q, m); err != nil {
				return err
			}
			if err := addComment(ctx, tx, st, "", "", kindWelcome, m.Body); err != nil {
				return err
			}
			sent = true
			return audit(ctx, tx, auditEntry{Profile: p.id, Service: svc.id, RequestID: q.ID,
				Username: q.Username, Action: "welcome_mail", Result: "ok"})
		})
		if err != nil {
			// Left for the next pass: nothing was committed.
			log.Printf("welcome %s/%s: %v", svc.id, q.ID, err)
			continue
		}
		welcomed.Store(key, true)
		if sent {
			log.Printf("welcome %s/%s: sent", svc.id, q.ID)
		}
	}
}

// welcomeDue: a request is greeted only while it is recent and has an address.
func welcomeDue(q module.Request, now time.Time) bool {
	return q.Email != "" && !q.Created.IsZero() && now.Sub(q.Created) < welcomeMaxAge
}
