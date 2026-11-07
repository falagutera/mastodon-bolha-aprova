// Package core is aprova without any app: sign-in, profiles, permissions, the
// queue, reviews, discussion and the audit log. Apps come in as modules.
package core

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"gitlab.gutocarvalho.net/open/aprova/module"
)

// Config is the YAML file plus secrets from the environment.
type Config struct {
	Instance string                    `yaml:"instance"` // sign-in instance (Mastodon)
	BaseURL  string                    `yaml:"base_url"`
	Lang     string                    `yaml:"lang"`
	Modules  []string                  `yaml:"modules"`
	Services map[string]*ServiceConfig `yaml:"services"`
	Profiles map[string]*Profile       `yaml:"profiles"`
	Checks   ChecksConfig              `yaml:"checks"`
	// AutoRejectOnBounce rejects a request on its own when the question e-mail
	// bounces (undeliverable). Off by default: a bounce only flags the request.
	// Never applies to auth: moderator services (no system token to act with).
	AutoRejectOnBounce bool `yaml:"auto_reject_on_bounce"`
	// Timezone the panel shows times in (IANA name). Default America/Sao_Paulo.
	Timezone string `yaml:"timezone"`
	loc      *time.Location
	// InternalRoutes sends requests for a public host to a URL inside the
	// cluster instead of through the edge, e.g. bolha.us: http://web.bolha-us.svc:3000.
	InternalRoutes map[string]string `yaml:"internal_routes"`
	routes         map[string]*url.URL

	// From the environment.
	ClientID, ClientSecret, EncryptionKey  string
	DBHost, DBPort, DBUser, DBPassword     string
	DBName, DBSSLMode                      string
	SMTPHost, SMTPPort, SMTPUser, SMTPPass string
	// The reply-to mailbox, read over IMAPS for the applicants' answers. Off when empty.
	IMAPHost, IMAPPort, IMAPUser, IMAPPass string
	// TeamChat is an optional https link to the moderators' chat (e.g. a group
	// invite), shown in the panel only to signed-in moderators. It comes from the
	// environment, not the YAML, so a public config repo never carries the invite.
	TeamChat       string
	SMTPTLS        string
	Listen         string
	instanceHost   string
	enabled        []string
	serviceProfile map[string]string
}

type ServiceConfig struct {
	Module         string `yaml:"module"`
	Name           string `yaml:"name"` // shown to moderators, e.g. bolha.us
	URL            string `yaml:"url"`
	PublicURL      string `yaml:"public_url"` // for e-mails, when url is internal (an agent); default url
	Auth           string `yaml:"auth"`       // moderator or token
	TokenEnv       string `yaml:"token_env"`
	DigestTokenEnv string `yaml:"digest_token_env"` // read-only token for the digest, with auth: moderator

	token, digestToken string
}

// publicURL is the address people use: public_url, or url when that is public.
func (c ServiceConfig) publicURL() string {
	if c.PublicURL != "" {
		return c.PublicURL
	}
	return c.URL
}

// Profile groups services and says who may do what. Roles are role names on
// the sign-in instance.
type Profile struct {
	Name       string       `yaml:"name"`
	Services   []string     `yaml:"services"`
	Approve    []string     `yaml:"approve"`     // review, decide one by one
	BulkReject []string     `yaml:"bulk_reject"` // reject many at once
	EscalateTo []string     `yaml:"escalate_to"` // decide escalated requests
	Review     ReviewConfig `yaml:"review"`
	Mail       ProfileMail  `yaml:"mail"`
	// Mattermost incoming webhook, from this environment variable. Off when unset.
	MattermostWebhookEnv string `yaml:"mattermost_webhook_env"`
	// Direct messages from a bot account on the sign-in instance. Off when unset.
	MastodonDM *MastodonDM `yaml:"mastodon_dm"`

	id         string
	mattermost string
}

// MastodonDM sends the digest and escalations as direct messages from a bot
// account. Its token only needs write:statuses. Each person can turn it off in
// the panel.
type MastodonDM struct {
	TokenEnv             string   `yaml:"token_env"`
	Recipients           []string `yaml:"recipients"`            // get the digest of pending sign-ups
	EscalationRecipients []string `yaml:"escalation_recipients"` // get escalations

	token string
}

