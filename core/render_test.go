package core

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

// Every page renders in every language, with data in every branch. A template
// error would otherwise only reach the log, after a 200.
func TestEveryPageRenders(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range supportedLangs {
		v := viewData{T: strs(lang), Lang: lang, Version: "test", Domain: "aprova.example", Acct: "ana", Role: "Sábios", CSRF: "c",
			Flash: "x", Error: "y", Modules: []string{"mastodon"}, Message: "m", Filters: filters, Mail: true, Filter: "new"}
		item := itemView{Service: "s", ID: "1", Username: "u", Email: "e@x", Reason: "r", Status: "ready", Approvals: 2, Needed: 2, Stale: true, Flags: []string{checkBotSignal}, CanBulk: true}
		v.Profiles = []profileView{{ID: "p", Name: "P", Counts: map[string]int{"ready": 1}, Services: []serviceView{
			{ID: "s", Name: "S", CanBulk: true, Items: []itemView{item}}, {ID: "t", Name: "T", Error: "unavailable"}, {ID: "u", Name: "U"}}}}
		for _, kind := range []string{kindComment, kindReview + "_approve", kindReview + "_reject", kindEscalate, kindDeescalate, kindInfoQuestion, kindInfoAnswer, kindRejectReason, kindApproveNote} {
			v.Req.Comments = append(v.Req.Comments, comment{Round: 1, Author: "a", Kind: kind, Body: "<b>x</b>", At: time.Now()})
		}
		v.Req.Comments = append(v.Req.Comments, comment{Kind: kindSystem, Body: "conflict", At: time.Now()})
		v.Audit = []auditEntry{{ActorAcct: "a", Profile: "p", Service: "s", RequestID: "1", Username: "u", Action: "approve", Detail: "d", Result: "ok", At: time.Now()}}
		for _, rv := range []requestView{
			{Service: "s", ID: "1", Pending: true, Username: "u", Email: "e@x", Reason: "r", Escalated: true, Awaiting: "now",
				Checks:    []module.Check{{Name: checkBotSignal, OK: false, Detail: "link_in_reason"}, {Name: checkReason, OK: true}},
				Reviews:   []reviewView{{Acct: "a", Role: "Sábios", Verdict: verdictApprove, Counts: true}, {Acct: "b", Verdict: verdictReject}, {Acct: "c", Verdict: verdictInfo}},
				CanReview: true, CanDecide: true, CanDeescalate: true, WouldApprove: true, SendsWelcome: true},
			{Service: "s", ID: "1", Pending: true, CanReview: true, SilentReject: true},
			{Service: "s", ID: "1", Decision: "approved", DecidedBy: "ana"},
			{Service: "s", ID: "1"},
		} {
			comments := v.Req.Comments
			v.Req = rv
			v.Req.Comments = comments
			for _, page := range []string{"index.html", "queue.html", "request.html", "audit.html", "error.html", "token.html"} {
				var b bytes.Buffer
				if err := tmpl.ExecuteTemplate(&b, page, v); err != nil {
					t.Fatalf("%s %s: %v", lang, page, err)
				}
				if strings.Contains(b.String(), "<b>x</b>") {
					t.Errorf("%s %s: HTML not escaped", lang, page)
				}
			}
		}
	}
}

// Every key used with t in the templates exists in every language.
func TestTranslationsHaveTheSameKeys(t *testing.T) {
	en := translations["en"]
	for lang, m := range translations {
		for k := range en {
			if _, ok := m[k]; !ok {
				t.Errorf("%s lacks %s", lang, k)
			}
		}
		for k := range m {
			if _, ok := en[k]; !ok {
				t.Errorf("en lacks %s (from %s)", k, lang)
			}
		}
	}
	for _, k := range []string{"status_new", "status_reviewing", "status_ready", "status_blocked", "status_awaiting", "status_escalated",
		"check_bot_signal", "check_disposable_email", "check_reason_given", "decision_approved", "decision_rejected", "decision_gone",
		"filter_new", "filter_reviewing", "filter_awaiting", "filter_escalated", "service_unavailable", "service_unauthorized"} {
		if en[k] == "" {
			t.Errorf("missing %s", k)
		}
	}
	for _, f := range flashes {
		if en["flash_"+f] == "" {
			t.Errorf("missing flash_%s", f)
		}
	}
}

// The discussion is split by round once there is more than one, and the current
// round shows even before anyone reviews in it.
func TestDiscussionSplitsRounds(t *testing.T) {
	r := requestView{Round: 2, Comments: []comment{
		{Round: 1, Author: "a", Kind: kindReview + "_approve", At: time.Now()},
		{Kind: kindSystem, Body: "conflict", At: time.Now()},
		{Round: 1, Kind: kindInfoAnswer, Body: "answer", At: time.Now()},
	}}
	rs := r.Rounds()
	if len(rs) != 2 || rs[0].N != 1 || len(rs[0].Comments) != 3 || rs[0].Current || rs[1].N != 2 || !rs[1].Current || len(rs[1].Comments) != 0 {
		t.Fatalf("rounds = %+v", rs)
	}
	if one := (requestView{Round: 1, Comments: r.Comments[:1]}).Rounds(); len(one) != 1 || !one[0].Current {
		t.Fatalf("single round = %+v", one)
	}

	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range supportedLangs {
		v := viewData{T: strs(lang), Lang: lang, Version: "test", Domain: "aprova.example", Acct: "ana", Role: "Sábios", CSRF: "c", Req: r}
		var b bytes.Buffer
		if err := tmpl.ExecuteTemplate(&b, "request.html", v); err != nil {
			t.Fatal(err)
		}
		out := b.String()
		for _, want := range []string{v.T["round_closed"], v.T["round_current"], v.T["round_started"], v.T["round_empty"]} {
			if !strings.Contains(out, template.HTMLEscapeString(want)) {
				t.Errorf("%s: missing %q", lang, want)
			}
		}
	}
}
