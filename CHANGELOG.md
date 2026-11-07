# Changelog

All notable changes to aprova. Versions follow [SemVer](https://semver.org/); images are
published as `registry.gutocarvalho.net/images/aprova:<version>-<build>`.

## 0.9.1 — 2026-11-06

- Times in the panel (requests, discussion, log) are shown in the configured `timezone` (default America/Sao_Paulo) instead of UTC; the footer says which zone.

## 0.9.0 — 2026-11-06

- Welcome e-mail (`mail.welcome`, off by default): every new request gets an e-mail right away, in Portuguese and English, asking who the person is, what they do and what they will post. It shows as the first item of the discussion, the reply is attached to the request, and a bounce flags it "e-mail not delivered". Texts configurable with `welcome_subject` / `welcome_body`.
- "How it works" page in the top menu: the automatic approval and rejection rules (three reviews), blocks, escalation and rounds, per profile, read from the running config.

## 0.8.3 — 2026-11-06

- Release notes now come from `CHANGELOG.md` in the repo (shared [relnotes](https://gitlab.gutocarvalho.net/open/gcn/relnotes) package), and are also served as `/release-notes.json`.

## 0.8.2 — 2026-11-06

- Release notes page, reached from the top menu.
- Optional team-chat link (env APROVA_TEAM_CHAT) shown in the panel to signed-in moderators only — kept out of the config file so a public repo never carries the invite.

## 0.8.0 — 2026-11-05

- Bounce detection: when the question e-mail to an applicant is undeliverable, the request is flagged "e-mail not delivered", so a fake sign-up is easy to reject. Optional auto_reject_on_bounce rejects it on its own.

## 0.7.2 — 2026-11-05

- Forgejo reject: purge only an account that never signed in; one that did is deleted without purge, so an established account that owns content is protected rather than erased.

## 0.7.1 — 2026-11-05

- Forgejo reject now deletes a pending account even if it signed in before approval — bots that log in first can finally be rejected from the panel.

## 0.7.0 — 2026-11-05

- A reject quorum, and the approval quorum raised to three.

## 0.6.4 — 2026-11-05

- Security fixes: API-token replay and NUL-byte reply poisoning.

## 0.6.3 — 2026-11-05

- Skip the reason check for apps whose sign-up form has no reason field.

## 0.6.2 — 2026-11-04

- The discussion is split by round.

## 0.6.1 — 2026-11-04

- Approve: the note is internal, required only after a reject or a failed check.

## 0.6.0 — 2026-11-04

- WriteFreely module, approving through the approval patch's registrations API.

## 0.5.0 — 2026-11-03

- Lemmy, PeerTube and Forgejo modules; BookWyrm and Friendica agent recipes.

## 0.4.0 — 2026-11-03

- Applicants' answers are read from the reply-to mailbox and attached on their own.

## 0.3.0 — 2026-11-03

- Review and question in separate boxes; authors fix their own comments.

## 0.2.0 — 2026-11-03

- Agent module and aprova-agent, with a Pixelfed recipe for apps without an API.

## 0.1.0 — 2026-11-03

- Sign-up approval panel with the Mastodon and GitLab modules.
