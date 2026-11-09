# AGENTS.md

Notes for coding agents (and people) changing aprova. The README says what the panel does, how to
install it and the API; this file says how to change it without breaking the promises the README makes.

## What it is

Two Go binaries, no JavaScript at all, PostgreSQL through pgx (no ORM):

- `aprova` (`main.go` + `core/`): the moderators' panel and JSON API. The core knows no app; each
  app is a **module** (`modules/<name>`) behind the interface in `module/module.go`.
- `aprova-agent` (`cmd/aprova-agent`): runs next to an app with no approval API and executes a fixed
  recipe (`recipes/*.php|py`) that calls the app's own code. Contract in `wire/wire.go`.

## Commands

```sh
gofmt -l .                # must print nothing
go vet ./...
go test -count=1 -p 1 ./...
for t in agent forgejo gitlab lemmy mastodon peertube writefreely; do go build -tags no_$t ./... || echo "no_$t breaks"; done
```

Without `APROVA_TEST_DB_HOST`, 33 core tests **skip** — most of the policy and privacy tests (rules 1, 3, 5–8
below). A change under `core/` is not tested until the database block runs. The build loop is the same list
as the one in `.gitlab-ci.yml`; keep them equal.

With a database (what CI runs). The core tests **empty aprova's tables** (`DELETE FROM`) at the start of
each test, so use a port of your own, chain with `&&`, and never point them at a shared Postgres: they
refuse a database whose name does not end in `_test` (`mustBeTestDB` in `core/harness_test.go`). They
connect as the app's role `aprova` (password `aprova`), so they also prove the grants in `schema.sql`.

```sh
P=55641   # any free port
docker run -d --name aprova-pg-$P -p $P:5432 -e POSTGRES_PASSWORD=pg postgres:17-alpine &&
  until docker exec aprova-pg-$P pg_isready -h 127.0.0.1 -U postgres -q; do sleep 1; done &&
  docker exec aprova-pg-$P psql -q -U postgres -c "CREATE ROLE aprova LOGIN PASSWORD 'aprova'" -c "CREATE DATABASE aprova_test" &&
  docker exec -i aprova-pg-$P psql -q -U postgres -d aprova_test -v ON_ERROR_STOP=1 -f - < schema.sql &&
  APROVA_TEST_DB_HOST=127.0.0.1 APROVA_TEST_DB_PORT=$P go test -count=1 -p 1 ./...

docker rm -f aprova-pg-$P   # always, even when a step above failed
```

Test variables: `APROVA_TEST_DB_HOST`, `APROVA_TEST_DB_PORT`, `APROVA_TEST_DB_NAME` (default `aprova_test`).
After changing `schema.sql`, recreate the container: the tests do not migrate.

To look at a rendered page: there is no dump. In a core test, `w := newWorld(t)`, add an applicant
(`w.bolha.add(uniq(), "ana", "ana@example.org", "hi")`), then `_, _, body := w.login("p1").do("GET", "/queue", nil)`
and `t.Log(body)`. `TestEveryPageRenders` executes every template in both languages with data in each branch.

## Map

