package core

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestParseInboundGmailReply(t *testing.T) {
	raw := "Message-ID: <CAF123@mail.gmail.com>\r\n" +
		"In-Reply-To: <aprova.abc@bolha.us>\r\n" +
		"References: <aprova.abc@bolha.us>\r\n" +
		"From: Fay Example <Fay@Example.org>\r\n" +
		"Subject: Re: your request\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=\"UTF-8\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"I'm part of the bike coop, in S=C3=A3o Paulo.\r\n\r\n" +
		"On Mon, Nov 3, 2026 at 1:31 PM bolha.us <aprova@bolha.us>\r\nwrote:\r\n\r\n> Which collective are you part of?\r\n" +
		"--b1\r\nContent-Type: text/html; charset=\"UTF-8\"\r\n\r\n<div>I'm part of the bike coop</div>\r\n--b1--\r\n"
	in, err := parseInbound(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if in.MessageID != "CAF123@mail.gmail.com" || in.From != "fay@example.org" || len(in.References) != 2 || in.References[0] != "aprova.abc@bolha.us" {
		t.Errorf("headers: %+v", in)
	}
	if in.Body != "I'm part of the bike coop, in São Paulo." {
		t.Errorf("body: %q", in.Body)
	}
}

func TestStripQuoted(t *testing.T) {
	for in, want := range map[string]string{
		"Sou do coletivo X.\n\nEm seg., 3 de nov. de 2026 às 13:31, bolha.us <aprova@bolha.us> escreveu:\n> pergunta": "Sou do coletivo X.",
		"Answer here\n\n-----Original Message-----\nFrom: x":                                                          "Answer here",
		"line one\n> quoted\nline two":                                 "line one\nline two",
		"Resposta\n\nDe: bolha.us <aprova@bolha.us>\nEnviado: segunda": "Resposta",
	} {
		if got := stripQuoted(in); got != want {
			t.Errorf("%q: got %q", in, got)
		}
	}
}

func TestHTMLOnlyAndLatin1(t *testing.T) {
	raw := "Message-ID: <x1@ex>\r\nFrom: a@b.c\r\nContent-Type: text/html; charset=iso-8859-1\r\n\r\n<p>Ol\xe1, sou eu<br>de novo</p><style>p{}</style>"
	in, err := parseInbound(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if in.Body != "Olá, sou eu\nde novo" {
		t.Errorf("body: %q", in.Body)
	}
}

func bounceDSN(questionID, recipient string) string {
	return "From: Mail Delivery System <MAILER-DAEMON@mail.example.com>\r\n" +
		"To: <aprova@bolha.us>\r\n" +
		"Message-ID: <dsn-abc@mail.example.com>\r\n" +
		"Subject: Undelivered Mail Returned to Sender\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"B\"\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=us-ascii\r\n\r\n" +
		"This is the mail system. Delivery failed.\r\n\r\n" +
		"--B\r\nContent-Type: message/delivery-status\r\n\r\n" +
		"Reporting-MTA: dns; mail.example.com\r\n\r\n" +
		"Final-Recipient: rfc822; " + recipient + "\r\n" +
		"Action: failed\r\n" +
		"Status: 5.1.1\r\n" +
		"Diagnostic-Code: smtp; 550 5.1.1 <" + recipient + ">: User unknown\r\n\r\n" +
		"--B\r\nContent-Type: message/rfc822\r\n\r\n" +
		"Message-ID: <" + questionID + ">\r\n" +
		"To: " + recipient + "\r\n" +
		"From: aprova@bolha.us\r\n" +
		"Subject: your request\r\n\r\n" +
		"Which collective are you part of?\r\n" +
		"--B--\r\n"
}

func TestParseBounce(t *testing.T) {
	in, err := parseInbound(strings.NewReader(bounceDSN("aprova.q123@bolha.us", "nouser@example.org")))
	if err != nil {
		t.Fatal(err)
	}
	if !in.IsBounce || in.BounceOf != "aprova.q123@bolha.us" || in.BounceRecipient != "nouser@example.org" {
		t.Errorf("bounce: %+v", in)
	}
	// A normal reply is not a bounce.
	ok, _ := parseInbound(strings.NewReader("Message-ID: <r@ex>\r\nFrom: gil@example.org\r\nReferences: <aprova.q@bolha.us>\r\n\r\nhi"))
	if ok.IsBounce {
		t.Errorf("reply flagged as bounce: %+v", ok)
	}
	// A delayed (not failed) DSN is not a bounce.
	delayed := strings.Replace(bounceDSN("aprova.q@bolha.us", "slow@example.org"), "Action: failed", "Action: delayed", 1)
	if d, _ := parseInbound(strings.NewReader(delayed)); d.IsBounce {
		t.Errorf("delayed DSN flagged as bounce: %+v", d)
	}
}

type fakeInbox struct {
	mu   sync.Mutex
	msgs []Inbound
	done map[uint32]bool
}

func (f *fakeInbox) Unread(context.Context) ([]Inbound, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Inbound
	for _, m := range f.msgs {
		if !f.done[m.UID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeInbox) Done(_ context.Context, uid uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done[uid] = true
	return nil
}

func TestAnswersComeFromTheMailbox(t *testing.T) {
	w := newWorld(t)
	inbox := &fakeInbox{done: map[uint32]bool{}}
	w.srv.inbox = inbox
	id := uniq()
	w.bolha.add(id, "gil", "gil@example.org", "photos")
	p1 := w.login("p1")
	p1.review("bolha-us", id, "approve", "")
	p1.review("bolha-us", id, "info", "Which city?")
	sent := w.mail.sent[len(w.mail.sent)-1]
	if !strings.HasPrefix(sent.MessageID, "aprova.") || !strings.Contains(sent.MessageID, "@") {
		t.Fatalf("question has no Message-ID: %+v", sent)
	}
	ctx := context.Background()
	inbox.msgs = []Inbound{
		// someone else quoting the right Message-ID: ignored
		{UID: 1, MessageID: "m1@ex", References: []string{sent.MessageID}, From: "intruder@example.org", Body: "approve me"},
		// the applicant's address, forged, without our Message-ID: ignored
		{UID: 3, MessageID: "m3@ex", From: "gil@example.org", Body: "forged"},
		{UID: 4, MessageID: "m4@ex", References: []string{"other@ex"}, From: "gil@example.org", Body: "forged too"},
		// the applicant
		{UID: 2, MessageID: "m2@ex", References: []string{sent.MessageID}, From: "gil@example.org", Body: "Recife."},
	}
	w.srv.readReplies(ctx)
	w.srv.readReplies(ctx) // a second replica, or the next minute: nothing twice
	inbox.done = map[uint32]bool{}
	w.srv.readReplies(ctx) // the same message seen again (flag lost): still once
	list, _ := w.srv.db.comments(ctx, "bolha-us", id)
	answers := 0
	for _, c := range list {
		if c.Kind == kindInfoAnswer {
			answers++
			if c.Body != "Recife." || c.Author != "" {
				t.Errorf("answer: %+v", c)
			}
		}
		if strings.Contains(c.Body, "approve me") || strings.Contains(c.Body, "forged") {
			t.Error("took a message from someone else")
		}
	}
	if answers != 1 {
		t.Errorf("%d answers", answers)
	}
	st, _ := w.srv.db.state(ctx, "bolha-us", id)
	if st.AwaitingSince != nil || st.Round != 2 {
		t.Errorf("after the answer: %+v", st)
	}
	// It shows on the page as the applicant's.
	if _, _, body := p1.do("GET", "/r/bolha-us/"+id, nil); !strings.Contains(body, "Recife.") {
		t.Error("answer not on the page")
	}
}

// askQuestion drives one info question on jornalismo (auth: token) and returns
// the request id and the question's Message-ID, so a bounce can be matched.
func askQuestion(t *testing.T, w *world, email string) (string, string) {
	t.Helper()
	id := uniq()
	w.jorn.add(id, "no", email, "a reason")
	p1 := w.login("p1")
	p1.review("jornalismo", id, "info", "Which paper?")
	if len(w.mail.sent) == 0 {
		t.Fatal("no question sent")
	}
	mid := w.mail.sent[len(w.mail.sent)-1].MessageID
	if !strings.HasPrefix(mid, "aprova.") {
		t.Fatalf("question has no Message-ID: %q", mid)
	}
	return id, mid
}

func TestBounceFlagsRequest(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	id, mid := askQuestion(t, w, "nouser@example.org")
	bounce := &fakeInbox{done: map[uint32]bool{}, msgs: []Inbound{
		{UID: 1, MessageID: "dsn1@mx", IsBounce: true, BounceOf: mid, BounceRecipient: "nouser@example.org"},
	}}
	w.srv.bounce = bounce
	w.srv.readBounces(ctx)
	w.srv.readBounces(ctx) // a second replica, or next minute: still once

	st, _ := w.srv.db.state(ctx, "jornalismo", id)
	if !st.Undeliverable {
		t.Error("not flagged undeliverable")
	}
	if st.AwaitingSince != nil {
		t.Error("still awaiting after bounce")
	}
	if st.Decision != "" {
		t.Errorf("rejected without the toggle: %q", st.Decision)
	}
	if _, rej := w.jorn.state(id); rej != "" {
		t.Errorf("module reject without the toggle: %q", rej)
	}
	list, _ := w.srv.db.comments(ctx, "jornalismo", id)
	n := 0
	for _, c := range list {
		if c.Kind == kindBounce {
			n++
			if !strings.Contains(c.Body, "nouser@example.org") || c.Author != "" {
				t.Errorf("bounce comment: %+v", c)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d bounce comments", n)
	}
}

func TestBounceAutoRejects(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.srv.cfg.AutoRejectOnBounce = true
	id, mid := askQuestion(t, w, "nouser2@example.org")
	bounce := &fakeInbox{done: map[uint32]bool{}, msgs: []Inbound{
		{UID: 1, MessageID: "dsn2@mx", IsBounce: true, BounceOf: mid, BounceRecipient: "nouser2@example.org"},
	}}
	w.srv.bounce = bounce
	w.srv.readBounces(ctx)

	st, _ := w.srv.db.state(ctx, "jornalismo", id)
	if st.Decision != "rejected" {
		t.Errorf("not rejected: %q", st.Decision)
	}
	if !st.Undeliverable {
		t.Error("not flagged undeliverable")
	}
	if _, rej := w.jorn.state(id); rej == "" {
		t.Error("the module was not told to reject")
	}
}
