-- Run once as the database owner, not as the app user. See README.md.
-- The app can't alter its own schema, and can only add to the audit log.

-- Moderator sessions. The token is the moderator's own, encrypted, and revoked
-- when the session ends. token_hash finds sessions sharing one token: the
-- instance gives the same token to every sign-in of an account in one app.
CREATE TABLE IF NOT EXISTS sessions (
  id_hash      text PRIMARY KEY,
  account_id   text NOT NULL,
  acct         text NOT NULL,
  role         text NOT NULL,
  role_checked timestamptz NOT NULL,
  token        bytea NOT NULL,
  token_hash   text NOT NULL,
  csrf         text NOT NULL,
  expires      timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions (expires);
CREATE INDEX IF NOT EXISTS sessions_token ON sessions (token_hash);

-- What aprova keeps on top of a request; the request itself lives in the app.
CREATE TABLE IF NOT EXISTS requests (
  service        text NOT NULL,
  request_id     text NOT NULL,
  username       text NOT NULL DEFAULT '',
  round          integer NOT NULL DEFAULT 1, -- a new round starts when the applicant answers
  escalated      boolean NOT NULL DEFAULT false,
  awaiting_since timestamptz,                -- asked the applicant, no answer recorded yet
  decision       text NOT NULL DEFAULT '',   -- approved, rejected or gone
  decided_by     text NOT NULL DEFAULT '',
  decided_at     timestamptz,
  updated_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service, request_id)
);

-- One vote per moderator per round; the last one counts.
CREATE TABLE IF NOT EXISTS reviews (
  service       text NOT NULL,
  request_id    text NOT NULL,
  round         integer NOT NULL,
  reviewer_id   text NOT NULL,
  reviewer_acct text NOT NULL,
  reviewer_role text NOT NULL,
  verdict       text NOT NULL CHECK (verdict IN ('approve', 'reject', 'info')),
  at            timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service, request_id, round, reviewer_id)
);

-- The moderators' discussion. Never shown to the applicant.
CREATE TABLE IF NOT EXISTS comments (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  service     text NOT NULL,
  request_id  text NOT NULL,
  round       integer NOT NULL,
  author_id   text NOT NULL,
  author_acct text NOT NULL,
  kind        text NOT NULL,
  body        text NOT NULL,
  at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS comments_request ON comments (service, request_id, id);

CREATE TABLE IF NOT EXISTS audit_log (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at         timestamptz NOT NULL DEFAULT now(),
  actor_id   text NOT NULL,
  actor_acct text NOT NULL,
  profile    text NOT NULL,
  service    text NOT NULL,
  request_id text NOT NULL,
  username   text NOT NULL,
  action     text NOT NULL,
  detail     text NOT NULL DEFAULT '',
  result     text NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_log_profile ON audit_log (profile, id DESC);

-- Last digest posted per profile, so replicas don't repeat it.
CREATE TABLE IF NOT EXISTS digests (
  profile     text PRIMARY KEY,
  fingerprint text NOT NULL,
  sent_at     timestamptz NOT NULL
);

-- Moderators who turned off the bot's direct messages.
CREATE TABLE IF NOT EXISTS prefs (
  acct    text PRIMARY KEY, -- lowercase username on the sign-in instance
  dm_off  boolean NOT NULL DEFAULT false,
  updated timestamptz NOT NULL DEFAULT now()
);

-- Replace aprova with your app user if it has another name.
GRANT SELECT, INSERT, UPDATE, DELETE ON sessions, requests, reviews, digests, prefs TO aprova;
GRANT SELECT, INSERT ON comments, audit_log TO aprova;
