package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/jackc/pgx/v5"
)

// Inbound is one e-mail in a mailbox aprova reads (the reply-to mailbox, or the
// sender mailbox for bounces).
type Inbound struct {
	UID        uint32
	MessageID  string
	References []string // In-Reply-To and References, message ids without <>
	From       string   // bare address, lower case
	Body       string   // the new text, without the quoted message

	// Set for a bounce (a delivery-status notification reporting a failure).
	IsBounce        bool
	BounceOf        string // Message-ID of the original question, without <>
	BounceRecipient string // the address that failed, lower case
}

// Inbox reads the mailbox applicants answer to. Done marks a message handled,
// so it isn't read again.
type Inbox interface {
	Unread(ctx context.Context) ([]Inbound, error)
	Done(ctx context.Context, uid uint32) error
}

const maxInbound = 1 << 20 // bigger messages are left for a human

type imapInbox struct {
	host, port, user, pass string
}

func (b *imapInbox) dial(ctx context.Context) (*client.Client, error) {
	d := &net.Dialer{Timeout: 15 * time.Second}
	c, err := client.DialWithDialerTLS(d, net.JoinHostPort(b.host, b.port), &tls.Config{ServerName: b.host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, err
	}
	c.Timeout = time.Minute
	if err := c.Login(b.user, b.pass); err != nil {
		c.Logout()
		return nil, err
	}
	if _, err := c.Select("INBOX", false); err != nil {
		c.Logout()
		return nil, err
	}
	return c, nil
}

func (b *imapInbox) Unread(ctx context.Context) ([]Inbound, error) {
	c, err := b.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Logout()
	crit := imap.NewSearchCriteria()
	crit.WithoutFlags = []string{imap.SeenFlag}
	uids, err := c.UidSearch(crit)
	if err != nil || len(uids) == 0 {
		return nil, err
	}
	if len(uids) > 200 {
		uids = uids[:200]
	}
	// Sizes first: a message too big to read is marked seen and left to the
	// team's copy, so it can't hold a place in the window forever.
	sizes := map[uint32]uint32{}
	{
		all := new(imap.SeqSet)
		all.AddNum(uids...)
		ch := make(chan *imap.Message, 16)
		done := make(chan error, 1)
		go func() { done <- c.UidFetch(all, []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size}, ch) }()
		for m := range ch {
			sizes[m.Uid] = m.Size
		}
		if err := <-done; err != nil {
			return nil, err
		}
	}
	big, small := new(imap.SeqSet), []uint32{}
	for _, u := range uids {
		if sizes[u] > maxInbound {
			log.Printf("inbox: uid %d is %d bytes, left for a human", u, sizes[u])
			big.AddNum(u)
		} else {
			small = append(small, u)
		}
	}
	if !big.Empty() {
		if err := c.UidStore(big, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil); err != nil {
			return nil, err
		}
	}
	if len(small) == 0 {
		return nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(small...)
	section := &imap.BodySectionName{Peek: true}
	ch := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size, section.FetchItem()}, ch)
	}()
	var out []Inbound
	for m := range ch {
		if m.Size > maxInbound {
			log.Printf("inbox: uid %d is %d bytes, left for a human", m.Uid, m.Size)
			continue
		}
		r := m.GetBody(section)
		if r == nil {
			continue
		}
		in, err := parseInbound(io.LimitReader(r, maxInbound))
		if err != nil {
			log.Printf("inbox: uid %d: %v", m.Uid, err)
			in = Inbound{}
		}
		in.UID = m.Uid
		out = append(out, in)
	}
	return out, <-done
}

func (b *imapInbox) Done(ctx context.Context, uid uint32) error {
	c, err := b.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Logout()
	set := new(imap.SeqSet)
	set.AddNum(uid)
	return c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag}, nil)
}

var msgID = regexp.MustCompile(`<([^<>\s]{1,250})>`)

