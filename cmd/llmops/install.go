package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/latere-ai/llmops/internal/install"
	"github.com/latere-ai/llmops/internal/manifest"
)

// runInstall places a bare-metal model's unit and manifest on this host
// (specs/020). One command writes both, so the documented procedure
// cannot drift from what actually gets installed.
func runInstall(rest []string, out, errw io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(errw)
	path := fs.String("manifest", "", "path to models/<name>.yaml")
	binPath := fs.String("bin", install.DefaultBinPath, "installed llmops binary")
	configDir := fs.String("config-dir", install.DefaultConfigDir, "where the manifest is placed")
	unitDir := fs.String("unit-dir", install.DefaultUnitDir, "where the unit is written")
	cacheRoot := fs.String("cache-root", "", "weights root passed to serve (omitted if empty)")
	user := fs.String("user", "", "run the service as this user")
	print := fs.Bool("print", false, "write nothing; print the unit that would be installed")
	noReload := fs.Bool("no-reload", false, "write the files but do not run systemctl daemon-reload")
	if err := fs.Parse(rest); err != nil || *path == "" {
		return usagef("usage: llmops install --manifest <manifest.yaml> [--print]")
	}
	m, err := manifest.Load(*path)
	if err != nil {
		return err
	}
	opts := install.Options{
		BinPath:   *binPath,
		ConfigDir: *configDir,
		UnitDir:   *unitDir,
		CacheRoot: *cacheRoot,
		User:      *user,
	}
	if *noReload {
		// Staging units for another machine, or installing somewhere
		// this process has no business reloading.
		opts.Reload = func(io.Writer) error { return nil }
	}
	if *print {
		if m.DeployMode() != manifest.DeployBareMetal {
			return fmt.Errorf("%s is deploy: %s; only bare-metal models have a unit", m.Name, m.DeployMode())
		}
		fmt.Fprint(out, install.Unit(m, opts))
		return nil
	}
	res, err := install.Run(m, *path, opts, out)
	if err != nil {
		return err
	}
	if !res.ManifestChanged && !res.UnitChanged {
		fmt.Fprintf(out, "%s already installed; nothing to do\n", m.Name)
		return nil
	}
	fmt.Fprintf(out, "installed %s — start it with: systemctl enable --now %s\n",
		m.Name, install.UnitName(m))
	return nil
}
