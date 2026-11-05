package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestOnlyModerationRolesGetIn(t *testing.T) {
	w := newWorld(t)
	fig := w.login("fig")
	if fig.csrf != "" {
		t.Fatal("Figurinhas got in")
	}
	if !contains(w.bolha.revoked, "token-fig") {
		t.Error("refused token not revoked")
	}
	p1 := w.login("p1")
	w.gitlab.add("77", "gl-user", "gl@example.org")
	_, _, page := p1.do(http.MethodGet, "/queue", nil)
	if !strings.Contains(page, "bolha.us") || strings.Contains(page, "gl-user") {
		t.Error("Protetores must see bolha and not gcn")
	}
	if status, _, _ := p1.do(http.MethodGet, "/r/gitlab-gcn/77", nil); status != http.StatusNotFound {
		t.Errorf("gcn request for a Protetor: %d", status)
	}
	sabia := w.login("sabia")
	if _, _, page := sabia.do(http.MethodGet, "/queue", nil); !strings.Contains(page, "gl-user") {
		t.Error("Sábios must see gcn")
	}
}

func TestQuorumApprovesWithTheLastReviewersToken(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "ana", "ana@example.org", "I write about coops")
	p1, p2 := w.login("p1"), w.login("p2")
	p1.review("bolha-us", id, "approve", "looks fine")
	if by, _ := w.bolha.state(id); by != "" {
		t.Fatal("approved with one review")
	}
	if _, _, page := p2.do(http.MethodGet, "/r/bolha-us/"+id, nil); !strings.Contains(page, "Review and approve") {
		t.Error("the second reviewer isn't told their review approves")
	}
	if loc := p2.review("bolha-us", id, "approve", ""); !strings.Contains(loc, "done=approved") {
		t.Fatalf("second review: %s", loc)
	}
	if by, _ := w.bolha.state(id); by != "token-p2" {
		t.Errorf("approved with %q, want the last reviewer's own token", by)
	}
	var detail string
	w.srv.db.pool.QueryRow(context.Background(), `SELECT detail FROM audit_log WHERE request_id = $1 AND action = 'approve_quorum'`, id).Scan(&detail)
	if detail != "quorum: @p1 + @p2" {
		t.Errorf("audit: %q", detail)
	}
}

func TestOneVotePerPerson(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "ana", "ana@example.org", "hello friends")
	p1 := w.login("p1")
	p1.review("bolha-us", id, "approve", "")
	p1.review("bolha-us", id, "approve", "")
	if by, _ := w.bolha.state(id); by != "" {
		t.Error("one person's two votes made a quorum")
	}
}

func TestRejectBlocksAndConflictEscalates(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "bob", "bob@example.org", "I like birds")
	p1, p2, sabia := w.login("p1"), w.login("p2"), w.login("sabia")
	p1.review("bolha-us", id, "approve", "")
	if loc := p2.review("bolha-us", id, "reject", ""); !strings.Contains(loc, "reason_required") {
		t.Errorf("reject without reason: %s", loc)
	}
	p2.review("bolha-us", id, "reject", "same text as a known spammer")
	if !w.escalated("bolha-us", id) {
		t.Fatal("conflict didn't escalate")
	}
	if len(w.hook.msgs) != 1 || !strings.Contains(w.hook.msgs[0], "bolha.us") || strings.Contains(w.hook.msgs[0], "bob@example.org") {
		t.Errorf("mattermost: %v", w.hook.msgs)
	}
	// Escalated: a Protetor can no longer decide.
	if status, _ := p1.post("/r/bolha-us/"+id+"/approve", url.Values{"body": {"trust me"}}); status != http.StatusForbidden {
		t.Errorf("Protetor deciding an escalated request: %d", status)
	}
	if loc := sabia.review("bolha-us", id, "approve", ""); strings.Contains(loc, "approved") {
		t.Error("blocked by a reject, but approved")
	}
	if _, loc := sabia.post("/r/bolha-us/"+id+"/approve", url.Values{}); !strings.Contains(loc, "note_required") {
		t.Errorf("approving past a reject without a note: %s", loc)
	}
	sabia.post("/r/bolha-us/"+id+"/approve", url.Values{"body": {"talked to them, legit"}})
	if by, _ := w.bolha.state(id); by != "token-sabia" {
		t.Errorf("approved by %q", by)
	}
}

