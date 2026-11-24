# Changelog

All notable changes to aprova. Versions follow [SemVer](https://semver.org/); images are
published as `registry.gutocarvalho.net/images/aprova:<version>-<build>`.

## 0.19.1 — 2026-11-24

- **Lemmy: a rejected request leaves the list.** Rejecting purges the applicant, and Lemmy keeps the purged person (deleted and banned) with its application still undecided, so the request stayed in the queue after the rejection. An application whose account is deleted now counts as decided; rejecting it again does nothing.

## 0.19.0 — 2026-11-22

- **CleanTalk in the Reputation card:** `cleantalk` in `lookups.reputation` (key in `APROVA_CLEANTALK_KEY`, from the Block Lists / spam check plan) checks the IP and the e-mail's SHA-256, never the address. It adds the network type (hosting, paid VPN, public proxy, Tor) and marks throwaway e-mail services.
- **Unavailable sources never break the page:** any lookup that fails (outage, rate limit, a paid plan unpaid or expired, an unexpected answer) shows as "unavailable right now" on its card and source pill; the rest of the page and the other sources are unaffected. When a paid provider turns its key down, the log says so, at most once an hour per provider.

## 0.18.0 — 2026-11-22

- **API tokens last as long as you pick:** 1, 7, 14 or 30 days, instead of 8 hours. The token page asks first and shows the expiry date with the token. The role is still checked on the instance every minute, so signing out there or losing the role ends a token sooner.

## 0.17.0 — 2026-11-22

- **New option `lookups.reputation`:** a "Reputation" card on the request page says whether the sign-up IP and the e-mail were reported to spam and abuse databases: StopForumSpam (`sfs`, no key; the IP and the e-mail's MD5, never the address itself) and AbuseIPDB (`abuseipdb`, IP only, key in `APROVA_ABUSEIPDB_KEY`). Each line gives the number of reports, the provider's confidence, the date of the last one, and, for the IP, its use (e.g. data center) and whether it is a Tor exit. It is context, never a check: home and mobile IPs are shared, so a report may be about someone else. Sources show with the same ✓/⚠/– pills; without the key, abuseipdb shows as off.

## 0.16.1 — 2026-11-22

- The "where it came from" and "e-mail history" cards list their sources with a status pill: ✓ answered, ⚠ didn't answer, – off (a breach provider listed without its API key). Labels and values sit side by side, so the cards take half the height.

## 0.16.0 — 2026-11-22

- **Where the sign-up came from:** on Mastodon services, the request page shows the sign-up IP and how many other accounts the instance saw at it, by status (suspended, disabled, limited, pending, active), with a link to the instance's admin search. Counts only: aprova never shows who the other accounts are. A new alert, "IP used by accounts the instance acted against", is shown when one of them was suspended, disabled or limited; it never blocks the quorum.
- **New option `lookups.ip_info`:** the place, network (ASN) and reverse DNS of the IP, from an ip-info service such as ip.gutocarvalho.net.
- **New option `lookups.breaches`:** the e-mail's history in public breaches, from Have I Been Pwned (`hibp`, key in `APROVA_HIBP_KEY`) and XposedOrNot (`xon`). Shown as context: an address in old breaches is old and real.
- Lookups run only on the page of a pending request, in parallel and within 3 seconds, and are kept in memory for a day; aprova still stores no IP addresses. Both options are off by default.

## 0.15.0 — 2026-11-18

- **New option, off by default: `review.reject_unanswered_hours`.** When a question to the applicant has had no answer for that many hours, aprova rejects the request on its own and sends the standard rejection e-mail; the team sees the reason ("no answer to the question within N hours") on the request. The "How it works" page shows the rule.
- **New option `act_token_env` for `auth: moderator` services:** a service-account token (admin:read:accounts, admin:write:accounts) that aprova uses only for its own decisions, the answer deadline and `auto_reject_on_bounce`. Without it, those skip the service, as before.

## 0.14.2 — 2026-11-18

- The welcome e-mail is no longer skipped because the team commented on the request. Only a review or an e-mail already sent to the applicant count now: comments are internal, and when the mail server refused us for a while, a comment left in the meantime meant the person never got the welcome. Requests from the last 72 hours that missed it get it on the next pass.

## 0.14.1 — 2026-11-12

- The API token page now explains how to use the token: the endpoints, the actions and what each one needs, and copy-ready examples with your token (list the queue, open a request, vote to approve or reject), plus a link to the full reference.
- README: the API section documents getting a token, every endpoint (including editing your own comment), the actions and who may use them, the results, the error codes and examples.

## 0.14.0 — 2026-11-09

- The bolha.io look: every page follows the shared stylesheet of the bolha apps (light and dark, header with the PT · EN switch, cards, pills, standard footer), with system fonts instead of the bundled web fonts.
- The sections (queue, log, how it works, team chat, API token) and the queue filters are pills; the one you are on is highlighted. Release notes and sign out moved to the top right.
- Status labels, comments and alerts use the shared colors, so they read the same in light and dark. Behavior is unchanged.

## 0.13.1 — 2026-11-09

- The comment edit redirect is annotated for gosec (G710, open redirect): the target is always a `/r/` path on this host, with the service from the config and the id path-escaped, so it can't leave the site. No behavior change.

## 0.13.0 — 2026-11-09

Two matching votes decide, in either direction.

- The reject quorum is now the profile's `review.quorum`, the same number that approves, instead of a fixed 3. With a panel of three, two matching votes are already a majority and the third can't change the outcome, so the request no longer waits for it.
- `review.quorum` defaults to 2 (was 3). A single reject still blocks an automatic approval, and approve and reject in the same round still go to `on_conflict`.
- The "How it works" page shows the new numbers on its own (it reads the running config).

## 0.12.2 — 2026-11-08

- The bolha menu (the floating “● bolha” button with the other services) now works on these pages: the security policy allows the menu's script, served from this same site, and nothing else.

## 0.12.1 — 2026-11-07

- Queue page: the bot DM notice and its button sit on their own row, instead of overlapping the flash message and the filters.
- Stylesheet links carry the version (`?v=`), so a new release isn't hidden behind a cached stylesheet.

## 0.12.0 — 2026-11-07

The bot's direct messages, ready for a real team.

- `mastodon_dm.recipient_role_ids` / `escalation_role_ids`: every active local account holding one of those roles on the instance gets the digest / the escalations, besides any names in `recipients`. Looked up through the admin API with `lookup_token_env` (a token with `admin:read:accounts`, such as the `digest_token_env` one), at most every 30 minutes; a failed lookup keeps the last good list. Someone who joins the team gets the messages with no config change.
- `mastodon_dm.digest_hours`: the digest DM goes out once in each of those hours (in `timezone`), and only when something is waiting, instead of every time the queue changes. Without it, nothing changes. Mattermost keeps posting on changes.

## 0.11.1 — 2026-11-07

Hardening from the updated linters (gosec, semgrep).

- The image runs as the distroless non-root user (`USER 65532:65532`).
- `-healthcheck` (aprova and aprova-agent) takes only the port from the listen address and always asks 127.0.0.1, with a timeout; a listen address like `0.0.0.0:8080` now works too.
- The agent writes its recipe file read-only for its own user (0400) inside its private temporary directory, through `os.Root`.
- Form handlers read the form the `action` wrapper already parsed under its 64 KiB limit (`r.PostForm`), instead of `PostFormValue`.
- The remaining gosec and semgrep warnings (operator-set config path, interpreter and `-install` target) are annotated with the reason.

## 0.11.0 — 2026-11-07

- Approval e-mail (`mail.approve`, off by default): once a request is approved, by a moderator or by the quorum, aprova e-mails the person besides whatever the app sends — where to sign in, and whatever the profile wants to add (rules, code of conduct, the community's other services). Texts with `approve_subject` / `approve_body`; new field `{{.URL}}`, the service's address (`public_url` when `url` is internal, as for an agent; the config refuses an approval e-mail that would link to a non-https address). Logged as `approve_mail`; a failure doesn't undo the approval.
- The "How it works" page says when the approval e-mail is on.

## 0.10.0 — 2026-11-07

- The rejection e-mail is always aprova's: every rejection (by a moderator or by the reject quorum) sends the standard message, configurable with `reject_subject` / `reject_body`, never the reason.
- lemmy and gitlab now reject silently: Lemmy purges the applicant instead of denying (deny always e-mails), GitLab deletes the pending user instead of `POST /reject` (which e-mails). Both check the request is still pending right before; Lemmy never purges an accepted or admin account, GitLab deletes without `hard_delete`.
- The "How it works" page says what the applicant gets on rejection and on approval (which apps send their own welcome, which send nothing).

## 0.9.2 — 2026-11-07

- `internal_routes`: requests for a public host (e.g. the sign-in instance) can go straight to a URL inside the cluster, with the public Host and `X-Forwarded-Proto: https`, instead of out and back in through the edge proxy and WAF — a burst of fake logins can no longer get the panel's egress IP banned there.

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
