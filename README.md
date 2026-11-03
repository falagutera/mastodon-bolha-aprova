# aprova

One queue to approve sign-ups across the apps of a community. Moderators sign in with their Mastodon account, review requests together, ask applicants for more information, escalate doubtful cases and decide. The core knows no app: each app is a module, and an installation turns on the ones it uses.

- **Reviews like a pull request:** "I would approve", "I would reject" or "needs more information". A quorum of approvals (2 by default) approves on its own, with the token of the moderator whose review completes it. A reject blocks it; approve and reject at once escalate.
- **Checks:** signs of automated sign-ups and disposable e-mail (domain or MX host) keep the quorum from approving.
- **Discussion:** internal comments on each request. The applicant never sees them.
- **Ask the applicant:** an e-mail with the moderator's question; the request waits, and the answer starts a new round of reviews.
- **Escalation:** to the senior roles of a profile, who then decide alone.
- **Notices:** a digest of pending sign-ups and escalations, by Mattermost webhook and/or direct messages from a bot account on the instance. Both off unless configured; each moderator can turn the bot's messages off in the panel. Messages never carry e-mails or reasons.
- **Rejection:** with an internal reason. The applicant gets a standard, configurable message, never the reason. Modules reject without messaging the applicant, so aprova is the only voice.
- **Profiles:** group services and say which roles approve, bulk reject and decide escalated requests.
- **API first:** everything the pages do is in the JSON API, with an aprova token made on the "API token" page.
- Decision log, Portuguese and English.

## Modules

| Module | App | Access |
|---|---|---|
| `mastodon` | Mastodon | The moderator's own token (admin scopes), or a service account's |
| `gitlab` | GitLab with admin approval of new users | Admin token: GitLab has nothing smaller. The module only calls list, approve and reject, and a test fails if it calls anything else |

Choose the modules in the configuration (`modules: [mastodon, gitlab]` or `all`). A module that isn't listed registers no route, opens no connection and reads no secret. To leave one out of the binary: `go build -tags no_gitlab`.

### Writing a module

A module is a package under `modules/` that implements `module.Module` (`Pending`, `Get`, `Approve`, `Reject`, `Caps`) and registers itself in `init`. Add a file to `modules/all` that imports it behind a `no_<name>` build tag. Its tests must pass `moduletest.Run`, the contract every module meets: pending is checked before acting, nothing is approved twice, a request that disappeared is `ErrGone`, and bad ids never reach the app. Use the app's official API; never write to its database.

## Install

1. Register the OAuth app on the sign-in instance:

   ```sh
   curl -F client_name=aprova \
     -F "scopes=profile admin:read:accounts admin:write:accounts" \
     -F redirect_uris=https://aprova.example.social/auth/callback \
     https://example.social/api/v1/apps
   ```

   With no `auth: moderator` service, `profile` alone is enough.

2. Create the database and apply `schema.sql` as its owner. The app user can't change the schema and can only add to the comments and the log.

3. Copy `config.example.yaml` and `.env.example`, fill them in, and run the image behind HTTPS (the session cookie needs it).

## API

`Authorization: Bearer aprova_...`, a token made on the "API token" page after signing in. It is a session of the moderator: same role, re-checked on the instance every minute, and it expires in 8 hours. Tokens from the instance itself are refused, whatever app issued them, and cookies never authenticate the API.

Requests are only shown while pending. The module's credentials can read any account of the app, so aprova never returns an account's data once it is no longer a pending request, and ignores ids it never handled.

| | |
|---|---|
| `GET /api/v1/me` | Account, role, visible profiles, modules |
| `GET /api/v1/queue` | Pending requests per profile and service |
| `GET /api/v1/search?email=` | Pending requests with that e-mail, in every visible service |
| `GET /api/v1/requests/{service}/{id}` | One request: checks, reviews, discussion, what you may do |
| `POST /api/v1/requests/{service}/{id}/{action}` | `review` (`verdict`: approve, reject, info), `approve`, `reject`, `escalate`, `deescalate`, `answer`, `comment`; JSON body `{"verdict": "...", "body": "..."}` |
| `GET /api/v1/audit` | Decision log of the visible profiles |

## Versions

`X.Y.Z-BUILD`: `X.Y.Z` in `VERSION`, `BUILD` the pipeline number. Pin `X.Y.Z-BUILD` in production.

## Development

```sh
go test ./...
```

Tests that need PostgreSQL skip themselves unless `APROVA_TEST_DB_HOST` is set; see `.gitlab-ci.yml`.

## License

AGPL-3.0, see `LICENSE`.