| Path | What |
|---|---|
| `module/module.go` | the contract: `Module` (`Pending`, `Get`, `Approve`, `Reject`, `Caps`), `Request`, `Check`, `ErrGone`, `ErrUnauthorized`, `Register` |
| `moduletest/contract.go` | `moduletest.Run`, the guarantees every module's tests must pass against a fake of the app |
| `modules/<name>/` | one app each; `modules/all/<name>.go` imports it behind `//go:build !no_<name>` |
| `wire/wire.go`, `modules/agent/` | the agent protocol (HMAC-SHA256 over method, path, time, body hash; `wire.ID` pattern) and the module that speaks it |
| `cmd/aprova-agent/` | the agent: `main.go` (signed handler, one decision at a time), `recipes.go` + `recipes/`, `script.go` (runs a recipe, no shell) |
| `core/config.go` | YAML + env, `validate` (https only, modules on only when listed, `auth: moderator` only on the sign-in instance) |
| `core/server.go` | routes, `page`/`action` wrappers (session, same-site, CSRF, 64 KiB form), `securityHeaders` (CSP), queue |
| `core/actions.go` | `buildRequest` and `act`: every page and API action ends here; `apply` holds the review/quorum logic |
| `core/policy.go` | who may view, review, decide, bulk-reject, de-escalate; `tally` (one quorum for both directions) |
| `core/auth.go`, `core/api.go`, `core/crypto.go` | OAuth sign-in, sessions (tokens sealed with AES-GCM), role re-check, API tokens |
| `core/inbox.go`, `notify.go`, `welcome.go`, `digest.go`, `dmteam.go` | IMAP replies and bounces, SMTP, welcome e-mail, Mattermost/DM digests, bot recipients by role |
| `core/rules.go` | the "How it works" page, built from the running config |
| `core/i18n.go`, `core/templates/`, `core/assets/` | every text in `en` and `pt-BR`; HTML templates; CSS and fonts |
| `schema.sql` | tables and **every grant** to the `aprova` role (the app can't alter its schema) |

Tests: `core/harness_test.go` builds the world (fake Mastodon instances `bolha` and `jorn`, fake GitLab,
mail and Mattermost catchers, real Postgres; config in `testConfig`); `core/core_test.go` holds the policy
and privacy tests; each module has `TestContract` plus app-specific tests against an `httptest` fake.

## Rules that must hold

A change that weakens one of these is a bug, even if no test catches it yet.
1. **No data about accounts that aren't pending.** A module's credentials can read any account, so
   `buildRequest` copies username, e-mail and reason only while `rv.Pending`, and unknown ids stay
   unknown (`act` only marks gone what aprova already knows). Guarded by `TestNoDataAboutAccountsThatArentPending`,
   `TestDecidedElsewhereIsGone`.
2. **Ids from URLs never reach an app unchecked.** Each module validates the id against its own
   pattern before any request (and checks values the app returns before putting them in a path).
   Guarded by `moduletest.Run` ("bad ids never reach the app") and per-module `TestBadIDsNeverReachTheServer`,
   `TestOddLoginFromTheServerIsNotUsed`; the agent checks `wire.ID` (`TestServer`).
3. **Check pending right before acting.** Approve/Reject re-read the request and return `module.ErrGone`
   if it was decided meanwhile; nothing is approved twice; a reject never deletes an approved/active
   account. Guarded by `moduletest.Run`, `TestActiveOrBlockedAccountsAreNeverDeleted`, `TestRejectLeavesActiveUsers`,
   `TestRejectNeverPurgesAnAcceptedAccount`.
4. **Least privilege on the app.** Modules with admin tokens call only their listed paths; a test reads the
   source or the fake's call log and fails on anything else (`TestSourceUsesOnlyTheUserPaths`,
   `TestSourceUsesOnlyTheUsersPath`, `TestSourceUsesOnlyTheRegistrationsAPI`, `allowed` in the forgejo/gitlab tests).
   Use the app's official API; never write to its database. Follow only pagination links on the same
   host (`TestPagesStayOnTheInstance`).
5. **aprova is the only voice on rejection.** The applicant gets the standard `reject_body`, never the
   internal reason; modules reject silently where the app allows (`Caps.SilentReject`, checked by the
   contract). Guarded by `TestRejectSendsTheStandardMessageOnly`, `TestRejectIsSilent`, `TestRejectPurgesWithoutReason`.
6. **Authorization on the server, same rules for pages and API.** Every handler asks `policy.go`; the
   API goes through the same `act`/`buildRequest`; only moderation roles get in, and losing the role ends
   the session; instance tokens and cookies never authenticate the API. Guarded by `TestOnlyModerationRolesGetIn`,
   `TestAPIUsesTheSameRules`, `TestLosingTheRoleEndsTheSession`, `TestDescalateOnlyBySeniors`,
   `TestAPITokenCannotBeReplayedAsABrowserSession`. Forms need CSRF and same-site (`action`; `TestActionsNeedCSRFAndSameSite`).
7. **Quorum semantics.** Approval quorum with the last reviewer's token; one vote per person per round;
   a reject blocks; a conflict goes to `on_conflict`; failed required checks keep the quorum from approving;
   the reject quorum is the same `review.quorum` (default 2, a majority of three). Tests `TestQuorum…`, `TestRejectQuorum…`, `TestOneVotePerPerson`, `TestFailedChecksKeepTheQuorumFromApproving`.
8. **Notices carry no e-mails or reasons**, and secrets (webhook URLs, passwords) stay out of errors
   (`TestBotSendsDirectMessagesAndEachPersonCanTurnThemOff`, `TestWebhookURLStaysOutOfErrors`, `TestBadCredentialsStayOutOfErrors`).