func TestRejectSendsTheStandardMessageOnly(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "cris", "cris@example.org", "hi")
	p1 := w.login("p1")
	if _, loc := p1.post("/r/bolha-us/"+id+"/reject", url.Values{}); !strings.Contains(loc, "reason_required") {
		t.Errorf("no reason: %s", loc)
	}
	p1.post("/r/bolha-us/"+id+"/reject", url.Values{"body": {"spam ring 42"}})
	if _, by := w.bolha.state(id); by != "token-p1" {
		t.Fatalf("not rejected: %q", by)
	}
	if len(w.mail.sent) != 1 {
		t.Fatalf("%d mails", len(w.mail.sent))
	}
	m := w.mail.sent[0]
	if m.To != "cris@example.org" || m.ReplyTo != "cadastro@bolha.example" || strings.Contains(m.Body, "spam ring") ||
		!strings.Contains(m.Body, "can't accept your request") || !strings.Contains(m.Subject, "bolha.us") {
		t.Errorf("mail: %+v", m)
	}
	var reason string
	w.srv.db.pool.QueryRow(context.Background(), `SELECT body FROM comments WHERE request_id = $1 AND kind = 'reject_reason'`, id).Scan(&reason)
	if reason != "spam ring 42" {
		t.Errorf("internal reason: %q", reason)
	}
}

// Three rejections reject on their own, mirroring the approval quorum: the vote
// that completes it rejects with that moderator's own token, and the audit names
// who. No approval is cast, so there is no conflict and nothing escalates.
func TestRejectQuorumRejectsWithTheLastReviewersToken(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "dora", "dora@example.org", "same bio as a known spam ring")
	p1, p2, sabia := w.login("p1"), w.login("p2"), w.login("sabia")
	p1.review("bolha-us", id, "reject", "looks like a spam ring")
	p2.review("bolha-us", id, "reject", "same bio as a known bot")
	if _, by := w.bolha.state(id); by != "" {
		t.Fatal("rejected before three rejections")
	}
	if _, _, page := sabia.do(http.MethodGet, "/r/bolha-us/"+id, nil); !strings.Contains(page, "Review and reject") {
		t.Error("the third reviewer isn't told their review rejects")
	}
	if loc := sabia.review("bolha-us", id, "reject", "agreed, no"); !strings.Contains(loc, "done=rejected") {
		t.Fatalf("third reject: %s", loc)
	}
	if _, by := w.bolha.state(id); by != "token-sabia" {
		t.Errorf("rejected with %q, want the last reviewer's own token", by)
	}
	if w.escalated("bolha-us", id) {
		t.Error("a reject quorum should not escalate")
	}
	var detail string
	w.srv.db.pool.QueryRow(context.Background(), `SELECT detail FROM audit_log WHERE request_id = $1 AND action = 'reject_quorum'`, id).Scan(&detail)
	if detail != "reject quorum: @p1 + @p2 + @sabia" {
		t.Errorf("audit: %q", detail)
	}
}