type ReviewConfig struct {
	Quorum         int      `yaml:"quorum"`
	OnConflict     string   `yaml:"on_conflict"` // escalate or block
	ResetOnNewInfo *bool    `yaml:"reset_on_new_info"`
	RequiredChecks []string `yaml:"required_checks"`
}

// ProfileMail is what the applicant receives. Empty texts use the built-in ones.
type ProfileMail struct {
	From          string `yaml:"from"`
	ReplyTo       string `yaml:"reply_to"`
	Lang          string `yaml:"lang"`
	RejectSubject string `yaml:"reject_subject"`
	RejectBody    string `yaml:"reject_body"`
	InfoSubject   string `yaml:"info_subject"`
	InfoBody      string `yaml:"info_body"`
	// Welcome e-mails every new applicant right away, asking who they are and
	// what they will post. It is tracked like a question: the reply lands in the
	// request and a bounce flags it as "e-mail not delivered". Off by default.
	Welcome        bool   `yaml:"welcome"`
	WelcomeSubject string `yaml:"welcome_subject"`
	WelcomeBody    string `yaml:"welcome_body"`
	// Approve e-mails the person once approved, besides whatever the app sends:
	// where to sign in, the rules and the rest of the community. Off by default.
	Approve        bool   `yaml:"approve"`
	ApproveSubject string `yaml:"approve_subject"`
	ApproveBody    string `yaml:"approve_body"`
}

type ChecksConfig struct {
	DisposableDomains []string `yaml:"disposable_domains"` // also matched against MX hosts
	InfoStaleDays     int      `yaml:"info_stale_days"`
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// LoadConfig reads the file and the environment, and refuses anything unclear.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's -config / APROVA_CONFIG file
	if err != nil {
		return nil, err
	}
	return ParseConfig(raw, os.Getenv)
}

// ParseConfig is LoadConfig without the file system, for tests.
func ParseConfig(raw []byte, getenv func(string) string) (*Config, error) {
	c := &Config{}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	get := func(k, fallback string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return fallback
	}
	c.ClientID, c.ClientSecret, c.EncryptionKey = getenv("APROVA_CLIENT_ID"), getenv("APROVA_CLIENT_SECRET"), getenv("APROVA_ENCRYPTION_KEY")
	c.DBHost, c.DBPort, c.DBUser = getenv("APROVA_DB_HOST"), get("APROVA_DB_PORT", "5432"), get("APROVA_DB_USER", "aprova")
	c.DBPassword, c.DBName, c.DBSSLMode = getenv("APROVA_DB_PASSWORD"), get("APROVA_DB_NAME", "aprova"), get("APROVA_DB_SSLMODE", "prefer")
	c.SMTPHost, c.SMTPPort, c.SMTPUser, c.SMTPPass = getenv("APROVA_SMTP_HOST"), get("APROVA_SMTP_PORT", "587"), getenv("APROVA_SMTP_USER"), getenv("APROVA_SMTP_PASSWORD")
	c.SMTPTLS = get("APROVA_SMTP_TLS", "starttls")
	c.IMAPHost, c.IMAPPort, c.IMAPUser, c.IMAPPass = getenv("APROVA_IMAP_HOST"), get("APROVA_IMAP_PORT", "993"), getenv("APROVA_IMAP_USER"), getenv("APROVA_IMAP_PASSWORD")
	c.TeamChat = getenv("APROVA_TEAM_CHAT")
	c.Listen = get("APROVA_LISTEN", ":8080")
	if c.Timezone == "" {
		c.Timezone = "America/Sao_Paulo"
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("config: timezone %q: %w", c.Timezone, err)
	}
	c.loc = loc
	if c.routes, err = parseRoutes(c.InternalRoutes); err != nil {
		return nil, err
	}
	if c.Lang == "" {
		c.Lang = "en"
	}
	if c.Checks.InfoStaleDays == 0 {
		c.Checks.InfoStaleDays = 7
	}
	return c, c.validate(getenv)
}

