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
	RejectMail   bool
	AppWelcome   []string // services whose app e-mails the person on approval
	NoWelcome    []string // services where approval sends nothing
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request, sess session) {
	v := s.view(r, sess, "rules")
	for _, p := range s.visibleProfiles(sess.Role) {
		rv := ruleView{Name: p.Name, Approve: p.Approve, EscalateTo: p.EscalateTo, Quorum: p.Review.Quorum,
			RejectQuorum: rejectQuorum, Escalate: p.Review.OnConflict == "escalate", Reset: *p.Review.ResetOnNewInfo,
			Checks: p.Review.RequiredChecks, Welcome: p.Mail.Welcome && s.mailer != nil,
			AutoBounce: s.cfg.AutoRejectOnBounce, StaleDays: s.cfg.Checks.InfoStaleDays,
			RejectMail: s.mailer != nil && p.Mail.From != ""}
		if rv.Name == "" {
			rv.Name = p.id
		}
		for _, sid := range p.Services {
			svc := s.services[sid]
			rv.Services = append(rv.Services, svc.cfg.Name)
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
