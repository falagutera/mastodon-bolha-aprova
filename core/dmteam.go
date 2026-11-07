package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dmRosterTTL is how long the team looked up by role is reused.
const dmRosterTTL = 30 * time.Minute

// dmRoster caches who holds the configured roles on the instance.
type dmRoster struct {
	mu                 sync.Mutex
	at                 time.Time
	digest, escalation []string
}

// dmRecipients is who gets the digest and who gets escalations: the names in the
// config plus, with role ids, every active local account holding one of those
// roles. If a lookup fails the last good list stays, so a hiccup on the
// instance doesn't silence the bot.
func (s *Server) dmRecipients(ctx context.Context, p *Profile) (digest, escalation []string) {
	d := p.MastodonDM
	if d == nil {
		return nil, nil
	}
	digest, escalation = slices.Clone(d.Recipients), slices.Clone(d.EscalationRecipients)
	if len(d.RecipientRoleIDs)+len(d.EscalationRoleIDs) == 0 {
		return digest, escalation
	}
	r := &d.roster
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.at.IsZero() || time.Since(r.at) > dmRosterTTL {
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		dg, esc, err := s.lookupRoles(lctx, d)
		cancel()
		if err != nil {
			log.Printf("dm roster %s: %v", p.id, err)
			if r.at.IsZero() {
				// Nothing good yet: try again on the next call, not in 30 minutes.
				return digest, escalation
			}
		} else {
			r.digest, r.escalation, r.at = dg, esc, time.Now()
		}
	}
	return union(digest, r.digest), union(escalation, r.escalation)
}

type adminAccount struct {
	Username string `json:"username"`
	Role     *struct {
		ID json.RawMessage `json:"id"`
	} `json:"role"`
}

// lookupRoles lists the active local accounts with any of the roles, through
// the admin API (admin:read:accounts), following the pagination on the same
// instance only.
func (s *Server) lookupRoles(ctx context.Context, d *MastodonDM) (digest, escalation []string, err error) {
	q := url.Values{"origin": {"local"}, "status": {"active"}, "limit": {"200"}}
	for _, id := range union(d.RecipientRoleIDs, d.EscalationRoleIDs) {
		q.Add("role_ids[]", strconv.Itoa(id))
	}
	next := s.cfg.Instance + "/api/v2/admin/accounts?" + q.Encode()
	base, err := url.Parse(s.cfg.Instance)
	if err != nil {
		return nil, nil, err
	}
	for page := 0; next != "" && page < 20; page++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+d.lookupToken)
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		var list []adminAccount
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, nil, fmt.Errorf("role lookup: status %d", resp.StatusCode)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("role lookup: %w", err)
		}
		for _, a := range list {
			h := strings.ToLower(a.Username)
			if a.Role == nil || !localHandle.MatchString(h) {
				continue
			}
			role, err := strconv.Atoi(strings.Trim(string(a.Role.ID), `"`))
			if err != nil {
				continue
			}
			if slices.Contains(d.RecipientRoleIDs, role) {
				digest = append(digest, h)
			}
			if slices.Contains(d.EscalationRoleIDs, role) {
				escalation = append(escalation, h)
			}
		}
		next = ""
		if u, ok := nextLink(resp.Header.Get("Link")); ok {
			if nu, err := url.Parse(u); err == nil && nu.Scheme == base.Scheme && nu.Host == base.Host {
				next = u
			}
		}
	}
	if digest == nil && escalation == nil {
		return nil, nil, errors.New("role lookup: nobody holds the roles")
	}
	return union(digest), union(escalation), nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

func nextLink(h string) (string, bool) {
	m := linkNext.FindStringSubmatch(h)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// union is the sorted set of the lists, without repeats.
func union[T interface{ ~int | ~string }](lists ...[]T) []T {
	var out []T
	for _, l := range lists {
		out = append(out, l...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// digestAtHours sends the digest DM once in each configured hour, and only when
// something is waiting. The row lock makes one replica send it.
func (s *Server) digestAtHours(ctx context.Context, p *Profile, total int, msg string) error {
	d := p.MastodonDM
	now := s.clock().In(s.cfg.loc)
	if !slices.Contains(d.DigestHours, now.Hour()) {
		return nil
	}
	slot := now.Format("2006-01-02T15")
	key := "dm:" + p.id
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO digests (profile, fingerprint, sent_at) VALUES ($1, '', now()) ON CONFLICT DO NOTHING`, key); err != nil {
		return err
	}
	var last string
	if err := tx.QueryRow(ctx, `SELECT fingerprint FROM digests WHERE profile = $1 FOR UPDATE`, key).Scan(&last); err != nil {
		return err
	}
	if last == slot {
		return nil
	}
	if total > 0 {
		dg, _ := s.dmRecipients(ctx, p)
		s.dm(ctx, p, dg, msg, "digest-slot\x00"+p.id+"\x00"+slot)
	}
	if _, err := tx.Exec(ctx, `UPDATE digests SET fingerprint = $2, sent_at = now() WHERE profile = $1`, key, slot); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
