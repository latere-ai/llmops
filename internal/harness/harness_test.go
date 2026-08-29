package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

func testEndpoint() Endpoint {
	return Endpoint{BaseURL: "http://box:8000", Model: "qwen3.8-27b", Token: "local"}
}

// TestRegistryIsData is specs/026 AC5: adding a harness must be a row,
// not a branch. Every entry is exercised through the same three calls,
// so a harness that needed special handling would fail here rather than
// quietly grow a code path.
func TestRegistryIsData(t *testing.T) {
	if len(Registry) < 3 {
		t.Fatalf("registry has %d harnesses, expected the three named in specs/026", len(Registry))
	}
	for _, h := range Registry {
		t.Run(h.Name, func(t *testing.T) {
			e := testEndpoint()
			vars, err := h.EnvVars(e)
			if err != nil {
				t.Fatalf("EnvVars: %v", err)
			}
			if len(vars) == 0 {
				t.Fatal("no environment variables")
			}
			sh, err := h.Shell(e)
			if err != nil {
				t.Fatalf("Shell: %v", err)
			}
			if !strings.HasPrefix(sh, "export ") {
				t.Fatalf("shell output is not eval-able: %q", sh)
			}
			def, err := h.Default(e)
			if err != nil {
				t.Fatalf("Default: %v", err)
			}
			if !strings.Contains(def, "box:8000") {
				t.Fatalf("default output does not carry the endpoint:\n%s", def)
			}
		})
	}
}

// TestBaseSuffixIsAnSDKConvention pins the one real difference between
// the harnesses: an Anthropic client appends /v1/messages to a bare
// base, an OpenAI client appends /chat/completions to one ending /v1.
// Both surfaces are served either way (specs/025), so this is about the
// client's expectation, not ours.
func TestBaseSuffixIsAnSDKConvention(t *testing.T) {
	cases := map[string]string{
		"claude":   "http://box:8000",
		"codex":    "http://box:8000/v1",
		"opencode": "http://box:8000/v1",
	}
	for name, want := range cases {
		h, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := h.Default(testEndpoint())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: output does not contain base %q:\n%s", name, want, got)
		}
		// The bare base must not appear where a /v1 base is required,
		// which a naive substring check would miss.
		if want != "http://box:8000" && strings.Contains(got, `"http://box:8000"`) {
			t.Errorf("%s: emitted a bare base URL where /v1 is required", name)
		}
	}
}

func TestClaudeEmitsTheVariablesClaudeReads(t *testing.T) {
	h, err := Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := h.Shell(testEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export ANTHROPIC_BASE_URL=http://box:8000\n",
		"export ANTHROPIC_AUTH_TOKEN=local\n",
		"export ANTHROPIC_MODEL=qwen3.8-27b\n",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("missing %q in:\n%s", want, sh)
		}
	}
}

func TestOpencodeConfigIsValidJSON(t *testing.T) {
	// A config a person pastes must parse, or the failure lands on them
	// at a moment when nothing points back here.
	h, err := Lookup("opencode")
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Config(testEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("opencode config is not valid JSON: %v\n%s", err, out)
	}
	prov, _ := v["provider"].(map[string]any)
	llmops, _ := prov["llmops"].(map[string]any)
	opts, _ := llmops["options"].(map[string]any)
	if opts["baseURL"] != "http://box:8000/v1" {
		t.Fatalf("baseURL is %v", opts["baseURL"])
	}
}

func TestCodexConfigNamesTheProvider(t *testing.T) {
	h, err := Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Config(testEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`model_provider = "llmops"`,
		`base_url = "http://box:8000/v1"`,
		`wire_api = "chat"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("codex config missing %q:\n%s", want, out)
		}
	}
}

func TestUnknownHarnessListsTheKnownOnes(t *testing.T) {
	_, err := Lookup("aider")
	if err == nil {
		t.Fatal("unknown harness accepted")
	}
	for _, n := range Names() {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("error does not mention %q: %v", n, err)
		}
	}
}

func TestShellQuotingSurvivesAwkwardValues(t *testing.T) {
	h, err := Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := h.Shell(Endpoint{BaseURL: "http://box:8000", Model: "a b'c", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sh, `'a b'\''c'`) {
		t.Fatalf("value was not quoted for eval:\n%s", sh)
	}
}

func TestConfigRefusedForAnEnvOnlyHarness(t *testing.T) {
	// claude has no config file; asking for one must say so rather than
	// emit an empty file someone then wonders about.
	h, err := Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Config(testEndpoint()); err == nil ||
		!strings.Contains(err.Error(), "environment only") {
		t.Fatalf("Config on an env-only harness: %v", err)
	}
}

func TestRenderRejectsABrokenTemplate(t *testing.T) {
	// A registry row is data, but a malformed template in one must fail
	// loudly rather than emit a half-rendered config.
	h := Harness{Name: "broken", Env: []Var{{"K", "{{.Nope}}"}}}
	if _, err := h.EnvVars(testEndpoint()); err == nil {
		t.Fatal("a template naming an unknown field rendered")
	}
	h2 := Harness{Name: "broken", Env: []Var{{"K", "{{"}}}
	if _, err := h2.Shell(testEndpoint()); err == nil {
		t.Fatal("an unparseable template rendered")
	}
}

func TestEndpointForDefaultsTheHost(t *testing.T) {
	// An empty host means "this machine": ps and endpoint probe over
	// loopback, so that is the honest default.
	e := EndpointFor(Model{Name: "m", Port: 8000}, "", "tok")
	if e.BaseURL != "http://127.0.0.1:8000" {
		t.Fatalf("BaseURL %q", e.BaseURL)
	}
}
