package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type store struct {
	pool *pgxpool.Pool
}

func openStore(ctx context.Context, c *Config) (*store, error) {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.DBUser, c.DBPassword),
		Host:     net.JoinHostPort(c.DBHost, c.DBPort),
		Path:     "/" + c.DBName,
		RawQuery: url.Values{"sslmode": {c.DBSSLMode}}.Encode(),
	}
	cfg, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("database settings: %w", err)
	}
	cfg.MaxConns = 4
	// Simple protocol, so it also works behind pgbouncer in transaction mode.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	s := &store{pool: pool}
	var n int
	err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sessions WHERE false) + (SELECT count(*) FROM requests WHERE false)
		+ (SELECT count(*) FROM reviews WHERE false) + (SELECT count(*) FROM comments WHERE false)
		+ (SELECT count(*) FROM audit_log WHERE false) + (SELECT count(*) FROM digests WHERE false)
		+ (SELECT count(*) FROM prefs WHERE false)`).Scan(&n)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w (was schema.sql applied?)", err)
	}
	return s, nil
}

// ---- sessions

type session struct {
	IDHash      string
	AccountID   string
	Acct        string
	Role        string
	RoleChecked time.Time
	Token       string // decrypted
	TokenHash   string
	CSRF        string
	Expires     time.Time
}

func (s *store) createSession(ctx context.Context, b *box, sess session) error {
	sealed := b.seal([]byte(sess.Token), []byte(sess.IDHash))
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (id_hash, account_id, acct, role, role_checked, token, token_hash, csrf, expires)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		sess.IDHash, sess.AccountID, sess.Acct, sess.Role, sess.RoleChecked, sealed, sess.TokenHash, sess.CSRF, sess.Expires)
	return err
}

var errNoSession = errors.New("no session")

func (s *store) session(ctx context.Context, b *box, idHash string) (session, error) {
	sess := session{IDHash: idHash}
	var sealed []byte
	err := s.pool.QueryRow(ctx, `SELECT account_id, acct, role, role_checked, token, token_hash, csrf, expires FROM sessions WHERE id_hash = $1`, idHash).
		Scan(&sess.AccountID, &sess.Acct, &sess.Role, &sess.RoleChecked, &sealed, &sess.TokenHash, &sess.CSRF, &sess.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return sess, errNoSession
	}
	if err != nil {
		return sess, err
	}
	if time.Now().After(sess.Expires) {
		return sess, errNoSession
	}
	token, err := b.open(sealed, []byte(idHash))
	if err != nil {
		return sess, errNoSession // encrypted with another key
	}
	sess.Token = string(token)
	return sess, nil
}

func (s *store) updateRole(ctx context.Context, idHash, role string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET role = $2, role_checked = now() WHERE id_hash = $1`, idHash, role)
	return err
}

func (s *store) expireSession(ctx context.Context, idHash string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET expires = now() - interval '1 second' WHERE id_hash = $1`, idHash)
	return err
}

// finishSessions revokes the tokens of expired sessions and forgets them. A token
// still used by a live session is kept. A failed revoke is tried again later.
func (s *store) finishSessions(ctx context.Context, b *box, revoke func(string) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id_hash, token, token_hash FROM sessions WHERE expires < now()
		ORDER BY expires LIMIT 50 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	type dead struct {
		id, hash string
		sealed   []byte
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dead, error) {
		var d dead
		return d, r.Scan(&d.id, &d.sealed, &d.hash)
	})
	if err != nil {
		return err
	}
	for _, d := range list {
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE token_hash = $1 AND expires >= now()`, d.hash).Scan(&live); err != nil {
			return err
		}
		if live == 0 {
			if token, err := b.open(d.sealed, []byte(d.id)); err == nil {
				if err := revoke(string(token)); err != nil {
					continue // keep the row, try again on the next sweep
				}
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id_hash = $1`, d.id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---- request state

type reqState struct {
	Service       string
	RequestID     string
	Username      string
	Round         int
	Escalated     bool
	AwaitingSince *time.Time
	Decision      string
	DecidedBy     string
	DecidedAt     *time.Time
}

func (s *store) state(ctx context.Context, service, id string) (reqState, error) {
	st := reqState{Service: service, RequestID: id, Round: 1}
	err := s.pool.QueryRow(ctx, `SELECT username, round, escalated, awaiting_since, decision, decided_by, decided_at
		FROM requests WHERE service = $1 AND request_id = $2`, service, id).
		Scan(&st.Username, &st.Round, &st.Escalated, &st.AwaitingSince, &st.Decision, &st.DecidedBy, &st.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	return st, err
}

// states loads many at once, for the queue.
func (s *store) states(ctx context.Context, service string, ids []string) (map[string]reqState, error) {
	out := map[string]reqState{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT request_id, username, round, escalated, awaiting_since, decision
		FROM requests WHERE service = $1 AND request_id = ANY($2)`, service, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		st := reqState{Service: service}
		if err := rows.Scan(&st.RequestID, &st.Username, &st.Round, &st.Escalated, &st.AwaitingSince, &st.Decision); err != nil {
			return nil, err
		}
		out[st.RequestID] = st
	}
	return out, rows.Err()
}

