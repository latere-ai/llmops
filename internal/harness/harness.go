// Package harness renders the configuration a coding agent needs to
// talk to a model we serve (specs/026-harness-integration.md).
//
// Every harness speaks one of the dialects the shim already serves
// (specs/025), so this package does no routing and makes no decision
// about surfaces. What differs between harnesses is only how they are
// *told* where to look: the variable names, the file format, and
// whether their SDK expects a base URL ending in /v1.
//
// That difference is data. A new harness is a row in Registry, and
// TestRegistryIsData pins that adding one needs no new code path.
package harness

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// Endpoint is what a harness must be pointed at.
type Endpoint struct {
	BaseURL string // scheme://host:port, no path
	Model   string // the manifest name the engine serves under
	Token   string // placeholder; the shim has no auth (specs/026)
}

// Format is how a harness consumes its configuration.
type Format string

const (
	// FormatEnv is shell exports, eval-able.
	FormatEnv Format = "env"
	// FormatJSON is a config file the user pastes.
	FormatJSON Format = "json"
	// FormatTOML is a config file the user pastes.
	FormatTOML Format = "toml"
)

// Harness is one coding agent's configuration shape.
type Harness struct {
	Name string
	// Dialect is the surface it speaks, for documentation and for the
	// suffix decision below. All three are served (specs/025), so this
	// never gates anything.
	Dialect string
	// BaseSuffix is appended to the endpoint's base URL. It is an SDK
	// convention, not a property of what we serve: an Anthropic client
	// appends /v1/messages to a bare base, while an OpenAI client
	// appends /chat/completions to one already ending in /v1.
	BaseSuffix string
	// Env is the variable set, in emit order. Values are templates over
	// Endpoint.
	Env []Var
	// ConfigFile names the file a harness reads when it cannot be
	// configured by environment alone; empty means env is enough.
	ConfigFile string
	// ConfigFormat and ConfigTmpl render that file.
	ConfigFormat Format
	ConfigTmpl   string
}

// Var is one environment variable and the template for its value.
type Var struct {
	Key  string
	Tmpl string
}

// Registry is every harness we can configure. Adding one is a row.
var Registry = []Harness{
	{
		Name:       "claude",
		Dialect:    "anthropic-messages",
		BaseSuffix: "",
		Env: []Var{
			{"ANTHROPIC_BASE_URL", "{{.BaseURL}}"},
			{"ANTHROPIC_AUTH_TOKEN", "{{.Token}}"},
			{"ANTHROPIC_MODEL", "{{.Model}}"},
		},
	},
	{
		Name:       "codex",
		Dialect:    "openai-chat",
		BaseSuffix: "/v1",
		Env: []Var{
			{"OPENAI_BASE_URL", "{{.BaseURL}}"},
			{"OPENAI_API_KEY", "{{.Token}}"},
		},
		ConfigFile:   "~/.codex/config.toml",
		ConfigFormat: FormatTOML,
		ConfigTmpl: `# llmops: {{.Model}}
model = "{{.Model}}"
model_provider = "llmops"

[model_providers.llmops]
name = "llmops"
base_url = "{{.BaseURL}}"
env_key = "OPENAI_API_KEY"
wire_api = "chat"
`,
	},
	{
		Name:       "opencode",
		Dialect:    "openai-chat",
		BaseSuffix: "/v1",
		// opencode has no environment variable for the base URL; the
		// provider block is the only way in. The key is interpolated
		// from the environment so the file stays committable.
		Env: []Var{
			{"LLMOPS_API_KEY", "{{.Token}}"},
		},
		ConfigFile:   "opencode.json",
		ConfigFormat: FormatJSON,
		ConfigTmpl: `{
  "provider": {
    "llmops": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "llmops",
      "options": {
        "baseURL": "{{.BaseURL}}",
        "apiKey": "{env:LLMOPS_API_KEY}"
      },
      "models": {
        "{{.Model}}": { "name": "{{.Model}}" }
      }
    }
  }
}
`,
	},
}

// Names lists the known harnesses, sorted.
func Names() []string {
	out := make([]string, len(Registry))
	for i, h := range Registry {
		out[i] = h.Name
	}
	sort.Strings(out)
	return out
}

// Lookup finds a harness by name.
func Lookup(name string) (*Harness, error) {
	for i := range Registry {
		if Registry[i].Name == name {
			return &Registry[i], nil
		}
	}
	return nil, fmt.Errorf("unknown harness %q; known: %s", name, strings.Join(Names(), ", "))
}

// resolve applies the harness's base-URL convention to an endpoint.
func (h *Harness) resolve(e Endpoint) Endpoint {
	e.BaseURL = strings.TrimSuffix(e.BaseURL, "/") + h.BaseSuffix
	return e
}

// EnvVars returns the variables this harness reads, values rendered.
func (h *Harness) EnvVars(e Endpoint) ([][2]string, error) {
	e = h.resolve(e)
	out := make([][2]string, 0, len(h.Env))
	for _, v := range h.Env {
		val, err := render(v.Tmpl, e)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", v.Key, err)
		}
		out = append(out, [2]string{v.Key, val})
	}
	return out, nil
}

// Shell renders eval-able exports.
func (h *Harness) Shell(e Endpoint) (string, error) {
	vars, err := h.EnvVars(e)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, kv := range vars {
		_, _ = fmt.Fprintf(&b, "export %s=%s\n", kv[0], shellQuote(kv[1]))
	}
	return b.String(), nil
}

// Config renders the harness's config file, if it has one.
func (h *Harness) Config(e Endpoint) (string, error) {
	if h.ConfigTmpl == "" {
		return "", fmt.Errorf("%s is configured by environment only", h.Name)
	}
	return render(h.ConfigTmpl, h.resolve(e))
}

// Default renders whatever this harness actually reads: its config file
// when it has one, otherwise shell exports.
func (h *Harness) Default(e Endpoint) (string, error) {
	if h.ConfigTmpl != "" {
		return h.Config(e)
	}
	return h.Shell(e)
}

func render(tmpl string, e Endpoint) (string, error) {
	t, err := template.New("h").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, e); err != nil {
		return "", err
	}
	return b.String(), nil
}

// shellQuote makes a value safe to eval. Endpoint values are ours, but
// a model name or token is caller-supplied often enough that quoting is
// the cheaper habit.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return r != '-' && r != '_' && r != '.' && r != '/' && r != ':' &&
			(r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