// A reject quorum needs no contending approval: an approval in the mix is a
// conflict, and a conflict is decided by the on_conflict rule, never by an
// automatic rejection. Mirrors how one reject blocks an automatic approval.
func TestRejectQuorumYieldsToAConflict(t *testing.T) {
	p := &Profile{Approve: []string{"mod"}, Review: ReviewConfig{Quorum: 3}}
	r := func(id, v string) review { return review{ReviewerID: id, ReviewerRole: "mod", Verdict: v} }
	clean := []review{r("a", "reject"), r("b", "reject"), r("c", "reject")}
	if !p.count(false, clean, nil).ReadyReject() {
		t.Error("three clean rejections are not a reject quorum")
	}
	conflicted := []review{r("a", "reject"), r("b", "reject"), r("c", "reject"), r("d", "approve")}
	if p.count(false, conflicted, nil).ReadyReject() {
		t.Error("rejected automatically despite a contending approval")
	}
}

// Two rejections are not a quorum; they still block an automatic approval.
func TestTwoRejectionsAreNotAQuorum(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "eli", "eli@example.org", "hello")
	p1, p2 := w.login("p1"), w.login("p2")
	p1.review("bolha-us", id, "reject", "spam ring")
	p2.review("bolha-us", id, "reject", "agreed")
	if _, by := w.bolha.state(id); by != "" {
		t.Error("two rejections rejected on their own")
	}
}

func TestFailedChecksKeepTheQuorumFromApproving(t *testing.T) {
	w := newWorld(t)
	a, b := uniq(), uniq()
	w.bolha.add(a, "dee", "dee@example.org", "visit https://casino.example")
	w.bolha.add(b, "eve", "eve@hidden-disposable.example", "hello there")
	p1, p2 := w.login("p1"), w.login("p2")
	for _, id := range []string{a, b} {
		p1.review("bolha-us", id, "approve", "")
		p2.review("bolha-us", id, "approve", "")
		if by, _ := w.bolha.state(id); by != "" {
			t.Errorf("%s approved despite a failed check", id)
		}
	}
	_, _, page := p1.do(http.MethodGet, "/r/bolha-us/"+b, nil)
	if !strings.Contains(page, "Disposable e-mail") {
		t.Error("MX on a disposable host not flagged")
	}
}

func TestAskingTheApplicantStartsANewRound(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "fay", "fay@example.org", "art")
	p1, p2 := w.login("p1"), w.login("p2")
	p1.review("bolha-us", id, "approve", "")
	if loc := p2.review("bolha-us", id, "info", ""); !strings.Contains(loc, "question_required") {
		t.Errorf("info without question: %s", loc)
	}
	p2.review("bolha-us", id, "info", "Which collective are you part of?")
	if len(w.mail.sent) != 1 || !strings.Contains(w.mail.sent[0].Body, "Which collective") || w.mail.sent[0].To != "fay@example.org" {
		t.Fatalf("info mail: %+v", w.mail.sent)
	}
	st, _ := w.srv.db.state(context.Background(), "bolha-us", id)
	if st.AwaitingSince == nil {
		t.Fatal("not waiting")
	}
	p2.post("/r/bolha-us/"+id+"/answer", url.Values{"body": {"The bike coop downtown."}})
	st, _ = w.srv.db.state(context.Background(), "bolha-us", id)
	if st.AwaitingSince != nil || st.Round != 2 {
		t.Fatalf("after answer: %+v", st)
	}
	// p1's earlier approval is from round 1 and no longer counts.
	p2.review("bolha-us", id, "approve", "")
	if by, _ := w.bolha.state(id); by != "" {
		t.Error("an old-round vote counted")
	}
	p1.review("bolha-us", id, "approve", "")
	if by, _ := w.bolha.state(id); by != "token-p1" {
		t.Errorf("round 2 quorum: %q", by)
	}
}

func TestMailFailureChangesNothing(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "gil", "gil@example.org", "music")
	w.mail.fail = true
	p1 := w.login("p1")
	if loc := p1.review("bolha-us", id, "info", "Tell us more?"); !strings.Contains(loc, "error=mail") {
		t.Errorf("info with mail down: %s", loc)
	}
	st, _ := w.srv.db.state(context.Background(), "bolha-us", id)
	if st.AwaitingSince != nil {
		t.Error("marked waiting though the mail failed")
	}
}

