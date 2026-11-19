package core

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// rejectUnanswered rejects, on its own, requests whose question to the applicant
// has had no answer for the profile's review.reject_unanswered_hours. The
// applicant gets the standard rejection e-mail, as with any other rejection.
// Only services aprova can act on alone take part (systemSession); the others
// keep waiting for a moderator. Runs every minute; two replicas act once, since
// the request's row lock and its decision decide.
func (s *Server) rejectUnanswered(ctx context.Context) {
	for _, sid := range sortedKeys(s.services) {
		svc := s.services[sid]
		hours := svc.profile.Review.RejectUnansweredHours
		if hours <= 0 {
			continue
		}
		sys, ok := svc.systemSession()
		if !ok {
			continue
		}
		rows, err := s.db.pool.Query(ctx, `SELECT request_id, username FROM requests
			WHERE service = $1 AND decision = '' AND awaiting_since IS NOT NULL
			AND awaiting_since < now() - make_interval(hours => $2) ORDER BY awaiting_since LIMIT 50`, sid, hours)
		if err != nil {
			log.Printf("unanswered %s: %v", sid, err)
			continue
		}
		type due struct{ id, username string }
		var list []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.username); err != nil {
				log.Printf("unanswered %s: %v", sid, err)
				break
			}
			list = append(list, d)
		}
		rows.Close()
		for _, d := range list {
			if err := s.rejectOneUnanswered(ctx, svc, sys, hours, d.id, d.username); err != nil {
				// Left for the next minute: nothing was committed.
				log.Printf("unanswered %s/%s: %v", sid, d.id, err)
			}
		}
	}
}

func (s *Server) rejectOneUnanswered(ctx context.Context, svc *service, sys session, hours int, id, username string) error {
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	q, _, err := svc.mod.Get(lctx, actorFor(sys, svc), id)
	cancel()
	if err != nil {
		return err
	}
	var out outcome
	err = s.db.withRequest(ctx, svc.id, id, username, func(tx pgx.Tx, st reqState) error {
		// Re-check under the lock: answered, decided or asked again meanwhile.
		if st.Decision != "" || st.AwaitingSince == nil || time.Since(*st.AwaitingSince) < time.Duration(hours)*time.Hour {
			return nil
		}
		note := fmt.Sprintf(strs(s.cfg.Lang)["unanswered_note"], hours)
		o, err := s.reject(ctx, tx, sys, svc, st, q, note, "reject_unanswered", func(action, detail, result string) auditEntry {
			return auditEntry{Profile: svc.profile.id, Service: svc.id, RequestID: id, Username: username,
				Action: action, Detail: detail, Result: result}
		})
		if err != nil {
			return err
		}
		out = o
		return setAwaiting(ctx, tx, st, false)
	})
	if err != nil {
		return err
	}
	if out.done == "" {
		return nil
	}
	log.Printf("unanswered %s/%s: %s", svc.id, id, out.done)
	s.after(ctx, sys, svc, q, &out)
	return nil
}
