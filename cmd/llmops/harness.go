// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/latere-ai/llmops/internal/harness"
	"github.com/latere-ai/llmops/internal/install"

	"context"
	"errors"

	"latere.ai/x/pkg/wait"
)

// lookHarness and execHarness are the two steps that leave the program:
// finding the harness on PATH, and becoming it. Both are variables so a
// test can stand in for them.
//
// Injecting only the second was not enough, and failed in both
// directions: locally `claude` is on PATH, so the real exec replaced the
// test binary; on CI it is absent, so the lookup failed before the stub
// was ever reached. A half-injected boundary is worse than none, because
// it passes wherever it was written.
var (
	lookHarness = exec.LookPath
	execHarness = syscall.Exec
)

// defaultToken is what we hand a harness that insists on one. The shim
// has no authentication (specs/026); this exists so a client does not
// fall back to its own login flow, not to protect anything.
const defaultToken = "local"

func hostFlags(fs *flag.FlagSet) (configDir, unitDir *string) {
	configDir = fs.String("config-dir", install.DefaultConfigDir, "where installed manifests live")
	unitDir = fs.String("unit-dir", install.DefaultUnitDir, "where units live")
	return
}

// runPS lists the models installed on this host and what each is doing.
func runPS(rest []string, out, errw io.Writer) error {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	fs.SetOutput(errw)
	configDir, unitDir := hostFlags(fs)
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	timeout := fs.Duration("timeout", 2*time.Second, "per-model probe timeout")
	if err := fs.Parse(rest); err != nil {
		return usagef("usage: llmops ps [--json]")
	}
	models, err := harness.Discover(context.Background(), *configDir, *unitDir, *timeout)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(models)
	}
	if len(models) == 0 {
		_, _ = fmt.Fprintf(out, "no models installed in %s\n", *configDir)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tPORT\tRUNTIME\tGPU\tSPECULATOR\tLOADED")
	for _, m := range models {
		loaded := "-"
		if m.Loaded > 0 {
			loaded = fmt.Sprintf("%.1fs", m.Loaded)
		}
		// A model that is down reports nothing. "-" reads as "not
		// known", where "none" would claim speculation is off.
		spec := "-"
		if m.Speculator != "" {
			spec = m.Speculator
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", m.Name, m.State, m.Port, m.Runtime, m.GPU, spec, loaded)
	}
	return tw.Flush()
}

// resolveModel finds one installed model by name, or the only one.
func resolveModel(ctx context.Context, configDir, unitDir, name string, timeout time.Duration) (harness.Model, error) {
	models, err := harness.Discover(ctx, configDir, unitDir, timeout)
	if err != nil {
		return harness.Model{}, err
	}
	if name == "" {
		if len(models) == 1 {
			return models[0], nil
		}
		return harness.Model{}, usagef("--model is required when %d models are installed", len(models))
	}
	for _, m := range models {
		if m.Name == name {
			return m, nil
		}
	}
	return harness.Model{}, fmt.Errorf("no installed model named %q", name)
}

// runEndpoint prints the configuration a harness needs.
func runEndpoint(rest []string, out, errw io.Writer) error {
	fs := flag.NewFlagSet("endpoint", flag.ContinueOnError)
	fs.SetOutput(errw)
	configDir, unitDir := hostFlags(fs)
	name := fs.String("harness", "", "harness to configure: "+strings.Join(harness.Names(), ", "))
	model := fs.String("model", "", "model to point it at (default: the only installed one)")
	format := fs.String("format", "", "override the output shape: env|json|toml")
	host := fs.String("host", "127.0.0.1", "host a client should reach this endpoint on")
	token := fs.String("token", defaultToken, "token to emit; the endpoint has no auth")
	timeout := fs.Duration("timeout", 2*time.Second, "per-model probe timeout")
	if err := fs.Parse(rest); err != nil || *name == "" {
		return usagef("usage: llmops endpoint --harness <%s> [--model <name>]",
			strings.Join(harness.Names(), "|"))
	}
	h, err := harness.Lookup(*name)
	if err != nil {
		return err
	}
	m, err := resolveModel(context.Background(), *configDir, *unitDir, *model, *timeout)
	if err != nil {
		return err
	}
	ep := harness.EndpointFor(m, *host, *token)

	var rendered string
	switch harness.Format(*format) {
	case "":
		rendered, err = h.Default(ep)
	case harness.FormatEnv:
		rendered, err = h.Shell(ep)
	case harness.FormatJSON, harness.FormatTOML:
		rendered, err = h.Config(ep)
	default:
		return usagef("--format %q must be one of env|json|toml", *format)
	}
	if err != nil {
		return err
	}

	// Warnings go to stderr so `eval "$(llmops endpoint …)"` stays clean.
	_, _ = fmt.Fprintf(errw, "# %s is unauthenticated; the token below is a placeholder (specs/026)\n", ep.BaseURL)
	if !m.Ready() {
		_, _ = fmt.Fprintf(errw, "# warning: %s is %s, not ready\n", m.Name, m.State)
	}
	if h.ConfigFile != "" && harness.Format(*format) != harness.FormatEnv {
		_, _ = fmt.Fprintf(errw, "# write this to %s\n", h.ConfigFile)
	}
	_, _ = fmt.Fprint(out, rendered)
	return nil
}

// runHarness launches a harness against a served model.
func runHarness(rest []string, out, errw io.Writer) error {
	// Everything after -- belongs to the harness, untouched.
	var passthrough []string
	for i, a := range rest {
		if a == "--" {
			passthrough = rest[i+1:]
			rest = rest[:i]
			break
		}
	}
	name, flags := popPositional(rest)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(errw)
	configDir, unitDir := hostFlags(fs)
	model := fs.String("model", "", "model to point it at (default: the only installed one)")
	host := fs.String("host", "127.0.0.1", "host the harness should reach")
	token := fs.String("token", defaultToken, "token to set; the endpoint has no auth")
	wait := fs.Duration("wait", 0, "wait this long for a loading model to become ready")
	timeout := fs.Duration("timeout", 2*time.Second, "per-model probe timeout")
	if err := fs.Parse(flags); err != nil || name == "" {
		return usagef("usage: llmops run <%s> [--model <name>] [-- harness args]",
			strings.Join(harness.Names(), "|"))
	}
	h, err := harness.Lookup(name)
	if err != nil {
		return err
	}
	m, err := resolveModel(context.Background(), *configDir, *unitDir, *model, *timeout)
	if err != nil {
		return err
	}
	if m, err = awaitReady(*configDir, *unitDir, m, *wait, *timeout, errw); err != nil {
		return err
	}
	vars, err := h.EnvVars(harness.EndpointFor(m, *host, *token))
	if err != nil {
		return err
	}
	bin, err := lookHarness(h.Name)
	if err != nil {
		return fmt.Errorf("%s is not on PATH: %w", h.Name, err)
	}
	env := os.Environ()
	for _, kv := range vars {
		env = append(env, kv[0]+"="+kv[1])
	}
	if h.ConfigFile != "" {
		_, _ = fmt.Fprintf(errw, "# %s also reads %s; `llmops endpoint --harness %s` prints it\n",
			h.Name, h.ConfigFile, h.Name)
	}
	// Replace this process: signals, exit codes and the terminal then
	// behave exactly as running the harness directly.
	return execHarness(bin, append([]string{h.Name}, passthrough...), env)
}

// awaitReady blocks on a model that is still loading, when asked. It
// never starts a stopped one: a verb that silently triggers a
// ten-minute weight load because a name was typed is doing something
// nobody asked for (specs/026).
func awaitReady(configDir, unitDir string, m harness.Model, budget, timeout time.Duration, errw io.Writer) (harness.Model, error) {
	if m.Ready() {
		return m, nil
	}
	if budget <= 0 || m.State == "down" {
		return m, fmt.Errorf("%s is %s; start it with `systemctl start %s.service`, "+
			"or pass --wait to block on one that is loading", m.Name, m.State, m.Name)
	}
	_, _ = fmt.Fprintf(errw, "# %s is %s; waiting up to %s\n", m.Name, m.State, budget)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	got := m
	err := wait.Until(ctx, 2*time.Second, func(ctx context.Context) (bool, error) {
		var err error
		if got, err = resolveModel(ctx, configDir, unitDir, m.Name, timeout); err != nil {
			return false, err
		}
		if got.State == "down" {
			return false, fmt.Errorf("%s went down while waiting", m.Name)
		}
		return got.Ready(), nil
	})
	switch {
	case err == nil:
		return got, nil
	case errors.Is(err, context.DeadlineExceeded):
		return m, fmt.Errorf("%s did not become ready within %s", m.Name, budget)
	default:
		return got, err
	}
}