9. **No script, no inline style.** CSP is `default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; …`
   (script/connect `'self'` only so the bolha-bar the edge injects can load; the pages have none); the look is
   `core/assets/style.css`, a copy of `open/gcn/ui` (`sync.sh --assets core/assets`, never edited here;
   `TestPagesFollowTheLook`, `check.sh --assets core/assets`); what is aprova's own goes in
   `core/assets/aprova.css`, with the tokens, no web fonts (`TestNoScriptOrInlineStyle`). Templates are `html/template`; user text is
   escaped (`TestEveryPageRenders`).
10. **Both languages.** Every key exists in `en` and `pt-BR` (`TestTranslationsHaveTheSameKeys`); new
    flash keys go in `flashes` and need `flash_<key>`. A key missing from a language renders the raw id;
    for checks and bot-signal details `TestEveryCheckAndDetailHasTexts` catches that, elsewhere nothing does.
11. **Bounded work.** Outbound HTTP uses the core's client (`New` in `server.go`, 15 s timeout); responses go
    through `io.LimitReader`; forms are capped at 64 KiB, comments at `maxText`. The agent takes no code from the
    network. And every module can be left out with `-tags no_<name>` (the loop in Commands).

## Adding a module (the usual change)

0. **Check it is not there already**: `ls modules/`, the README's module table, and the agent recipes
   (`newRecipe` in `cmd/aprova-agent/main.go`). An app with no approval API is a **recipe**, not a module:
   add `recipes/<app>.*`, a `new<App>` in `recipes.go`, its case in `newRecipe` **and** its name in the two
   "(known: …)" error strings there, the name in `TestEveryRecipeLoads`, and the README's recipe list and table.
1. `modules/<name>/<name>.go`: `func init() { module.Register("<name>", New) }`; `New` refuses non-https
   URLs and the wrong `auth`; an `idPattern` checked first in `Get`, `Approve`, `Reject`; a `status()`
   mapping 401/403 to `ErrUnauthorized` and 404 to `ErrGone`; the `Config.Client` the core passes (it carries
   the timeout and `internal_routes`); `Caps()` honest about silence and welcome. Model: `forgejo`, `writefreely`.
2. `modules/<name>/<name>_test.go`: an `httptest` fake implementing `moduletest.Fake`, then `TestContract`
   calling `moduletest.Run`; plus a source/call-log test that fails on any API path you didn't mean to use,
   `TestConfig`, a wrong-token test, and the app's own corner cases.
3. `modules/all/<name>.go` with `//go:build !no_<name>`; add the name to the `for t in …` loop, both in
   Commands and in `.gitlab-ci.yml`.
4. README: a row in the module table (access needed, what approve/reject do, what the app e-mails).
5. `gofmt`, `go vet`, the tests with a database, the `no_<name>` builds; a VERSION bump and changelog entry.

### Adding a check or bot signal

A new bot signal is one more `return true, "<x>"` in `botSignal` (`core/checks.go`) plus `detail_<x>` in both
languages, and a case for it in `core/checks_test.go` (no test covers `botSignal` directly yet).
A new check `Name` needs `check_<n>` (shown when it fails) and
`checked_<n>` (the passed list and the "How it works" page) in both languages; `check_ok_*` keys exist but no
template uses them. Only core checks (`coreChecks`) can go in a profile's `review.required_checks`; a module's
own checks (`email_verified`) are shown, never required. `TestEveryCheckAndDetailHasTexts` finds names and
details from the source and fails on a missing text.

## Releasing

- `VERSION` holds `X.Y.Z`; CI adds the pipeline number (`X.Y.Z-BUILD`, `main.build`). Bump it for user-visible
  changes; docs-, test- or CI-only changes don't bump it.
- `TestChangelogHasVersion` fails when `VERSION` has no `## X.Y.Z` entry in `CHANGELOG.md` (the panel's release
  notes page). Changes not released yet may wait under `## Unreleased`, which `internal/relnotes` skips.
- A change to `schema.sql` says so in the changelog: installs apply it as the database owner.
- Code, comments, commits and the README in English. Keep commits small with a message that says why.

## Security checks

CI runs gofmt, vet, the tests against PostgreSQL 17, the `no_<name>` builds, govulncheck and the shared
scanners (gitleaks, gosec, semgrep, trivy, hadolint, shellcheck, yamllint, poutine). Before a release that
touches a module, the agent, sign-in or what a page shows, also review the change for authorization and
data exposure (who sees which account, with which token) — that is the part scanners do not see.
