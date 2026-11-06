package core

// release is one entry in the panel's release notes. Keep newest first and in
// step with VERSION: add an entry when you bump it.
type release struct {
	Version string
	Date    string // YYYY-MM-DD
	Notes   []string
}

var releases = []release{
	{"0.8.2", "2026-11-06", []string{
		"Release notes page, reached from the top menu.",
		"Optional team-chat link (env APROVA_TEAM_CHAT) shown in the panel to signed-in moderators only — kept out of the config file so a public repo never carries the invite.",
	}},
	{"0.8.0", "2026-11-05", []string{
		"Bounce detection: when the question e-mail to an applicant is undeliverable, the request is flagged \"e-mail not delivered\", so a fake sign-up is easy to reject. Optional auto_reject_on_bounce rejects it on its own.",
	}},
	{"0.7.2", "2026-11-05", []string{
		"Forgejo reject: purge only an account that never signed in; one that did is deleted without purge, so an established account that owns content is protected rather than erased.",
	}},
	{"0.7.1", "2026-11-05", []string{
		"Forgejo reject now deletes a pending account even if it signed in before approval — bots that log in first can finally be rejected from the panel.",
	}},
	{"0.7.0", "2026-11-05", []string{
		"A reject quorum, and the approval quorum raised to three.",
	}},
	{"0.6.4", "2026-11-05", []string{
		"Security fixes: API-token replay and NUL-byte reply poisoning.",
	}},
	{"0.6.3", "2026-11-05", []string{
		"Skip the reason check for apps whose sign-up form has no reason field.",
	}},
	{"0.6.2", "2026-11-04", []string{
		"The discussion is split by round.",
	}},
	{"0.6.1", "2026-11-04", []string{
		"Approve: the note is internal, required only after a reject or a failed check.",
	}},
	{"0.6.0", "2026-11-04", []string{
		"WriteFreely module, approving through the approval patch's registrations API.",
	}},
	{"0.5.0", "2026-11-03", []string{
		"Lemmy, PeerTube and Forgejo modules; BookWyrm and Friendica agent recipes.",
	}},
	{"0.4.0", "2026-11-03", []string{
		"Applicants' answers are read from the reply-to mailbox and attached on their own.",
	}},
	{"0.3.0", "2026-11-03", []string{
		"Review and question in separate boxes; authors fix their own comments.",
	}},
	{"0.2.0", "2026-11-03", []string{
		"Agent module and aprova-agent, with a Pixelfed recipe for apps without an API.",
	}},
	{"0.1.0", "2026-11-03", []string{
		"Sign-up approval panel with the Mastodon and GitLab modules.",
	}},
}
