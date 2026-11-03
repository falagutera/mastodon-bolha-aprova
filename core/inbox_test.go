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
		if strings.Contains(c.Body, "approve me") {
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
