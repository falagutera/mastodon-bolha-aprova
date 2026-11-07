package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"text/template"
	"time"
)

// Mailer sends the messages applicants receive. aprova is the only voice to
// applicants: modules reject without messaging them.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

type Message struct {
	From, To, ReplyTo, Subject, Body string
	// MessageID, without <>, lets a reply be matched to this message. Made up when empty.
	MessageID string
}

var errNoMail = errors.New("e-mail is not configured")

func (m Message) validate() error {
	for _, a := range []string{m.From, m.To} {
		if _, err := mail.ParseAddress(a); err != nil {
			return fmt.Errorf("address %q: %w", a, err)
		}
	}
	if m.ReplyTo != "" {
		if _, err := mail.ParseAddress(m.ReplyTo); err != nil {
			return fmt.Errorf("reply-to: %w", err)
		}
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("subject has a line break")
	}
	return nil
}

func (m Message) bytes(now time.Time) []byte {
	var b bytes.Buffer
	host := "aprova.local"
	if _, d, ok := strings.Cut(m.From, "@"); ok {
		host = strings.Trim(d, "> ")
	}
	to := m.To
	if a, err := mail.ParseAddress(m.To); err == nil {
		to = a.String()
	}
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\n", m.From, to)
	if m.ReplyTo != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", m.ReplyTo)
	}
	id := m.MessageID
	if id == "" {
		id = randomToken()[:24] + "@" + host
	}
	fmt.Fprintf(&b, "Subject: %s\r\nDate: %s\r\nMessage-ID: <%s>\r\n", mime.QEncoding.Encode("utf-8", m.Subject),
		now.Format(time.RFC1123Z), id)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	w := quotedprintable.NewWriter(&b)
	w.Write([]byte(strings.ReplaceAll(m.Body, "\n", "\r\n")))
	w.Close()
	return b.Bytes()
}

type smtpMailer struct {
	host, port, user, pass, mode string
}

func (s *smtpMailer) Send(ctx context.Context, m Message) error {
	if err := m.validate(); err != nil {
		return err
	}
	addr := net.JoinHostPort(s.host, s.port)
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	tlsCfg := &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
	if s.mode == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsCfg)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(time.Minute))
	}
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if s.mode == "starttls" {
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); err != nil {
			return err
		}
	}
	from, _ := mail.ParseAddress(m.From)
	to, _ := mail.ParseAddress(m.To)
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to.Address); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(m.bytes(time.Now())); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// mailData is what the templates see.
type mailData struct {
	Username string
	Service  string
	Question string
}

func render(tmpl string, d mailData) (string, error) {
	t, err := template.New("mail").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// applicantMail builds the message to an applicant from the profile's templates,
// falling back to the built-in ones.
func applicantMail(p *Profile, kind string, d mailData, to string) (Message, error) {
	t := strs(p.Mail.Lang)
	subject, body := p.Mail.RejectSubject, p.Mail.RejectBody
	switch kind {
	case "info":
		subject, body = p.Mail.InfoSubject, p.Mail.InfoBody
	case "welcome":
		subject, body = p.Mail.WelcomeSubject, p.Mail.WelcomeBody
	}
	if subject == "" {
		subject = t["mail_"+kind+"_subject"]
	}
	if body == "" {
		body = t["mail_"+kind+"_body"]
	}
	s, err := render(subject, d)
	if err != nil {
		return Message{}, err
	}
	b, err := render(body, d)
	if err != nil {
		return Message{}, err
	}
	return Message{From: p.Mail.From, To: to, ReplyTo: p.Mail.ReplyTo, Subject: strings.TrimSpace(s), Body: b}, nil
}

// directMessage posts a direct message from the bot to one local account. The
// idempotency key keeps a retry, or a second replica, from sending it twice.
func (s *Server) directMessage(ctx context.Context, token, acct, text, key string) error {
	form := url.Values{"status": {"@" + acct + " " + text}, "visibility": {"direct"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Instance+"/api/v1/statuses", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", hashID(key))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("direct message: status %d", resp.StatusCode)
	}
	return nil
}

// dm sends text to each recipient who hasn't turned the messages off.
func (s *Server) dm(ctx context.Context, p *Profile, recipients []string, text, key string) {
	if p.MastodonDM == nil || len(recipients) == 0 {
		return
	}
	off, err := s.db.dmOff(ctx, recipients)
	if err != nil {
		log.Printf("dm prefs: %v", err)
		return
	}
	for _, acct := range recipients {
		if off[acct] {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.directMessage(ctx, p.MastodonDM.token, acct, text, key+"\x00"+acct)
		cancel()
		if err != nil {
			log.Printf("dm @%s: %v", acct, err)
		}
	}
}

// mattermost posts to an incoming webhook. Messages never carry e-mails or reasons.
type mattermost struct {
	client *http.Client
}

func (m *mattermost) post(ctx context.Context, webhook, text string) error {
	if webhook == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		// The webhook URL is a secret: keep it out of the error, and so out of the logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("mattermost: %s %s: %w", ue.Op, req.URL.Host, ue.Err)
		}
		return errors.New("mattermost: request failed")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("mattermost: status %d", resp.StatusCode)
	}
	return nil
}
