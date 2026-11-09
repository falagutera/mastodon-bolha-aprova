package core

import (
	"slices"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
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

// rejectQuorum is how many moderator rejections reject a request on their own,
// mirroring the approval quorum. Fixed, not per-profile: the rule is "three
// moderators said no". A reject that does not reach it still blocks an automatic
// approval (tally.Blocked), exactly as before.
const rejectQuorum = 3

// tally is where a request stands in its current round.
type tally struct {
	Approvals   int      `json:"approvals"` // that count toward the approval quorum
	Needed      int      `json:"needed"`
	Rejects     int      `json:"rejects"`      // any reviewer; blocks an automatic approval
	RejectVotes int      `json:"reject_votes"` // that count toward the reject quorum
	Infos       int      `json:"infos"`
	Conflict    bool     `json:"conflict"` // approvals and rejects at once
	Blocked     bool     `json:"blocked"`  // a reject stops the automatic approval
	ChecksOK    bool     `json:"checks_ok"`
	Failing     []string `json:"failing"` // required checks that failed or are missing
}

// Ready reports whether the quorum approves the request.
func (t tally) Ready() bool { return t.Approvals >= t.Needed && !t.Blocked && t.ChecksOK }

// ReadyReject reports whether enough moderators rejected to reject on their own.
// It yields to a conflict exactly as Ready yields to a reject (Blocked): when an
// approval contends, the request is not decided automatically in either
// direction — the on_conflict rule (escalate, or leave it for a human) runs
// instead. Unlike Ready it ignores required checks: a check is a reason to
// approve with care, never a reason to keep a rejected request open.
func (t tally) ReadyReject() bool { return t.RejectVotes >= rejectQuorum && !t.Conflict }

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
			if p.canReview(r.ReviewerRole) && (!escalated || hasRole(r.ReviewerRole, p.EscalateTo)) {
				t.RejectVotes++
			}
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