func (s *store) ensure(ctx context.Context, q pgx.Tx, service, id, username string) error {
	_, err := q.Exec(ctx, `INSERT INTO requests (service, request_id, username) VALUES ($1, $2, $3)
		ON CONFLICT (service, request_id) DO UPDATE SET username = EXCLUDED.username WHERE requests.username = ''`, service, id, username)
	return err
}

// withRequest runs fn with the request's row locked, so two moderators or two
// replicas never act on the same request at once.
func (s *store) withRequest(ctx context.Context, service, id, username string, fn func(tx pgx.Tx, st reqState) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.ensure(ctx, tx, service, id, username); err != nil {
		return err
	}
	st := reqState{Service: service, RequestID: id}
	if err := tx.QueryRow(ctx, `SELECT username, round, escalated, awaiting_since, decision, decided_by, decided_at
		FROM requests WHERE service = $1 AND request_id = $2 FOR UPDATE`, service, id).
		Scan(&st.Username, &st.Round, &st.Escalated, &st.AwaitingSince, &st.Decision, &st.DecidedBy, &st.DecidedAt); err != nil {
		return err
	}
	if err := fn(tx, st); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func setDecision(ctx context.Context, tx pgx.Tx, st reqState, decision, by string) error {
	_, err := tx.Exec(ctx, `UPDATE requests SET decision = $3, decided_by = $4, decided_at = now(), updated_at = now()
		WHERE service = $1 AND request_id = $2`, st.Service, st.RequestID, decision, by)
	return err
}

func setEscalated(ctx context.Context, tx pgx.Tx, st reqState, on bool) error {
	_, err := tx.Exec(ctx, `UPDATE requests SET escalated = $3, updated_at = now() WHERE service = $1 AND request_id = $2`, st.Service, st.RequestID, on)
	return err
}

func setAwaiting(ctx context.Context, tx pgx.Tx, st reqState, on bool) error {
	_, err := tx.Exec(ctx, `UPDATE requests SET awaiting_since = CASE WHEN $3 THEN now() ELSE NULL END, updated_at = now()
		WHERE service = $1 AND request_id = $2`, st.Service, st.RequestID, on)
	return err
}

func newRound(ctx context.Context, tx pgx.Tx, st reqState) error {
	_, err := tx.Exec(ctx, `UPDATE requests SET round = round + 1, updated_at = now() WHERE service = $1 AND request_id = $2`, st.Service, st.RequestID)
	return err
}

// ---- reviews and comments

func putReview(ctx context.Context, tx pgx.Tx, st reqState, r review) error {
	_, err := tx.Exec(ctx, `INSERT INTO reviews (service, request_id, round, reviewer_id, reviewer_acct, reviewer_role, verdict)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (service, request_id, round, reviewer_id) DO UPDATE SET verdict = EXCLUDED.verdict, reviewer_role = EXCLUDED.reviewer_role, at = now()`,
		st.Service, st.RequestID, st.Round, r.ReviewerID, r.ReviewerAcct, r.ReviewerRole, r.Verdict)
	return err
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func reviewsOf(ctx context.Context, q querier, service, id string, round int) ([]review, error) {
	rows, err := q.Query(ctx, `SELECT reviewer_id, reviewer_acct, reviewer_role, verdict FROM reviews
		WHERE service = $1 AND request_id = $2 AND round = $3 ORDER BY at`, service, id, round)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (review, error) {
		var v review
		return v, r.Scan(&v.ReviewerID, &v.ReviewerAcct, &v.ReviewerRole, &v.Verdict)
	})
}

// reviewCounts gives approvals and rejects of the current round per request, for the queue.
func (s *store) reviewCounts(ctx context.Context, service string, rounds map[string]int) (map[string][2]int, error) {
	out := map[string][2]int{}
	if len(rounds) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rounds))
	for id := range rounds {
		ids = append(ids, id)
	}
	rows, err := s.pool.Query(ctx, `SELECT request_id, round, verdict, count(*) FROM reviews
		WHERE service = $1 AND request_id = ANY($2) GROUP BY request_id, round, verdict`, service, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, verdict string
		var round, n int
		if err := rows.Scan(&id, &round, &verdict, &n); err != nil {
			return nil, err
		}
		if round != rounds[id] {
			continue
		}
		c := out[id]
		switch verdict {
		case verdictApprove:
			c[0] += n
		case verdictReject:
			c[1] += n
		}
		out[id] = c
	}
	return out, rows.Err()
}

