package core

import (
	"bufio"
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

// Inbound is one e-mail in the reply-to mailbox.
type Inbound struct {
	UID        uint32
	MessageID  string
	References []string // In-Reply-To and References, message ids without <>
	From       string   // bare address, lower case
	Body       string   // the new text, without the quoted message
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
	m, err := mail.ReadMessage(bufio.NewReader(r))
	if err != nil {
		return Inbound{}, err
	}
	in := Inbound{}
	if ids := msgID.FindStringSubmatch(m.Header.Get("Message-Id")); ids != nil {
		in.MessageID = ids[1]
	}
	for _, h := range []string{"In-Reply-To", "References"} {
		for _, ids := range msgID.FindAllStringSubmatch(m.Header.Get(h), 20) {
			in.References = append(in.References, ids[1])
		}
	}
	if a, err := mail.ParseAddress(m.Header.Get("From")); err == nil {
		in.From = strings.ToLower(a.Address)
	}
	body, err := textOf(m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), m.Body, 0)
	if err != nil {
		return in, err
	}
	in.Body = stripQuoted(body)
	return in, nil
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

func newMessageID(from string) string {
	host := "aprova.local"
	if a, err := mail.ParseAddress(from); err == nil {
		if _, d, ok := strings.Cut(a.Address, "@"); ok {
			host = d
		}
	}
	return fmt.Sprintf("aprova.%s@%s", randomToken()[:24], host)
}