func TestServiceTokenAndGitLab(t *testing.T) {
	w := newWorld(t)
	j, g := uniq(), "9"+uniq()[:6]
	w.jorn.add(j, "hal", "hal@example.org", "journalist at a local paper")
	w.gitlab.add(g, "ivy", "ivy@example.org")
	sabia := w.login("sabia")
	sabia.post("/r/jornalismo/"+j+"/approve", url.Values{})
	if by, _ := w.jorn.state(j); by != "svc-jorn" {
		t.Errorf("jornalismo approved with %q, want the service token", by)
	}
	sabia.post("/r/gitlab-gcn/"+g+"/reject", url.Values{"body": {"unknown"}})
	if !w.gitlab.rejected[g] {
		t.Fatal("gitlab not rejected")
	}
	if len(w.mail.sent) != 0 {
		t.Error("GitLab already e-mails rejected users; aprova must not send another")
	}
}

func TestBulkReject(t *testing.T) {
	w := newWorld(t)
	a, b := uniq(), uniq()
	w.bolha.add(a, "bot1", "bot1@example.org", "XyZabcQwe")
	w.bolha.add(b, "bot2", "bot2@example.org", "AbcXyzQwe")
	p1, sabia := w.login("p1"), w.login("sabia")
	if status, _ := p1.post("/bulk-reject", url.Values{"service": {"bolha-us"}, "id": {a, b}, "body": {"bots"}}); status != http.StatusForbidden {
		t.Errorf("Protetor bulk: %d", status)
	}
	sabia.post("/bulk-reject", url.Values{"service": {"bolha-us"}, "id": {a, b}, "body": {"bot wave"}})
	_, ra := w.bolha.state(a)
	_, rb := w.bolha.state(b)
	if ra != "token-sabia" || rb != "token-sabia" || len(w.mail.sent) != 2 {
		t.Errorf("bulk: %q %q %d mails", ra, rb, len(w.mail.sent))
	}
}

func TestDecidedElsewhereIsGone(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "jo", "jo@example.org", "hi there")
	p1 := w.login("p1")
	p1.post("/r/bolha-us/"+id+"/comment", url.Values{"body": {"looking"}}) // aprova knows it now
	w.bolha.mu.Lock()
	delete(w.bolha.accounts, id) // decided directly on the instance
	w.bolha.mu.Unlock()
	if loc := p1.review("bolha-us", id, "approve", ""); !strings.Contains(loc, "done=gone") {
		t.Errorf("gone: %s", loc)
	}
	if d := w.decision("bolha-us", id); d != "gone" {
		t.Errorf("decision %q", d)
	}
}

func TestActionsNeedCSRFAndSameSite(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "kim", "kim@example.org", "hi there")
	p1 := w.login("p1")
	if status, _, _ := p1.do(http.MethodPost, "/r/bolha-us/"+id+"/reject", url.Values{"body": {"x"}}); status != http.StatusForbidden {
		t.Errorf("no csrf: %d", status)
	}
	req, _ := http.NewRequest(http.MethodPost, w.app.URL+"/r/bolha-us/"+id+"/reject", strings.NewReader(url.Values{"csrf": {p1.csrf}, "body": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", p1.cookie)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site: %d", resp.StatusCode)
	}
	if _, by := w.bolha.state(id); by != "" {
		t.Error("rejected anyway")
	}
}

func TestLosingTheRoleEndsTheSession(t *testing.T) {
	w := newWorld(t)
	p1 := w.login("p1")
	w.bolha.mu.Lock()
	w.bolha.roles["3"] = "Figurinhas"
	w.bolha.mu.Unlock()
	w.srv.db.pool.Exec(context.Background(), `UPDATE sessions SET role_checked = now() - interval '5 minutes'`)
	if status, loc, _ := p1.do(http.MethodGet, "/queue", nil); status != http.StatusFound || loc != "/" {
		t.Errorf("still in: %d %s", status, loc)
	}
}

