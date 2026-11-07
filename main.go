// aprova approves sign-ups across apps from one queue. The core knows no app:
// each one is a module, and the configuration turns on the ones to load.
// See README.md.
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"gitlab.gutocarvalho.net/open/aprova/core"
	"gitlab.gutocarvalho.net/open/aprova/internal/healthcheck"
	"gitlab.gutocarvalho.net/open/aprova/internal/relnotes"
	_ "gitlab.gutocarvalho.net/open/aprova/modules/all"
)

// The version is X.Y.Z-BUILD: X.Y.Z from the VERSION file, BUILD the CI pipeline
// number, set at link time. A local build shows X.Y.Z-dev.
//
//go:embed VERSION
var versionFile string

// The release notes page and /release-notes.json come from CHANGELOG.md.
//
//go:embed CHANGELOG.md
var changelog string

var build = "dev"

func main() {
	probe := flag.Bool("healthcheck", false, "probe the local server and exit")
	path := flag.String("config", os.Getenv("APROVA_CONFIG"), "configuration file")
	flag.Parse()
	listen := os.Getenv("APROVA_LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	if *probe {
		if err := healthcheck.Probe(listen); err != nil {
			log.Fatal(err)
		}
		return
	}
	core.Version = strings.TrimSpace(versionFile) + "-" + build
	core.Releases = relnotes.MustParse(changelog)
	if *path == "" {
		log.Fatal("set APROVA_CONFIG or -config")
	}
	cfg, err := core.LoadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	srv, err := core.New(ctx, cfg, core.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()
	srv.Run(ctx)
	log.Printf("aprova %s listening on %s, modules %v", core.Version, cfg.Listen, cfg.Enabled())
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		done, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hs.Shutdown(done)
	}()
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
