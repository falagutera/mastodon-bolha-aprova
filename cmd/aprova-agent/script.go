package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gitlab.gutocarvalho.net/open/aprova/wire"
)

// script runs a recipe file shipped inside this binary with the app's own
// interpreter, in the app's own image. The file answers one action per run on
// stdout, as one JSON object:
//
//	pending     -> {"requests": [...]}
//	get <id>    -> {"request": {...}, "pending": true} or {"request": null}
//	approve|reject <id> -> {"ok": true} or {"gone": true}
//
// and {"error": "..."} when it fails.
type script struct {
	recipeName string
	interp     []string // e.g. php, or python3 manage.py shell -c (with the file read in)
	path       string
	dir        string
	env        []string
	timeout    time.Duration
	capsValue  wire.Caps
}

// writeScript puts the recipe where the interpreter can read it.
func writeScript(name string, body []byte) (string, error) {
	dir := os.Getenv("APROVA_AGENT_TMP")
	if dir == "" {
		dir = os.TempDir()
	}
	dir, err := os.MkdirTemp(dir, "aprova-agent-")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	return p, os.WriteFile(p, body, 0o444)
}

type scriptOut struct {
	Requests []wire.Request `json:"requests"`
	Request  *wire.Request  `json:"request"`
	Pending  bool           `json:"pending"`
	OK       bool           `json:"ok"`
	Gone     bool           `json:"gone"`
	Error    string         `json:"error"`
}

func (s *script) run(ctx context.Context, args ...string) (scriptOut, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	argv := append(append(append([]string{}, s.interp[1:]...), s.path), args...)
	cmd := exec.CommandContext(ctx, s.interp[0], argv...)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), s.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{b: &stdout, n: 8 << 20}, &limited{b: &stderr, n: 1 << 16}
	err := cmd.Run()
	var out scriptOut
	// The last line is the answer: frameworks sometimes print before it.
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
	if jerr := json.Unmarshal(lines[len(lines)-1], &out); jerr != nil {
		return out, fmt.Errorf("%s %v: %v; stderr: %s", s.recipeName, args, errors.Join(err, jerr), bytes.TrimSpace(stderr.Bytes()))
	}
	if out.Error != "" {
		return out, fmt.Errorf("%s %v: %s", s.recipeName, args, out.Error)
	}
	if err != nil {
		return out, fmt.Errorf("%s %v: %v; stderr: %s", s.recipeName, args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

type limited struct {
	b *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

func (s *script) name() string    { return s.recipeName }
func (s *script) caps() wire.Caps { return s.capsValue }

func (s *script) pending(ctx context.Context) ([]wire.Request, error) {
	out, err := s.run(ctx, "pending")
	if err != nil {
		return nil, err
	}
	return out.Requests, nil
}

func (s *script) get(ctx context.Context, id string) (wire.Request, bool, error) {
	out, err := s.run(ctx, "get", id)
	if err != nil {
		return wire.Request{}, false, err
	}
	if out.Request == nil {
		return wire.Request{}, false, errGone
	}
	return *out.Request, out.Pending, nil
}

func (s *script) decide(ctx context.Context, action, id string) error {
	out, err := s.run(ctx, action, id)
	if err != nil {
		return err
	}
	if out.Gone {
		return errGone
	}
	if !out.OK {
		return fmt.Errorf("%s %s %s: no answer", s.recipeName, action, id)
	}
	return nil
}

func (s *script) approve(ctx context.Context, id string) error { return s.decide(ctx, "approve", id) }
func (s *script) reject(ctx context.Context, id string) error  { return s.decide(ctx, "reject", id) }
