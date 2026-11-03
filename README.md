# aprova

One queue to approve sign-ups across the apps of a community. Moderators sign in with their Mastodon account, review requests together, ask applicants for more information, escalate doubtful cases and decide. The core knows no app: each app is a module, and an installation turns on the ones it uses.

- **Reviews like a pull request:** "I would approve", "I would reject" or "needs more information". A quorum of approvals (2 by default) approves on its own, with the token of the moderator whose review completes it. A reject blocks it; approve and reject at once escalate.
- **Checks:** signs of automated sign-ups and disposable e-mail (domain or MX host) keep the quorum from approving.
- **Discussion:** internal comments on each request. The applicant never sees them.
- **Ask the applicant:** an e-mail with the moderator's question; the request waits, and the answer starts a new round of reviews. With `APROVA_IMAP_*` set, aprova reads the reply-to mailbox every minute and attaches the answer itself: it matches the question's Message-ID, accepts only the applicant's own address (kept as a hash), and drops the quoted text. Moderators can still paste an answer by hand.
- **Edits:** authors fix their own comments; the old text goes to the decision log.
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
| `lemmy` | Lemmy with "require application" (tested on 0.19) | JWT of a dedicated admin account. Lemmy e-mails the applicant on approve and reject and can't be told not to, so aprova skips its own rejection e-mail; no deny_reason is ever sent |
| `forgejo` | Forgejo with manual confirmation of new accounts (`REGISTER_MANUAL_CONFIRM`) | Site admin token (`read:admin`, `write:admin`, `read:user`). Approving activates the account; rejecting deletes it (purge), only if it is still inactive and never signed in. Forgejo sends no e-mail either way. A test fails if the module calls anything but these user paths |
| `peertube` | PeerTube with registration approval (8.3+) | `token_env` holds `username:password` of a dedicated Moderator user (password grant; the token is kept in memory and refreshed). Reject is silent; accept lets PeerTube send its welcome |
| `agent` | Apps with no approval API, through `aprova-agent` (see [The agent](#the-agent)) | A secret shared with the agent; every request is signed |

`aprova-agent` recipes: `pixelfed` (curated onboarding), `bookwyrm` (invite requests), `friendica` (sign-ups awaiting approval).

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

## The agent

Some apps have no API to approve sign-ups: their admin panel is the only way in. For those, `aprova-agent` runs next to the app, inside the app's own image and with the app's own configuration, and runs a fixed recipe that calls the app's own code. aprova reaches it through the `agent` module.

- It takes no code or commands from the network: a recipe is a fixed program built into the binary, and the only input that reaches it is a request id that passed a strict pattern.
- Every request is signed with HMAC-SHA256 over method, path, time and body, with a secret shared with aprova (32 characters or more). It refuses unsigned requests and those more than a minute off its clock. A signature for one id or action is no good for another.
- One decision at a time, and each recipe checks the request is still pending, under a lock, before acting.
- It holds nothing the app doesn't: its secrets are the app's own environment, plus the shared secret.
- Reject is always silent in the app; aprova sends its own message.

The contract is in `wire/wire.go`, so an agent can also be written in another language.

### Install

The `aprova` image carries the agent at `/aprova-agent`. The image is distroless, so an init container copies the binary into a shared volume, and the agent then runs in a container made from the app's image:

```yaml
initContainers:
- name: install-agent
  image: registry.gutocarvalho.net/images/aprova:<version>
  command: ["/aprova-agent", "-install", "/agent/aprova-agent"]
  volumeMounts: [{ name: agent, mountPath: /agent }]
containers:
- name: aprova-agent
  image: <the app's own image and tag>
  command: ["/agent/aprova-agent"]
  envFrom: [<the same ConfigMaps and Secrets as the app>]
  env:
  - { name: APROVA_AGENT_RECIPE, value: pixelfed }
  - { name: APROVA_AGENT_SECRET, valueFrom: { secretKeyRef: { name: aprova-agent, key: secret } } }
  ports: [{ containerPort: 8090 }]
  volumeMounts:
  - { name: agent, mountPath: /agent, readOnly: true }
  - { name: tmp, mountPath: /tmp }   # the recipe file is written here
volumes:
- { name: agent, emptyDir: {} }
- { name: tmp, emptyDir: {} }
```

Run it as its own Deployment rather than inside the app's pods, so restarting one never touches the other. Then:

1. Generate the shared secret (`openssl rand -hex 32`) and give it to both sides.
2. Let only aprova reach port 8090 (a NetworkPolicy in Kubernetes). Plain http is accepted for the `agent` module because requests are signed, but it carries applicants' e-mails: keep it inside a private network, or put TLS in front.
3. Add the service to aprova:

```yaml
modules: [mastodon, agent]
services:
  photos:
    module: agent
    name: photos.example
    url: http://aprova-agent.photos.svc.cluster.local:8090
    auth: token
    token_env: APROVA_PHOTOS_AGENT_SECRET
```

| Variable | Default | |
|---|---|---|
| `APROVA_AGENT_RECIPE` | | `pixelfed`, `bookwyrm` or `friendica` |
| `APROVA_AGENT_SECRET` | | Shared with aprova, 32+ characters |
| `APROVA_AGENT_LISTEN` | `:8090` | |
| `APROVA_AGENT_TMP` | system temp dir | Where the recipe file is written |
| `APROVA_AGENT_PIXELFED_ROOT` | `/var/www/html` | Pixelfed's code |
| `APROVA_AGENT_PHP` | `php` | |
| `APROVA_AGENT_BOOKWYRM_ROOT` | `/app` | BookWyrm's code |
| `APROVA_AGENT_BOOKWYRM_INVITER` | first superuser | Local user that owns the invites |
| `APROVA_AGENT_PYTHON` | `python3` | |
| `APROVA_AGENT_FRIENDICA_ROOT` | `/var/www/html` | Friendica's code |

Recipes:

- **bookwyrm:** invite requests (tested on 0.9.4). There is no username yet: the person picks one through the invite link, so the queue shows the e-mail's local part. Approve creates a one-use invite and BookWyrm e-mails the link, as the admin page does. Reject sets "ignored" (never toggles it back); BookWyrm sends nothing.
- **friendica:** sign-ups awaiting approval (tested on 2026.05). Loads Friendica as `bin/console` does and calls `User::allow` / `User::deny`. Approve sends Friendica's welcome (it includes the password the person chose; Friendica has no way to turn that off). Deny deletes the pending account and sends nothing.
- **pixelfed:** curated onboarding (tested on 0.15.2). Lists what the admin panel's "open" filter shows (e-mail confirmed, not waiting on the applicant). Approves and rejects by calling Pixelfed's own `AdminCuratedRegisterController`, so the account and the welcome e-mail are Pixelfed's; reject is `reject-silent`.

## Versions

`X.Y.Z-BUILD`: `X.Y.Z` in `VERSION`, `BUILD` the pipeline number. Pin `X.Y.Z-BUILD` in production.

## Development

```sh
go test ./...
```

Tests that need PostgreSQL skip themselves unless `APROVA_TEST_DB_HOST` is set; see `.gitlab-ci.yml`.

## License

AGPL-3.0, see `LICENSE`.
