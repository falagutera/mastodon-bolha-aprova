package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

func TestWelcomeDue(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		q    module.Request
		want bool
	}{
		{module.Request{Email: "a@b.c", Created: now.Add(-time.Hour)}, true},
		{module.Request{Email: "", Created: now.Add(-time.Hour)}, false},
		{module.Request{Email: "a@b.c"}, false},                                       // unknown age
		{module.Request{Email: "a@b.c", Created: now.Add(-welcomeMaxAge - 1)}, false}, // backlog
	} {
		if got := welcomeDue(c.q, now); got != c.want {
			t.Errorf("welcomeDue(%+v) = %v", c.q, got)
		}
	}
}

func TestWelcomeMailIsBilingual(t *testing.T) {
	p := &Profile{Mail: ProfileMail{From: "aprova@x", Lang: "en"}}
	m, err := applicantMail(p, "welcome", mailData{Username: "gil", Service: "bolha.us"}, "gil@example.org")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Recebemos seu pedido de conta em bolha.us (gil)", "We received your request for an account on bolha.us (gil)"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, m.Body)
		}
	}
	p.Mail.WelcomeSubject, p.Mail.WelcomeBody = "Hi {{.Service}}", "Custom for {{.Username}}"
	m, _ = applicantMail(p, "welcome", mailData{Username: "gil", Service: "bolha.us"}, "gil@example.org")
	if m.Subject != "Hi bolha.us" || m.Body != "Custom for gil" {
		t.Errorf("yaml texts not used: %+v", m)
	}
}

func TestWelcomeSentOnceTrackedAndBounces(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, p := range w.srv.cfg.Profiles {
		p.Mail.Welcome = true
	}
	welcomed.Clear()
	id := uniq()
	w.jorn.add(id, "nova", "nova@example.org", "a reason")
	before := len(w.mail.sent)
	w.srv.welcomeAll(ctx)
	welcomed.Clear() // the other replica, without the in-memory cache
	w.srv.welcomeAll(ctx)
	sent := w.mail.sent[before:]
	n := 0
	var mid string
	for _, m := range sent {
		if m.To == "nova@example.org" {
			n++
			mid = m.MessageID
		}
	}
	if n != 1 {
		t.Fatalf("%d welcome e-mails", n)
	}
	list, _ := w.srv.db.comments(ctx, "jornalismo", id)
	if len(list) != 1 || list[0].Kind != kindWelcome || !strings.Contains(list[0].Body, "nova") {
		t.Fatalf("welcome is not the first interaction: %+v", list)
	}
	st, _ := w.srv.db.state(ctx, "jornalismo", id)
	if st.AwaitingSince != nil {
		t.Error("the welcome must not put the request in waiting")
	}
	// The reply lands in the request, like an answer to a question.
	w.srv.inbox = &fakeInbox{done: map[uint32]bool{}, msgs: []Inbound{
		{UID: 1, MessageID: "r" + id + "@ex", References: []string{mid}, From: "nova@example.org", Body: "Sou jornalista."},
	}}
	w.srv.readReplies(ctx)
	// And a bounce of it flags the request.
	w.srv.bounce = &fakeInbox{done: map[uint32]bool{}, msgs: []Inbound{
		{UID: 1, MessageID: "dsn" + id + "@mx", IsBounce: true, BounceOf: mid, BounceRecipient: "nova@example.org"},
	}}
	w.srv.readBounces(ctx)
	st, _ = w.srv.db.state(ctx, "jornalismo", id)
	if !st.Undeliverable {
		t.Error("bounce of the welcome did not flag the request")
	}
	list, _ = w.srv.db.comments(ctx, "jornalismo", id)
	var kinds []string
	for _, c := range list {
		kinds = append(kinds, c.Kind)
	}
	if strings.Join(kinds, ",") != "welcome,info_answer,bounce" {
		t.Errorf("discussion: %v", kinds)
	}
}

func TestWelcomeSkipsTouchedAndModeratorQueue(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, p := range w.srv.cfg.Profiles {
		p.Mail.Welcome = true
	}
	welcomed.Clear()
	// A request someone already reviewed is not greeted.
	old := uniq()
	w.jorn.add(old, "velho", "velho@example.org", "r")
	p1 := w.login("p1")
	p1.review("jornalismo", old, "approve", "")
	// bolha-us is auth: moderator with no digest token: greeted from the queue.
	fresh := uniq()
	w.bolha.add(fresh, "fresca", "fresca@example.org", "r")
	before := len(w.mail.sent)
	w.srv.welcomeAll(ctx)
	p1.do("GET", "/queue", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		welcomeRun.Lock() // waits for the pass the queue page started
		welcomeRun.Unlock()
		list, _ := w.srv.db.comments(ctx, "bolha-us", fresh)
		if len(list) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := map[string]int{}
	w.mail.mu.Lock()
	defer w.mail.mu.Unlock()
	for _, m := range w.mail.sent[before:] {
		got[m.To]++
	}
	if got["velho@example.org"] != 0 {
		t.Error("greeted a request already reviewed")
	}
	if got["fresca@example.org"] != 1 {
		t.Errorf("moderator-auth request greeted %d times", got["fresca@example.org"])
	}
}

func TestRulesPage(t *testing.T) {
	w := newWorld(t)
	p1 := w.login("p1")
	code, _, body := p1.do("GET", "/how-it-works", nil)
	if code != 200 || !strings.Contains(body, "How approval works") || !strings.Contains(body, "Automatic rejection:") {
		t.Fatalf("rules page %d:\n%s", code, body)
	}
	for _, want := range []string{"Rejection e-mail:", "Approval e-mail:", "send their own welcome e-mail"} {
		if !strings.Contains(body, want) {
			t.Errorf("rules page without %q", want)
		}
	}
	if !strings.Contains(body, `href="/how-it-works"`) {
		t.Error("no menu entry")
	}
	anon := &browser{t: t, w: w}
	if code, _, _ := anon.do("GET", "/how-it-works", nil); code == 200 {
		t.Error("rules page open to the public")
	}
}