func TestLogoutRevokesOnlyTheLastSession(t *testing.T) {
	w := newWorld(t)
	a, b := w.login("p1"), w.login("p1") // same token on the instance
	a.post("/logout", url.Values{})
	if contains(w.bolha.revoked, "token-p1") {
		t.Fatal("revoked a token another session uses")
	}
	if status, _, _ := b.do(http.MethodGet, "/queue", nil); status != http.StatusOK {
		t.Errorf("other session: %d", status)
	}
	b.post("/logout", url.Values{})
	if !contains(w.bolha.revoked, "token-p1") {
		t.Error("last session's token not revoked")
	}
}

func api(t *testing.T, w *world, token, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var r *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		r = strings.NewReader(string(raw))
	} else {
		r = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, w.app.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

var apiTokenRe = regexp.MustCompile(`aprova_[A-Za-z0-9_-]+`)

func (b *browser) apiToken() string {
	b.t.Helper()
	b.post("/api-token", url.Values{})
	_, _, page := b.do(http.MethodPost, "/api-token", url.Values{"csrf": {b.csrf}})
	tok := apiTokenRe.FindString(page)
	if tok == "" {
		b.t.Fatal("no API token on the page")
	}
	return tok
}

func TestAPIUsesTheSameRules(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "lia", "Lia@Example.org", "photographer")
	p1tok, p2tok := w.login("p1").apiToken(), w.login("p2").apiToken()
	if status, _ := api(t, w, "", http.MethodGet, "/api/v1/queue", nil); status != http.StatusUnauthorized {
		t.Errorf("no token: %d", status)
	}
	// A Mastodon token, from aprova's app or any other, is not an API token.
	for _, tok := range []string{"token-p1", "token-guto", "aprova_made-up"} {
		if status, _ := api(t, w, tok, http.MethodGet, "/api/v1/queue", nil); status != http.StatusUnauthorized {
			t.Errorf("%s: %d", tok, status)
		}
	}
	status, out := api(t, w, p1tok, http.MethodGet, "/api/v1/search?email=lia@example.org", nil)
	if status != http.StatusOK || len(out["requests"].([]any)) != 1 {
		t.Fatalf("search: %d %v", status, out)
	}
	if status, out := api(t, w, p1tok, http.MethodPost, "/api/v1/requests/bolha-us/"+id+"/reject", map[string]string{}); status != http.StatusUnprocessableEntity || out["error"] != "reason_required" {
		t.Errorf("reject without reason: %d %v", status, out)
	}
	api(t, w, p1tok, http.MethodPost, "/api/v1/requests/bolha-us/"+id+"/review", map[string]string{"verdict": "approve"})
	status, out = api(t, w, p2tok, http.MethodPost, "/api/v1/requests/bolha-us/"+id+"/review", map[string]string{"verdict": "approve"})
	if status != http.StatusOK || out["result"] != "approved" {
		t.Errorf("quorum by API: %d %v", status, out)
	}
	if by, _ := w.bolha.state(id); by != "token-p2" {
		t.Errorf("approved with %q", by)
	}
	if status, _ := api(t, w, p1tok, http.MethodGet, "/api/v1/requests/gitlab-gcn/1", nil); status != http.StatusNotFound {
		t.Errorf("gcn for a Protetor: %d", status)
	}
	// A browser session cookie alone never works on the API.
	b := w.login("p1")
	req, _ := http.NewRequest(http.MethodGet, w.app.URL+"/api/v1/queue", nil)
	req.Header.Set("Cookie", b.cookie)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("cookie on the API: %d", resp.StatusCode)
	}
}

