package core

import (
	"net/http"
	"sync"
)

// welcomeRun keeps one welcome pass at a time started from the queue page.
var welcomeRun sync.Mutex

// ruleView is how one profile decides, as the "how it works" page shows it.
// Everything comes from the running config, so the page never drifts from it.
type ruleView struct {
	Name         string
	Services     []string
	Approve      []string
	EscalateTo   []string
	Quorum       int
	RejectQuorum int
	Escalate     bool // on_conflict: escalate (else block)
	Reset        bool
	Checks       []string
	Welcome      bool
	AutoBounce   bool
	StaleDays    int
	Unanswered   int      // review.reject_unanswered_hours (0 = off)
	NoAutoAct    []string // services aprova can't act on alone, so the deadline skips them
	RejectMail   bool
	ApproveMail  bool
	AppWelcome   []string // services whose app e-mails the person on approval
	NoWelcome    []string // services where approval sends nothing
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request, sess session) {
	v := s.view(r, sess, "rules")
	for _, p := range s.visibleProfiles(sess.Role) {
		rv := ruleView{Name: p.Name, Approve: p.Approve, EscalateTo: p.EscalateTo, Quorum: p.Review.Quorum,
			RejectQuorum: p.Review.Quorum, Escalate: p.Review.OnConflict == "escalate", Reset: *p.Review.ResetOnNewInfo,
			Checks: p.Review.RequiredChecks, Welcome: p.Mail.Welcome && s.mailer != nil,
			AutoBounce: s.cfg.AutoRejectOnBounce, StaleDays: s.cfg.Checks.InfoStaleDays,
			RejectMail: s.mailer != nil && p.Mail.From != "", ApproveMail: p.Mail.Approve && s.mailer != nil && p.Mail.From != "",
			Unanswered: p.Review.RejectUnansweredHours}
		if rv.Name == "" {
			rv.Name = p.id
		}
		for _, sid := range p.Services {
			svc := s.services[sid]
			rv.Services = append(rv.Services, svc.cfg.Name)
			if _, ok := svc.systemSession(); !ok && rv.Unanswered > 0 {
				rv.NoAutoAct = append(rv.NoAutoAct, svc.cfg.Name)
			}
			if svc.mod.Caps().SendsWelcome {
				rv.AppWelcome = append(rv.AppWelcome, svc.cfg.Name)
			} else {
				rv.NoWelcome = append(rv.NoWelcome, svc.cfg.Name)
			}
		}
		v.Rules = append(v.Rules, rv)
	}
	s.render(w, "rules.html", http.StatusOK, v)
}
