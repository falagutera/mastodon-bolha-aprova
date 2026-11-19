package core

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// withDeadline turns on the unanswered-question deadline on the bolha profile;
// act also gives bolha.us (auth: moderator) a token aprova can act with alone.
func withDeadline(act bool) func(string) string {
	return func(c string) string {
		c = strings.Replace(c, "review: { quorum: 2,", "review: { quorum: 2, reject_unanswered_hours: 24,", 1)
		if act {
			c = strings.Replace(c, "auth: moderator }", "auth: moderator, act_token_env: T_ACT }", 1)
		}
		return c
	}
}

// askedHoursAgo moves the question back in time, as if it went out that long ago.
func askedHoursAgo(t *testing.T, w *world, service, id string, hours int) {
	t.Helper()
	if _, err := w.srv.db.pool.Exec(context.Background(), `UPDATE requests SET awaiting_since = now() - make_interval(hours => $3)
		WHERE service = $1 AND request_id = $2`, service, id, hours); err != nil {
		t.Fatal(err)
	}
}

func rejectMails(w *world, to string) int {
	w.mail.mu.Lock()
	defer w.mail.mu.Unlock()
	n := 0
	for _, m := range w.mail.sent {
		if m.To == to && strings.Contains(m.Body, "can't accept") {
			n++
		}
	}
	return n
}

func TestUnansweredQuestionRejectsAfterTheDeadline(t *testing.T) {
	w := newWorldWith(t, withDeadline(false), nil)
	ctx := context.Background()
	id, _ := askQuestion(t, w, "quiet@example.org")

	askedHoursAgo(t, w, "jornalismo", id, 23)
	w.srv.rejectUnanswered(ctx)
	if st, _ := w.srv.db.state(ctx, "jornalismo", id); st.Decision != "" {
		t.Fatalf("rejected before the deadline: %q", st.Decision)
	}

	askedHoursAgo(t, w, "jornalismo", id, 25)
	w.srv.rejectUnanswered(ctx)
	w.srv.rejectUnanswered(ctx) // a second replica, or the next minute: once
	st, _ := w.srv.db.state(ctx, "jornalismo", id)
	if st.Decision != "rejected" || st.AwaitingSince != nil {
		t.Errorf("after the deadline: %+v", st)
	}
	if _, rej := w.jorn.state(id); rej == "" {
		t.Error("the app was not told to reject")
	}
	if n := rejectMails(w, "quiet@example.org"); n != 1 {
		t.Errorf("%d rejection e-mails, want 1", n)
	}
	// The reason stays with the team; the person gets the standard text only.
	w.mail.mu.Lock()
	for _, m := range w.mail.sent {
		if strings.Contains(m.Body, "24 hours") {
			t.Error("the internal reason went to the applicant")
		}
	}
	w.mail.mu.Unlock()
	list, _ := w.srv.db.comments(ctx, "jornalismo", id)
	found := false
	for _, c := range list {
		if c.Kind == kindRejectReason && strings.Contains(c.Body, "24 hours") {
			found = true
		}
	}
	if !found {
		t.Error("no reason recorded for the team")
	}
}

func TestAnsweredQuestionIsNotRejected(t *testing.T) {
	w := newWorldWith(t, withDeadline(false), nil)
	ctx := context.Background()
	id, _ := askQuestion(t, w, "talks@example.org")
	askedHoursAgo(t, w, "jornalismo", id, 30)
	// The answer arrives (here recorded by hand): no longer waiting.
	if err := w.srv.db.withRequest(ctx, "jornalismo", id, "no", func(tx pgx.Tx, st reqState) error {
		return setAwaiting(ctx, tx, st, false)
	}); err != nil {
		t.Fatal(err)
	}
	w.srv.rejectUnanswered(ctx)
	if st, _ := w.srv.db.state(ctx, "jornalismo", id); st.Decision != "" {
		t.Errorf("answered request rejected: %q", st.Decision)
	}
}

func askOnBolha(t *testing.T, w *world, email string) string {
	t.Helper()
	id := uniq()
	w.bolha.add(id, "ana", email, "a reason")
	w.login("p1").review("bolha-us", id, "info", "Who are you?")
	return id
}

func TestDeadlineOnModeratorServiceNeedsActToken(t *testing.T) {
	ctx := context.Background()

	w := newWorldWith(t, withDeadline(false), nil)
	id := askOnBolha(t, w, "ana1@example.org")
	askedHoursAgo(t, w, "bolha-us", id, 25)
	w.srv.rejectUnanswered(ctx)
	if st, _ := w.srv.db.state(ctx, "bolha-us", id); st.Decision != "" {
		t.Errorf("acted on bolha.us with no token of its own: %q", st.Decision)
	}

	w = newWorldWith(t, withDeadline(true), nil)
	id = askOnBolha(t, w, "ana2@example.org")
	askedHoursAgo(t, w, "bolha-us", id, 25)
	w.srv.rejectUnanswered(ctx)
	if st, _ := w.srv.db.state(ctx, "bolha-us", id); st.Decision != "rejected" {
		t.Errorf("not rejected with act_token_env: %q", st.Decision)
	}
	if _, by := w.bolha.state(id); by != "token-sabia" {
		t.Errorf("rejected with %q, want the act token", by)
	}
	if n := rejectMails(w, "ana2@example.org"); n != 1 {
		t.Errorf("%d rejection e-mails, want 1", n)
	}
}

func TestDeadlineConfig(t *testing.T) {
	env := testEnv(map[string]string{"T_HOOK": "https://h.example", "APROVA_DB_HOST": "db.invalid"})
	parse := func(c string) error {
		_, err := ParseConfig([]byte(strings.NewReplacer("%[1]s", "https://a.example", "%[2]s", "https://b.example", "%[3]s", "https://c.example").Replace(c)), env)
		return err
	}
	if err := parse(withDeadline(true)(testConfig)); err != nil {
		t.Errorf("valid config refused: %v", err)
	}
	bad := map[string]string{
		"negative hours":       strings.Replace(testConfig, "review: { quorum: 2,", "review: { quorum: 2, reject_unanswered_hours: -1,", 1),
		"act token on a token": strings.Replace(testConfig, "token_env: T_JORN }", "token_env: T_JORN, act_token_env: T_ACT }", 1),
		"empty act token":      strings.Replace(testConfig, "auth: moderator }", "auth: moderator, act_token_env: T_NONE }", 1),
	}
	for name, c := range bad {
		if err := parse(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