func TestDigestPostsOnlyChanges(t *testing.T) {
	w := newWorld(t)
	w.jorn.add(uniq(), "mo", "mo@example.org", "hi")
	w.bolha.add(uniq(), "nu", "nu@example.org", "hi") // auth moderator, no digest token: not counted
	w.srv.Digest(context.Background())
	w.srv.Digest(context.Background())
	if len(w.hook.msgs) != 1 || !strings.Contains(w.hook.msgs[0], "jornalismo.social 1") || strings.Contains(w.hook.msgs[0], "bolha.us") {
		t.Errorf("digest: %v", w.hook.msgs)
	}
	w.jorn.add(uniq(), "ox", "ox@example.org", "hi")
	w.srv.Digest(context.Background())
	if len(w.hook.msgs) != 2 || !strings.Contains(w.hook.msgs[1], "jornalismo.social 2") {
		t.Errorf("after a new one: %v", w.hook.msgs)
	}
}

func TestDescalateOnlyBySeniors(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "pat", "pat@example.org", "hello")
	p1, sabia := w.login("p1"), w.login("sabia")
	if _, loc := p1.post("/r/bolha-us/"+id+"/escalate", url.Values{}); !strings.Contains(loc, "why_required") {
		t.Errorf("escalate without why: %s", loc)
	}
	p1.post("/r/bolha-us/"+id+"/escalate", url.Values{"body": {"not sure"}})
	if status, _ := p1.post("/r/bolha-us/"+id+"/deescalate", url.Values{}); status != http.StatusForbidden {
		t.Errorf("Protetor de-escalating: %d", status)
	}
	sabia.post("/r/bolha-us/"+id+"/deescalate", url.Values{"body": {"fine"}})
	if w.escalated("bolha-us", id) {
		t.Error("still escalated")
	}
}

var _ = time.Now

// Once escalated, only the senior roles count toward the quorum, and only they decide.
func TestEscalatedQuorumCountsSeniorsOnly(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "quin", "quin@example.org", "I organize a reading club")
	p1, p2, sabia, guto := w.login("p1"), w.login("p2"), w.login("sabia"), w.login("guto")
	p1.post("/r/bolha-us/"+id+"/escalate", url.Values{"body": {"name matches an old ban"}})
	p1.review("bolha-us", id, "approve", "")
	p2.review("bolha-us", id, "approve", "")
	if by, _ := w.bolha.state(id); by != "" {
		t.Fatal("two Protetores approved an escalated request")
	}
	if status, _ := p2.post("/r/bolha-us/"+id+"/reject", url.Values{"body": {"no"}}); status != http.StatusForbidden {
		t.Errorf("Protetor rejecting an escalated request: %d", status)
	}
	sabia.review("bolha-us", id, "approve", "")
	if by, _ := w.bolha.state(id); by != "" {
		t.Fatal("one senior vote made the quorum")
	}
	guto.review("bolha-us", id, "approve", "")
	if by, _ := w.bolha.state(id); by != "token-guto" {
		t.Errorf("seniors' quorum: %q", by)
	}
}

// The module's credentials can read any account. aprova must only show what is
// a pending request, and must not take note of ids it never handled.
func TestNoDataAboutAccountsThatArentPending(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.jorn.add(id, "admin", "secret-admin@jorn.example", "")
	w.jorn.mu.Lock()
	w.jorn.accounts[id].Approved = true // an account approved long ago, never seen by aprova
	w.jorn.mu.Unlock()
	p1 := w.login("p1")
	tok := p1.apiToken()
	p1.post("/r/jornalismo/"+id+"/comment", url.Values{"body": {"hi"}})
	api(t, w, tok, http.MethodPost, "/api/v1/requests/jornalismo/"+id+"/comment", map[string]string{"body": "hi"})
	status, out := api(t, w, tok, http.MethodGet, "/api/v1/requests/jornalismo/"+id, nil)
	if status != http.StatusNotFound || strings.Contains(fmtAny(out), "secret-admin") {
		t.Errorf("unknown account: %d %v", status, out)
	}
	var n int
	w.srv.db.pool.QueryRow(context.Background(), `SELECT count(*) FROM requests WHERE request_id = $1`, id).Scan(&n)
	if n != 0 {
		t.Error("a row was created for an id aprova never handled")
	}
	// A request aprova decided shows its history, never the account's data.
	d := uniq()
	w.jorn.add(d, "rae", "rae-private@jorn.example", "my reason")
	sabia := w.login("sabia")
	sabia.post("/r/jornalismo/"+d+"/approve", url.Values{})
	_, out = api(t, w, sabia.apiToken(), http.MethodGet, "/api/v1/requests/jornalismo/"+d, nil)
	if s := fmtAny(out); strings.Contains(s, "rae-private") || strings.Contains(s, "my reason") || out["decision"] != "approved" {
		t.Errorf("decided request: %v", out)
	}
}