type comment struct {
	Round  int       `json:"round"`
	Author string    `json:"author"`
	Kind   string    `json:"kind"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
}

// Comment kinds. The applicant never sees any of them.
const (
	kindComment      = "comment"
	kindReview       = "review"
	kindEscalate     = "escalate"
	kindDeescalate   = "deescalate"
	kindInfoQuestion = "info_question"
	kindInfoAnswer   = "info_answer"
	kindRejectReason = "reject_reason"
	kindApproveNote  = "approve_note"
	kindSystem       = "system"
)

func addComment(ctx context.Context, tx pgx.Tx, st reqState, authorID, authorAcct, kind, body string) error {
	_, err := tx.Exec(ctx, `INSERT INTO comments (service, request_id, round, author_id, author_acct, kind, body)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, st.Service, st.RequestID, st.Round, authorID, authorAcct, kind, body)
	return err
}

func (s *store) comments(ctx context.Context, service, id string) ([]comment, error) {
	rows, err := s.pool.Query(ctx, `SELECT round, author_acct, kind, body, at FROM comments
		WHERE service = $1 AND request_id = $2 ORDER BY id`, service, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (comment, error) {
		var c comment
		return c, r.Scan(&c.Round, &c.Author, &c.Kind, &c.Body, &c.At)
	})
}

// ---- preferences

func (s *store) dmOff(ctx context.Context, accts []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(accts) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT acct FROM prefs WHERE dm_off AND acct = ANY($1)`, accts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}

func (s *store) setDMOff(ctx context.Context, acct string, off bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO prefs (acct, dm_off) VALUES ($1, $2)
		ON CONFLICT (acct) DO UPDATE SET dm_off = $2, updated = now()`, strings.ToLower(acct), off)
	return err
}

// ---- audit

type auditEntry struct {
	ActorID   string    `json:"-"`
	ActorAcct string    `json:"actor_acct"`
	Profile   string    `json:"profile"`
	Service   string    `json:"service"`
	RequestID string    `json:"request_id"`
	Username  string    `json:"username"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	Result    string    `json:"result"`
	At        time.Time `json:"at"`
}

func audit(ctx context.Context, tx pgx.Tx, e auditEntry) error {
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (actor_id, actor_acct, profile, service, request_id, username, action, detail, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.ActorID, e.ActorAcct, e.Profile, e.Service, e.RequestID, e.Username, e.Action, e.Detail, e.Result)
	return err
}

func (s *store) auditLog(ctx context.Context, profiles []string, limit int) ([]auditEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT at, actor_acct, profile, service, request_id, username, action, detail, result
		FROM audit_log WHERE profile = ANY($1) ORDER BY id DESC LIMIT $2`, profiles, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (auditEntry, error) {
		var e auditEntry
		return e, r.Scan(&e.At, &e.ActorAcct, &e.Profile, &e.Service, &e.RequestID, &e.Username, &e.Action, &e.Detail, &e.Result)
	})
}
