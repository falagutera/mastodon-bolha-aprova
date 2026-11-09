package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"gitlab.gutocarvalho.net/open/bolha/aprova/module"
)

// Digest posts, per profile with a Mattermost webhook, how many requests are
// pending, only when that changed. It runs with no one signed in, so it can only
// count services with a token of their own: auth token, or digest_token_env.
func (s *Server) Digest(ctx context.Context) {
	for _, pid := range sortedKeys(s.cfg.Profiles) {
		p := s.cfg.Profiles[pid]
		if p.mattermost == "" && p.MastodonDM == nil {
			continue
		}
		if err := s.digestProfile(ctx, p); err != nil {
			log.Printf("digest %s: %v", pid, err)
		}
	}
}

func (s *Server) digestProfile(ctx context.Context, p *Profile) error {
	t := strs(s.cfg.Lang)
	var parts, ids []string
	total := 0
	for _, sid := range p.Services {
		svc := s.services[sid]
		if svc.cfg.digestToken == "" {
			continue // auth moderator without a digest token: nothing to count with
		}
		reqs, err := svc.mod.Pending(ctx, module.Actor{Token: svc.cfg.digestToken})
		if err != nil {
			return fmt.Errorf("%s: %w", sid, err)
		}
		if len(reqs) == 0 {
			continue
		}
		total += len(reqs)
		parts = append(parts, fmt.Sprintf("%s %d", svc.cfg.Name, len(reqs)))
		for _, q := range reqs {
			ids = append(ids, sid+"/"+q.ID)
		}
	}
	msg := fmt.Sprintf(t["mm_digest"], total, p.Name, strings.Join(parts, " · "), s.cfg.BaseURL+"/queue")
	hours := p.MastodonDM != nil && len(p.MastodonDM.DigestHours) > 0
	if hours {
		if err := s.digestAtHours(ctx, p, total, msg); err != nil {
			return err
		}
		if p.mattermost == "" {
			return nil
		}
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	fp := hex.EncodeToString(sum[:])
	tx, err := s.db.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO digests (profile, fingerprint, sent_at) VALUES ($1, '', now()) ON CONFLICT DO NOTHING`, p.id); err != nil {
		return err
	}
	var last string
	// The row lock makes one replica post; the other sees the new fingerprint.
	if err := tx.QueryRow(ctx, `SELECT fingerprint FROM digests WHERE profile = $1 FOR UPDATE`, p.id).Scan(&last); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if last == fp {
		return nil
	}
	if total > 0 {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.mm.post(pctx, p.mattermost, msg)
		cancel()
		if err != nil {
			return err
		}
		if p.MastodonDM != nil && !hours {
			dg, _ := s.dmRecipients(ctx, p)
			s.dm(ctx, p, dg, msg, "digest\x00"+p.id+"\x00"+fp)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE digests SET fingerprint = $2, sent_at = now() WHERE profile = $1`, p.id, fp); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
