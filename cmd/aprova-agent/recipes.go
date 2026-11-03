package main

import (
	_ "embed"
	"os"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/wire"
)

//go:embed recipes/pixelfed.php
var pixelfedPHP []byte

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
