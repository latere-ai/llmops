package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/latere-ai/llmops/internal/manifest"
	"github.com/latere-ai/llmops/internal/runtime"
)

// DefaultPort is what `llmops serve` listens on unless told otherwise.
const DefaultPort = 8000

// Model is one installed model and what it is doing right now.
type Model struct {
	Name    string
	Runtime string
	GPU     string
	Port    int
	State   string  // ready | loading | down
	Loaded  float64 // seconds spent preparing weights; 0 if unknown

	// Speculator is the draft-model configuration the running process
	// was started with. It is read from the endpoint rather than the
	// manifest because the manifest only offers a default: an operator
	// can start the same model with a different draft head, and the
	// manifest would then describe something that is not running
	// (specs/027).
	Speculator string `json:",omitempty"`
}

// Ready reports whether this model can serve a request.
func (m Model) Ready() bool { return m.State == "ready" }

// Discover lists the models installed on this host and probes each one.
//
// It reads the installed manifests rather than asking systemd, so it
// works on a host where the process was started by hand, and it reports
// what is *answering* rather than what is supposed to be.
//
// The port comes from the unit's ExecStart, not from the manifest: a
// port is a property of the deployment, and the unit already carries it.
// Growing a port field on the manifest would put the same fact in two
// places.
func Discover(configDir, unitDir string, timeout time.Duration) ([]Model, error) {
	ms, err := manifest.LoadDir(configDir)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(ms))
	for _, m := range ms {
		mod := Model{
			Name:    m.Name,
			Runtime: m.Runtime,
			GPU:     fmt.Sprintf("%dx%s", m.GPU.Count, m.GPU.Type),
			Port:    portFromUnit(filepath.Join(unitDir, m.Name+".service")),
		}
		mod.State, mod.Loaded, mod.Speculator = probe(mod.Name, mod.Port, timeout)
		out = append(out, mod)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var portRe = regexp.MustCompile(`--port[= ](\d+)`)

// portFromUnit reads --port out of a unit's ExecStart, falling back to
// the serve default when the unit is absent or does not set one.
func portFromUnit(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return DefaultPort
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		if m := portRe.FindStringSubmatch(rest); m != nil {
			if p, err := strconv.Atoi(m[1]); err == nil {
				return p
			}
		}
		break
	}
	return DefaultPort
}

// probe asks the shim what it is doing. A model that is installed but
// not answering is reported as down rather than omitted: "configured
// but not running" is the state an operator most needs to see.
//
// It also checks *identity*. Without a unit a model falls back to the
// default port, so several manifests can point at one address — and
// whatever is serving there would otherwise be reported as all of them.
// Asking the endpoint which model it serves is what makes `ps` report
// reality rather than a coincidence of port numbers.
func probe(name string, port int, timeout time.Duration) (state string, loaded float64, speculator string) {
	c := &http.Client{Timeout: timeout}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	resp, err := c.Get(base + "/ready")
	if err != nil {
		return "down", 0, ""
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	// The shim reports the active draft head on every response, so the
	// answer arrives with the readiness check rather than costing a
	// second request (specs/027).
	speculator = resp.Header.Get(runtime.SpeculatorHeader)
	state = strings.TrimSpace(string(body))
	if state == "" {
		state = "down"
	}
	if resp.StatusCode != http.StatusOK && state != "loading" {
		state = "down"
	}
	if state == "ready" && !serves(c, base, name) {
		// Something is up on this port, but not this model.
		return "down", 0, ""
	}
	return state, weightsLoaded(c, base), speculator
}

// serves reports whether the endpoint is serving this model. An engine
// that does not answer /v1/models gets the benefit of the doubt: the
// check exists to catch a wrong model, not to demand a surface.
func serves(c *http.Client, base, name string) bool {
	resp, err := c.Get(base + "/v1/models")
	if err != nil {
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return true
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || len(payload.Data) == 0 {
		return true
	}
	for _, m := range payload.Data {
		if m.ID == name {
			return true
		}
	}
	return false
}

// weightsLoaded pulls llmops_weights_load_seconds out of /metrics — the
// gauge the shim already exports, so `ps` needs no new plumbing.
func weightsLoaded(c *http.Client, base string) float64 {
	resp, err := c.Get(base + "/metrics")
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for line := range strings.SplitSeq(string(body), "\n") {
		if v, ok := strings.CutPrefix(line, "llmops_weights_load_seconds "); ok {
			f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return f
		}
	}
	return 0
}

// EndpointFor builds the address a harness should be pointed at.
func EndpointFor(m Model, host, token string) Endpoint {
	if host == "" {
		host = "127.0.0.1"
	}
	return Endpoint{
		BaseURL: fmt.Sprintf("http://%s:%d", host, m.Port),
		Model:   m.Name,
		Token:   token,
	}
}