func fmtAny(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func TestAnswerNeedsAQuestion(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "sam", "sam@example.org", "hello")
	sabia, p1 := w.login("sabia"), w.login("p1")
	sabia.review("bolha-us", id, "reject", "known spammer")
	if _, loc := p1.post("/r/bolha-us/"+id+"/answer", url.Values{"body": {"fake answer"}}); !strings.Contains(loc, "not_waiting") {
		t.Errorf("answer with no question: %s", loc)
	}
	st, _ := w.srv.db.state(context.Background(), "bolha-us", id)
	if st.Round != 1 {
		t.Errorf("round moved to %d", st.Round)
	}
}

func TestWebhookURLStaysOutOfErrors(t *testing.T) {
	m := &mattermost{client: &http.Client{Timeout: time.Second}}
	err := m.post(context.Background(), "https://127.0.0.1:1/hooks/super-secret-id", "x")
	if err == nil || strings.Contains(err.Error(), "super-secret-id") {
		t.Errorf("error: %v", err)
	}
}

func (in *instance) sentDMs() []url.Values {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]url.Values(nil), in.dms...)
}

func TestBotSendsDirectMessagesAndEachPersonCanTurnThemOff(t *testing.T) {
	w := newWorld(t)
	w.jorn.add(uniq(), "zed", "zed@example.org", "hi")
	w.srv.Digest(context.Background())
	dms := w.bolha.sentDMs()
	if len(dms) != 2 {
		t.Fatalf("%d digest DMs: %v", len(dms), dms)
	}
	for _, d := range dms {
		if d.Get("visibility") != "direct" || d.Get("token") != "bot-token" || strings.Contains(d.Get("status"), "zed@example.org") || d.Get("key") == "" {
			t.Errorf("dm: %v", d)
		}
	}
	if !strings.HasPrefix(dms[0].Get("status"), "@p1 ") && !strings.HasPrefix(dms[1].Get("status"), "@p1 ") {
		t.Error("no DM to p1")
	}
	// p2 turns them off in the panel; the next digest goes to p1 only.
	p2 := w.login("p2")
	if _, _, page := p2.do(http.MethodGet, "/queue", nil); !strings.Contains(page, "Turn off") {
		t.Fatal("no switch for a recipient")
	}
	p2.post("/prefs", url.Values{"dm": {"off"}})
	w.jorn.add(uniq(), "zoe", "zoe@example.org", "hi")
	w.srv.Digest(context.Background())
	dms = w.bolha.sentDMs()[2:]
	if len(dms) != 1 || !strings.HasPrefix(dms[0].Get("status"), "@p1 ") {
		t.Errorf("after p2 turned off: %v", dms)
	}
	// Someone who isn't a recipient sees no switch.
	if _, _, page := w.login("guto").do(http.MethodGet, "/queue", nil); strings.Contains(page, "Turn off") || strings.Contains(page, "Turn on") {
		t.Error("switch shown to a non-recipient")
	}
	// Escalations go to the escalation recipients.
	id := uniq()
	w.bolha.add(id, "ash", "ash@example.org", "hello")
	before := len(w.bolha.sentDMs())
	w.login("p1").post("/r/bolha-us/"+id+"/escalate", url.Values{"body": {"unsure"}})
	esc := w.bolha.sentDMs()[before:]
	if len(esc) != 1 || !strings.HasPrefix(esc[0].Get("status"), "@sabia ") || !strings.Contains(esc[0].Get("status"), "/r/bolha-us/"+id) {
		t.Errorf("escalation DM: %v", esc)
	}
}