func parseInbound(r io.Reader) (Inbound, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxInbound))
	if err != nil {
		return Inbound{}, err
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Inbound{}, err
	}
	in := Inbound{}
	if ids := msgID.FindStringSubmatch(m.Header.Get("Message-Id")); ids != nil {
		in.MessageID = dropNUL(ids[1])
	}
	for _, h := range []string{"In-Reply-To", "References"} {
		for _, ids := range msgID.FindAllStringSubmatch(m.Header.Get(h), 20) {
			in.References = append(in.References, dropNUL(ids[1]))
		}
	}
	if a, err := mail.ParseAddress(m.Header.Get("From")); err == nil {
		in.From = dropNUL(strings.ToLower(a.Address))
	}
	// A bounce is a delivery-status notification: multipart/report with
	// report-type=delivery-status (RFC 3464), usually from mailer-daemon.
	if isDSN(m.Header.Get("Content-Type"), in.From) {
		fillBounce(&in, raw)
	}
	body, err := textOf(m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), m.Body, 0)
	if err != nil {
		return in, err
	}
	in.Body = dropNUL(stripQuoted(body))
	return in, nil
}

// isDSN recognises a delivery-status notification: a multipart/report of
// report-type delivery-status, or a message from the mailer-daemon/postmaster.
func isDSN(ctype, from string) bool {
	if strings.HasPrefix(from, "mailer-daemon@") || strings.HasPrefix(from, "postmaster@") {
		return true
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		return false
	}
	return mt == "multipart/report" && strings.EqualFold(params["report-type"], "delivery-status")
}

// fillBounce walks a DSN for a failed delivery: the per-recipient
// message/delivery-status part gives the Action and Final-Recipient; the
// returned original message (message/rfc822 or text/rfc822-headers) gives the
// question's Message-ID. IsBounce is set only when some recipient Action is
// "failed". A fallback uses In-Reply-To/References when the original Message-ID
// is not embedded.
func fillBounce(in *Inbound, raw []byte) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return
	}
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		return
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		pmt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		switch pmt {
		case "message/delivery-status":
			b, _ := io.ReadAll(io.LimitReader(p, maxInbound))
			parseDeliveryStatus(in, string(b))
		case "message/rfc822", "text/rfc822-headers":
			b, _ := io.ReadAll(io.LimitReader(p, maxInbound))
			if om, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(b))); err == nil {
				if ids := msgID.FindStringSubmatch(om.Header.Get("Message-Id")); ids != nil {
					in.BounceOf = dropNUL(ids[1])
				}
				if in.BounceRecipient == "" {
					if a, err := mail.ParseAddress(om.Header.Get("To")); err == nil {
						in.BounceRecipient = dropNUL(strings.ToLower(a.Address))
					}
				}
			}
		}
	}
	if in.BounceOf == "" && len(in.References) > 0 {
		in.BounceOf = in.References[0]
	}
}

var dsnAddr = regexp.MustCompile(`(?i)rfc822;\s*<?([^\s<>]+@[^\s<>]+?)>?\s*$`)

// parseDeliveryStatus reads the per-recipient fields of a message/delivery-status
// body. It sets IsBounce when a recipient's Action is "failed", and records the
// failed Final-Recipient (or Original-Recipient) address.
func parseDeliveryStatus(in *Inbound, body string) {
	var failed bool
	var recip string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "action":
			if strings.EqualFold(v, "failed") {
				failed = true
			}
		case "final-recipient", "original-recipient":
			if m := dsnAddr.FindStringSubmatch(v); m != nil && recip == "" {
				recip = dropNUL(strings.ToLower(m[1]))
			}
		}
	}
	if failed {
		in.IsBounce = true
		if recip != "" {
			in.BounceRecipient = recip
		}
	}
}

// dropNUL removes NUL bytes. A U+0000 is valid UTF-8 but the database runs in
// simple-protocol mode, where any argument carrying a NUL makes PostgreSQL
// reject the whole statement ("invalid message format"). An inbound message
// with a NUL in its headers or body would then fail every takeReply, never be
// marked handled, and be re-read on each sweep until it fills the read window
// and blocks all later replies. NUL never carries meaning in a reply, so drop it.
func dropNUL(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// textOf finds the readable text of a message: text/plain if there is one,
// else text/html without its tags.
func textOf(ctype, cte string, body io.Reader, depth int) (string, error) {
	if depth > 5 {
		return "", errors.New("parts nested too deep")
	}
	if ctype == "" {
		ctype = "text/plain"
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		var html string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			t, err := textOf(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1)
			if err != nil {
				continue
			}
			pt, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			if pt == "text/html" {
				html = t
				continue
			}
			if strings.TrimSpace(t) != "" {
				return t, nil
			}
		}
		return html, nil
	}
	if mt != "text/plain" && mt != "text/html" {
		return "", errors.New("not text")
	}
	raw, err := io.ReadAll(decodeCTE(cte, body))
	if err != nil {
		return "", err
	}
	s := toUTF8(raw, params["charset"])
	if mt == "text/html" {
		s = htmlText(s)
	}
	return s, nil
}