// httpsURL accepts a bare https origin. Plain http only on 127.0.0.1, for local
// development and tests.
func httpsURL(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return u, false
	}
	return u, u.Scheme == "https" || (u.Scheme == "http" && u.Hostname() == "127.0.0.1")
}

func (c *Config) validate(getenv func(string) string) error {
	c.Instance, c.BaseURL = strings.TrimRight(c.Instance, "/"), strings.TrimRight(c.BaseURL, "/")
	u, ok := httpsURL(c.Instance)
	if !ok {
		return errors.New("config: instance must be an https URL like https://example.social")
	}
	c.instanceHost = u.Host
	if _, ok := httpsURL(c.BaseURL); !ok {
		return errors.New("config: base_url must be an https URL")
	}
	if !slices.Contains(supportedLangs, c.Lang) {
		return fmt.Errorf("config: lang must be one of %v", supportedLangs)
	}
	switch {
	case c.ClientID == "" || c.ClientSecret == "":
		return errors.New("APROVA_CLIENT_ID and APROVA_CLIENT_SECRET are required")
	case len(c.EncryptionKey) < 32:
		return errors.New("APROVA_ENCRYPTION_KEY must have at least 32 characters")
	case c.DBHost == "" || c.DBPassword == "":
		return errors.New("APROVA_DB_HOST and APROVA_DB_PASSWORD are required")
	}
	if c.SMTPTLS != "starttls" && c.SMTPTLS != "tls" {
		return errors.New("APROVA_SMTP_TLS must be starttls or tls")
	}
	if c.TeamChat != "" {
		if u, err := url.Parse(c.TeamChat); err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("team_chat must be an https URL")
		}
	}

	// Modules: nothing is on unless listed.
	if len(c.Modules) == 0 {
		return fmt.Errorf("config: modules is empty, so nothing would be approved; list some of %v, or all", module.Names())
	}
	if slices.Equal(c.Modules, []string{"all"}) {
		c.enabled = module.Names()
	} else {
		for _, m := range c.Modules {
			if _, ok := module.Lookup(m); !ok {
				return fmt.Errorf("config: module %q is not in this build (it has %v)", m, module.Names())
			}
		}
		c.enabled = slices.Clone(c.Modules)
	}

	if len(c.Services) == 0 {
		return errors.New("config: no services")
	}
	for id, s := range c.Services {
		if !validID(id) {
			return fmt.Errorf("config: service id %q: use lowercase letters, digits and dashes", id)
		}
		if !slices.Contains(c.enabled, s.Module) {
			return fmt.Errorf("config: service %s uses module %q, which is not in modules", id, s.Module)
		}
		su, ok := httpsURL(strings.TrimRight(s.URL, "/"))
		// An agent may be on plain http inside a private network: its requests
		// are signed, and it holds nothing the app doesn't.
		if !ok && s.Module == "agent" && su != nil && su.Scheme == "http" && su.Host != "" && su.User == nil && su.RawQuery == "" && (su.Path == "" || su.Path == "/") {
			ok = true
		}
		if !ok {
			return fmt.Errorf("config: service %s: url must be https", id)
		}
		s.URL = strings.TrimRight(s.URL, "/")
		if s.Name == "" {
			s.Name = su.Host
		}
		switch s.Auth {
		case "moderator":
			// The moderator's token is from the sign-in instance only.
			if su.Host != c.instanceHost {
				return fmt.Errorf("config: service %s: auth moderator only works on the sign-in instance %s", id, c.instanceHost)
			}
			if s.DigestTokenEnv != "" {
				s.digestToken = getenv(s.DigestTokenEnv)
				if s.digestToken == "" {
					return fmt.Errorf("config: service %s: %s is empty", id, s.DigestTokenEnv)
				}
			}
		case "token":
			if s.TokenEnv == "" || getenv(s.TokenEnv) == "" {
				return fmt.Errorf("config: service %s: auth token needs token_env set to a non-empty variable", id)
			}
			s.token = getenv(s.TokenEnv)
			s.digestToken = s.token
		default:
			return fmt.Errorf("config: service %s: auth must be moderator or token", id)
		}
	}

	if len(c.Profiles) == 0 {
		return errors.New("config: no profiles")
	}
	c.serviceProfile = map[string]string{}
	for id, p := range c.Profiles {
		if !validID(id) {
			return fmt.Errorf("config: profile id %q: use lowercase letters, digits and dashes", id)
		}
		p.id = id
		if p.Name == "" {
			p.Name = id
		}
		if len(p.Services) == 0 || len(p.Approve) == 0 || len(p.EscalateTo) == 0 {
			return fmt.Errorf("config: profile %s needs services, approve and escalate_to", id)
		}
		for _, s := range p.Services {
			if _, ok := c.Services[s]; !ok {
				return fmt.Errorf("config: profile %s: unknown service %s", id, s)
			}
			if other, dup := c.serviceProfile[s]; dup {
				return fmt.Errorf("config: service %s is in profiles %s and %s; use one", s, other, id)
			}
			c.serviceProfile[s] = id
			// The approval e-mail links to the service: never an internal address.
			if sc := c.Services[s]; p.Mail.Approve && !strings.HasPrefix(sc.publicURL(), "https://") {
				return fmt.Errorf("config: profile %s: approve e-mail on, but service %s has no https address; set public_url", id, s)
			}
		}
		r := &p.Review
		if r.Quorum == 0 {
			r.Quorum = 3
		}
		if r.Quorum < 1 || r.Quorum > 10 {
			return fmt.Errorf("config: profile %s: quorum must be 1 to 10", id)
		}
		if r.OnConflict == "" {
			r.OnConflict = "escalate"
		}
		if r.OnConflict != "escalate" && r.OnConflict != "block" {
			return fmt.Errorf("config: profile %s: on_conflict must be escalate or block", id)
		}
		if r.ResetOnNewInfo == nil {
			yes := true
			r.ResetOnNewInfo = &yes
		}
		for _, chk := range r.RequiredChecks {
			if !slices.Contains(coreChecks, chk) {
				return fmt.Errorf("config: profile %s: unknown check %q (known: %v)", id, chk, coreChecks)
			}
		}
		if p.Mail.Lang == "" {
			p.Mail.Lang = c.Lang
		}
		if !slices.Contains(supportedLangs, p.Mail.Lang) {
			return fmt.Errorf("config: profile %s: mail lang must be one of %v", id, supportedLangs)
		}
		if d := p.MastodonDM; d != nil {
			if d.TokenEnv == "" || getenv(d.TokenEnv) == "" {
				return fmt.Errorf("config: profile %s: mastodon_dm needs token_env set to a non-empty variable", id)
			}
			d.token = getenv(d.TokenEnv)
			for _, list := range []*[]string{&d.Recipients, &d.EscalationRecipients} {
				for i, h := range *list {
					h = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(h), "@"))
					if !localHandle.MatchString(h) {
						return fmt.Errorf("config: profile %s: mastodon_dm recipient %q must be a local username", id, (*list)[i])
					}
					(*list)[i] = h
				}
			}
			if len(d.Recipients)+len(d.EscalationRecipients) == 0 {
				return fmt.Errorf("config: profile %s: mastodon_dm has no recipients", id)
			}
		}
		if p.MattermostWebhookEnv != "" {
			p.mattermost = getenv(p.MattermostWebhookEnv)
			if _, ok := webhookURL(p.mattermost); !ok {
				return fmt.Errorf("config: profile %s: %s must be an https webhook URL", id, p.MattermostWebhookEnv)
			}
		}
	}
	for id := range c.Services {
		if _, ok := c.serviceProfile[id]; !ok {
			return fmt.Errorf("config: service %s is in no profile", id)
		}
	}
	return nil
}

var localHandle = regexp.MustCompile(`^[a-z0-9_]{1,30}$`)

func webhookURL(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	return u, err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http" && u.Hostname() == "127.0.0.1")
}

func validID(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// moderatorScopes reports whether sign-in must ask for the admin scopes.
func (c *Config) moderatorScopes() bool {
	for _, s := range c.Services {
		if s.Auth == "moderator" {
			return true
		}
	}
	return false
}

// SMTPEnabled reports whether aprova can e-mail applicants.
func (c *Config) SMTPEnabled() bool { return c.SMTPHost != "" }

// Enabled lists the modules turned on.
func (c *Config) Enabled() []string { return c.enabled }