// The bot is refused when its configuration is unclear.
func TestBotConfiguration(t *testing.T) {
	for name, dm := range map[string]string{
		"no token":      `{ token_env: T_NONE, recipients: [p1] }`,
		"no recipients": `{ token_env: T_BOT }`,
		"remote":        `{ token_env: T_BOT, recipients: ["p1@other.example"] }`,
	} {
		bad := strings.Replace(testConfig, `{ token_env: T_BOT, recipients: ["@p1", "P2"], escalation_recipients: [sabia] }`, dm, 1)
		if _, err := ParseConfig([]byte(fmt.Sprintf(bad, "https://a.example", "https://b.example", "https://c.example")), testEnv(map[string]string{"T_HOOK": "https://hook.example/x"})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAuthorsFixTheirOwnComments(t *testing.T) {
	w := newWorld(t)
	id := uniq()
	w.bolha.add(id, "tom", "tom@example.org", "hello")
	sabia, p1 := w.login("sabia"), w.login("p1")
	sabia.review("bolha-us", id, "reject", "Ne parece bot")
	ctx := context.Background()
	list, _ := w.srv.db.comments(ctx, "bolha-us", id)
	var cid int64
	for _, c := range list {
		if c.Kind == kindReview+"_"+verdictReject {
			cid = c.ID
		}
	}
	if cid == 0 {
		t.Fatalf("no review comment: %+v", list)
	}
	edit := func(c *browser, body string) string {
		_, loc := c.post("/r/bolha-us/"+id+"/edit", url.Values{"comment": {fmt.Sprint(cid)}, "body": {body}})
		return loc
	}
	if code, _ := p1.post("/r/bolha-us/"+id+"/edit", url.Values{"comment": {fmt.Sprint(cid)}, "body": {"hijack"}}); code != http.StatusForbidden {
		t.Errorf("someone else's comment: %d", code)
	}
	if loc := edit(sabia, ""); !strings.Contains(loc, "error=empty") {
		t.Errorf("empty edit: %s", loc)
	}
	if loc := edit(sabia, "Me parece bot"); !strings.Contains(loc, "done=edited") {
		t.Errorf("edit: %s", loc)
	}
	list, _ = w.srv.db.comments(ctx, "bolha-us", id)
	for _, c := range list {
		if c.ID == cid && (c.Body != "Me parece bot" || c.EditedAt == nil) {
			t.Errorf("after edit: %+v", c)
		}
	}
	var old string
	w.srv.db.pool.QueryRow(ctx, `SELECT detail FROM audit_log WHERE service = 'bolha-us' AND request_id = $1 AND action = 'comment_edit'`, id).Scan(&old)
	if old != "Ne parece bot" {
		t.Errorf("old text in the log: %q", old)
	}
	// A question already went out by e-mail: it stays as sent.
	sabia.review("bolha-us", id, "info", "Can you tell us more?")
	list, _ = w.srv.db.comments(ctx, "bolha-us", id)
	for _, c := range list {
		if c.Kind == kindInfoQuestion || c.Kind == kindReview+"_"+verdictInfo {
			if code, _ := sabia.post("/r/bolha-us/"+id+"/edit", url.Values{"comment": {fmt.Sprint(c.ID)}, "body": {"changed"}}); code != http.StatusForbidden {
				t.Errorf("edited a question already sent: %d", code)
			}
		}
	}
}
