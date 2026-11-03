package main

import (
	_ "embed"
	"os"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/wire"
)

//go:embed recipes/pixelfed.php
var pixelfedPHP []byte

//go:embed recipes/bookwyrm.py
var bookwyrmPy []byte

//go:embed recipes/friendica.php
var friendicaPHP []byte

// Pixelfed: curated onboarding. Runs in the Pixelfed image with its environment.
//
//	APROVA_AGENT_PIXELFED_ROOT  default /var/www/html
//	APROVA_AGENT_PHP            default php
func newPixelfed() (recipe, error) {
	root := os.Getenv("APROVA_AGENT_PIXELFED_ROOT")
	if root == "" {
		root = "/var/www/html"
	}
	php := os.Getenv("APROVA_AGENT_PHP")
	if php == "" {
		php = "php"
	}
	p, err := writeScript("pixelfed.php", pixelfedPHP)
	if err != nil {
		return nil, err
	}
	return &script{
		recipeName: "pixelfed",
		interp:     []string{php},
		path:       p,
		dir:        root,
		env:        []string{"APROVA_AGENT_PIXELFED_ROOT=" + root},
		timeout:    60 * time.Second,
		// Pixelfed's reject-silent sends nothing; approving sends its welcome.
		capsValue: wire.Caps{SilentReject: true, SendsWelcome: true},
	}, nil
}

// BookWyrm: invite requests. Runs in the BookWyrm image with its environment.
// Approving makes a one-use invite that BookWyrm e-mails; rejecting ignores.
//
//	APROVA_AGENT_BOOKWYRM_ROOT     default /app
//	APROVA_AGENT_BOOKWYRM_INVITER  local username that owns the invites (default: first superuser)
//	APROVA_AGENT_PYTHON            default python3
func newBookwyrm() (recipe, error) {
	root := os.Getenv("APROVA_AGENT_BOOKWYRM_ROOT")
	if root == "" {
		root = "/app"
	}
	py := os.Getenv("APROVA_AGENT_PYTHON")
	if py == "" {
		py = "python3"
	}
	p, err := writeScript("bookwyrm.py", bookwyrmPy)
	if err != nil {
		return nil, err
	}
	return &script{
		recipeName: "bookwyrm",
		interp:     []string{py},
		path:       p,
		dir:        root,
		env:        []string{"APROVA_AGENT_BOOKWYRM_ROOT=" + root},
		timeout:    90 * time.Second,
		capsValue:  wire.Caps{SilentReject: true, SendsWelcome: true},
	}, nil
}

// Friendica: sign-ups awaiting approval (REGISTER_APPROVE). Runs in the
// Friendica image with its environment. Approving sends Friendica's own
// welcome (it includes the password the person chose); deny deletes the
// pending account and sends nothing.
//
//	APROVA_AGENT_FRIENDICA_ROOT  default /var/www/html
//	APROVA_AGENT_PHP             default php
func newFriendica() (recipe, error) {
	root := os.Getenv("APROVA_AGENT_FRIENDICA_ROOT")
	if root == "" {
		root = "/var/www/html"
	}
	php := os.Getenv("APROVA_AGENT_PHP")
	if php == "" {
		php = "php"
	}
	p, err := writeScript("friendica.php", friendicaPHP)
	if err != nil {
		return nil, err
	}
	return &script{
		recipeName: "friendica",
		interp:     []string{php},
		path:       p,
		dir:        root,
		env:        []string{"APROVA_AGENT_FRIENDICA_ROOT=" + root},
		timeout:    60 * time.Second,
		capsValue:  wire.Caps{SilentReject: true, SendsWelcome: true},
	}, nil
}