func decodeCTE(cte string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &newlineStripper{r: r})
	}
	return r
}

type newlineStripper struct{ r io.Reader }

func (n *newlineStripper) Read(p []byte) (int, error) {
	k, err := n.r.Read(p)
	j := 0
	for _, c := range p[:k] {
		if c != '\r' && c != '\n' && c != ' ' {
			p[j] = c
			j++
		}
	}
	return j, err
}

// toUTF8 handles UTF-8 and the Latin-1 family, which covers most mail here.
func toUTF8(b []byte, charset string) string {
	cs := strings.ToLower(charset)
	if utf8.Valid(b) && (cs == "" || cs == "utf-8" || cs == "us-ascii") {
		return string(b)
	}
	if cs == "iso-8859-1" || cs == "latin1" || cs == "windows-1252" || cs == "iso-8859-15" || !utf8.Valid(b) {
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r)
	}
	return string(b)
}

var (
	htmlBreak = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr)[^>]*>`)
	htmlTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlDrop  = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)>`)
)

func htmlText(s string) string {
	s = htmlDrop.ReplaceAllString(s, "")
	s = htmlBreak.ReplaceAllString(s, "\n")
	s = htmlTag.ReplaceAllString(s, "")
	r := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
	return r.Replace(s)
}

// Where a reply starts quoting the message it answers.
var quoteStart = regexp.MustCompile(`(?i)^(on .+ wrote:|em .+ escreveu:|el .+ escribió:|le .+ a écrit\s?:|-{2,} ?original message ?-{2,}|-{2,} ?mensagem original ?-{2,}|from: .+|de: .+|_{10,})\s*$`)

func stripQuoted(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var keep []string
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		t := strings.TrimSpace(l)
		// Gmail wraps "On ... wrote:" over two lines.
		if i+1 < len(lines) && quoteStart.MatchString(t+" "+strings.TrimSpace(lines[i+1])) && !quoteStart.MatchString(t) {
			break
		}
		if quoteStart.MatchString(t) {
			break
		}
		if strings.HasPrefix(t, ">") {
			continue
		}
		keep = append(keep, strings.TrimRight(l, " \t"))
	}
	out := strings.TrimSpace(strings.Join(keep, "\n"))
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	if utf8.RuneCountInString(out) > maxText {
		out = string([]rune(out)[:maxText])
	}
	return out
}

// emailHash keys a question by the applicant's address without keeping it.
func emailHash(e string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(e))))
	return hex.EncodeToString(sum[:])
}

// readReplies attaches the applicants' answers to their requests. A message
// counts only if it references one of our questions' Message-ID and comes from
// the address that question went to. Each message is handled once, whatever
// the number of replicas; everything else is left to the team's copy.
func (s *Server) readReplies(ctx context.Context) {
	if s.inbox == nil {
		return
	}
	msgs, err := s.inbox.Unread(ctx)
	if err != nil {
		log.Printf("inbox: %v", err)
		return
	}
	for _, m := range msgs {
		if err := s.takeReply(ctx, m); err != nil {
			log.Printf("inbox: uid %d: %v", m.UID, err)
			continue
		}
		if err := s.inbox.Done(ctx, m.UID); err != nil {
			log.Printf("inbox: mark uid %d: %v", m.UID, err)
		}
	}
}

