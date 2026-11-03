package core

import (
	"slices"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

func hasRole(role string, roles []string) bool { return role != "" && slices.Contains(roles, role) }

// Who may do what on a profile. Every handler asks these, on the server.
func (p *Profile) canView(role string) bool {
	return hasRole(role, p.Approve) || hasRole(role, p.EscalateTo) || hasRole(role, p.BulkReject)
}
func (p *Profile) canReview(role string) bool {
	return hasRole(role, p.Approve) || hasRole(role, p.EscalateTo)
}
func (p *Profile) canDecide(role string, escalated bool) bool {
	if escalated {
		return hasRole(role, p.EscalateTo)
	}
	return hasRole(role, p.Approve) || hasRole(role, p.EscalateTo)
}
func (p *Profile) canBulkReject(role string) bool { return hasRole(role, p.BulkReject) }
func (p *Profile) canDeescalate(role string) bool { return hasRole(role, p.EscalateTo) }

type review struct {
	ReviewerID   string
	ReviewerAcct string
	ReviewerRole string
	Verdict      string
}

const (
	verdictApprove = "approve"
	verdictReject  = "reject"
	verdictInfo    = "info"
)

// tally is where a request stands in its current round.
type tally struct {
	Approvals int      `json:"approvals"` // that count toward the quorum
	Needed    int      `json:"needed"`
	Rejects   int      `json:"rejects"`
	Infos     int      `json:"infos"`
	Conflict  bool     `json:"conflict"` // approvals and rejects at once
	Blocked   bool     `json:"blocked"`  // a reject stops the automatic approval
	ChecksOK  bool     `json:"checks_ok"`
	Failing   []string `json:"failing"` // required checks that failed or are missing
}

// Ready reports whether the quorum approves the request.
func (t tally) Ready() bool { return t.Approvals >= t.Needed && !t.Blocked && t.ChecksOK }

// count applies the profile's review rules to the current round. When the
// request is escalated, only escalate_to votes count toward the quorum.
func (p *Profile) count(escalated bool, reviews []review, checks []module.Check) tally {
	t := tally{Needed: p.Review.Quorum, ChecksOK: true}
	seen := map[string]bool{}
	anyApprove := false
	for _, r := range reviews {
		if seen[r.ReviewerID] {
			continue // one vote per person; callers pass the latest only
		}
		seen[r.ReviewerID] = true
		switch r.Verdict {
		case verdictApprove:
			anyApprove = true
			if p.canReview(r.ReviewerRole) && (!escalated || hasRole(r.ReviewerRole, p.EscalateTo)) {
				t.Approvals++
			}
		case verdictReject:
			t.Rejects++
		case verdictInfo:
			t.Infos++
		}
	}
	t.Blocked = t.Rejects > 0
	t.Conflict = anyApprove && t.Rejects > 0
	for _, name := range p.Review.RequiredChecks {
		i := slices.IndexFunc(checks, func(c module.Check) bool { return c.Name == name })
		if i < 0 || !checks[i].OK {
			t.ChecksOK = false
			t.Failing = append(t.Failing, name)
		}
	}
	return t
}