func (s *Server) takeReply(ctx context.Context, m Inbound) error {
	if m.MessageID == "" || m.From == "" {
		return nil // not something to attach; a human sees the copy
	}
	from := emailHash(m.From)
	// The sender must be the applicant asked (the hash taken when asking), and
	// the message must answer one of our questions.
	// Both are required: the From header alone is easy to forge, and the
	// question's Message-ID is random and only went to the applicant.
	if len(m.References) == 0 {
		return nil
	}
	var service, requestID, username string
	var round int
	err := s.db.pool.QueryRow(ctx, `SELECT service, request_id, username, round FROM asked
		WHERE message_id = ANY($1) AND email_hash = $2
		ORDER BY at DESC LIMIT 1`, m.References, from).Scan(&service, &requestID, &username, &round)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	svc, ok := s.services[service]
	if !ok {
		return nil
	}
	body := m.Body
	if body == "" {
		body = "(" + strs(s.cfg.Lang)["empty_reply"] + ")"
	}
	return s.db.withRequest(ctx, svc.id, requestID, username, func(tx pgx.Tx, st reqState) error {
		tag, err := tx.Exec(ctx, `INSERT INTO inbound (message_id) VALUES ($1) ON CONFLICT DO NOTHING`, m.MessageID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 || st.Decision != "" {
			return nil
		}
		if err := addComment(ctx, tx, st, "", "", kindInfoAnswer, body); err != nil {
			return err
		}
		// Only an answer to this round's question moves the request on; a late
		// answer to an older question is kept as a comment.
		if st.AwaitingSince != nil && round == st.Round {
			if err := setAwaiting(ctx, tx, st, false); err != nil {
				return err
			}
			if *svc.profile.Review.ResetOnNewInfo {
				if err := newRound(ctx, tx, st); err != nil {
					return err
				}
			}
		}
		return audit(ctx, tx, auditEntry{Profile: svc.profile.id, Service: svc.id,
			RequestID: requestID, Username: username, Action: "answer_mail", Result: "ok"})
	})
}

// readBounces flags requests whose question e-mail bounced. The bounce mailbox
// is the sender account (SMTP user), where delivery-status notifications return;
// it is read like the reply mailbox, once a minute.
func (s *Server) readBounces(ctx context.Context) {
	if s.bounce == nil {
		return
	}
	msgs, err := s.bounce.Unread(ctx)
	if err != nil {
		log.Printf("bounce: %v", err)
		return
	}
	for _, m := range msgs {
		if err := s.takeBounce(ctx, m); err != nil {
			log.Printf("bounce: uid %d: %v", m.UID, err)
			continue
		}
		if err := s.bounce.Done(ctx, m.UID); err != nil {
			log.Printf("bounce: mark uid %d: %v", m.UID, err)
		}
	}
}

func (s *Server) takeBounce(ctx context.Context, m Inbound) error {
	if !m.IsBounce || m.BounceOf == "" {
		return nil // not a failure we can match; a human sees the copy
	}
	// The bounced question's Message-ID is random and only ever went out on one
	// request, so it alone identifies the request.
	var service, requestID, username string
	var round int
	err := s.db.pool.QueryRow(ctx, `SELECT service, request_id, username, round FROM asked
		WHERE message_id = $1`, m.BounceOf).Scan(&service, &requestID, &username, &round)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	svc, ok := s.services[service]
	if !ok {
		return nil
	}
	recip := m.BounceRecipient
	note := strs(s.cfg.Lang)["bounce_note"]
	if recip != "" {
		note = recip + " — " + note
	}
	return s.db.withRequest(ctx, svc.id, requestID, username, func(tx pgx.Tx, st reqState) error {
		// Dedup so two replicas, or a re-read, act once. The DSN's own Message-ID
		// is unique; some lack one, so fall back to a per-question key.
		key := m.MessageID
		if key == "" {
			key = "bounce:" + m.BounceOf
		}
		tag, err := tx.Exec(ctx, `INSERT INTO inbound (message_id) VALUES ($1) ON CONFLICT DO NOTHING`, key)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 || st.Decision != "" {
			return nil
		}
		if err := addComment(ctx, tx, st, "", "", kindBounce, note); err != nil {
			return err
		}
		if err := setAwaiting(ctx, tx, st, false); err != nil {
			return err
		}
		action, result := "bounce", "ok"
		// Auto-reject only when turned on and the service acts with its own token:
		// an auth: moderator service has no system token, so it only gets flagged.
		if s.cfg.AutoRejectOnBounce && svc.cfg.Auth != "moderator" {
			res, err := s.execute(ctx, tx, svc, session{}, st, "reject")
			if err != nil {
				return err // transient: left unread, retried next minute
			}
			if err := addComment(ctx, tx, st, "", "", kindRejectReason, note); err != nil {
				return err
			}
			action, result = "reject_bounce", res
		}
		return audit(ctx, tx, auditEntry{Profile: svc.profile.id, Service: svc.id,
			RequestID: requestID, Username: username, Action: action, Result: result})
	})
}

func newMessageID(from string) string {
	host := "aprova.local"
	if a, err := mail.ParseAddress(from); err == nil {
		if _, d, ok := strings.Cut(a.Address, "@"); ok {
			host = d
		}
	}
	return fmt.Sprintf("aprova.%s@%s", randomToken()[:24], host)
}
