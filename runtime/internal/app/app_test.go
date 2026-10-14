package app_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/app"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/llmtest"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp/mcptest"
)

func start(t *testing.T, cfg string, env map[string]string) *httptest.Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	a := app.Start(ctx, app.Options{
		ConfigPath: path, Getenv: func(k string) string { return env[k] },
		RetryInterval: 20 * time.Millisecond,
	})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func invoke(t *testing.T, base, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+"/v1/invoke", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return resp.StatusCode, m
}

func waitReady(t *testing.T, base string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if code, m := get(t, base+"/readyz"); code == 200 {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	code, m := get(t, base+"/readyz")
	t.Fatalf("never ready: %d %v", code, m)
	return nil
}

func errKind(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	k, _ := e["kind"].(string)
	return k
}

func TestEndToEndOverHTTP(t *testing.T) {
	type setup struct {
		name     string
		provider string
		endpoint func(*testing.T, ...llmtest.Step) string
		env      map[string]string
		keyEnv   string
	}
	setups := []setup{
		{"anthropic", "anthropic", func(t *testing.T, s ...llmtest.Step) string { return llmtest.NewAnthropic(t, s...).URL }, map[string]string{"KEY": "k"}, "KEY"},
		{"openai-compatible", "openai-compatible", func(t *testing.T, s ...llmtest.Step) string { return llmtest.NewOpenAI(t, s...).URL + "/v1" }, nil, ""},
		{"vllm alias", "vllm", func(t *testing.T, s ...llmtest.Step) string { return llmtest.NewOpenAI(t, s...).URL + "/v1" }, nil, ""},
	}
	for _, s := range setups {
		t.Run(s.name, func(t *testing.T) {
			tool := mcptest.New(t, false, mcptest.Tool{Name: "search", Handler: func(json.RawMessage) (string, bool) { return "doc text", false }})
			endpoint := s.endpoint(t,
				llmtest.Step{Calls: []llmtest.Call{{ID: "c1", Name: "docs__search", Args: `{}`}}},
				llmtest.Step{Text: "the answer"})
			cfg := fmt.Sprintf(`{"model":{"provider":%q,"name":"m","endpoint":%q,"apiKeyEnv":%q},
			  "instructions":"help","mcpServers":[{"name":"docs","url":%q}]}`, s.provider, endpoint, s.keyEnv, tool.URL)
			srv := start(t, cfg, s.env)
			ready := waitReady(t, srv.URL)
			if ready["configHash"] == "" {
				t.Fatal("readyz must report the config hash")
			}
			code, body := invoke(t, srv.URL, `{"input":"what?"}`)
			if code != 200 || body["output"] != "the answer" {
				t.Fatalf("code=%d body=%v", code, body)
			}
			if tool.Calls("search") != 1 {
				t.Fatal("tool was not called")
			}
		})
	}
}

func TestInvalidConfigurationPreventsExecution(t *testing.T) {
	cases := map[string]struct {
		cfg    string
		env    map[string]string
		reason string
		kind   string
	}{
		"malformed":        {`{`, nil, "cannot parse", "invalid_config"},
		"unknown field":    {`{"model":{"provider":"anthropic","name":"m","apiKeyEnv":"K"},"instructions":"x","extra":1}`, map[string]string{"K": "k"}, "unknown field", "invalid_config"},
		"unresolved model": {`{"model":{"provider":"anthropic","name":"m"},"instructions":"x","unresolved":[{"kind":"Model","name":"gone","reason":"not found"}]}`, nil, `Model "gone": not found`, "invalid_config"},
		"unknown provider": {`{"model":{"provider":"nope","name":"m"},"instructions":"x"}`, nil, `unknown provider "nope"`, "invalid_config"},
		"missing key":      {`{"model":{"provider":"anthropic","name":"m","apiKeyEnv":"K"},"instructions":"x"}`, nil, "K is empty or unset", "invalid_config"},
		"not implemented":  {`{"model":{"provider":"bedrock","name":"m"},"instructions":"x"}`, nil, "not implemented", "unsupported"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := start(t, tc.cfg, tc.env)
			time.Sleep(100 * time.Millisecond) // let initialisation settle
			code, m := get(t, srv.URL+"/readyz")
			if code != 503 || !strings.Contains(fmt.Sprint(m["reason"]), tc.reason) {
				t.Fatalf("readyz: %d %v", code, m)
			}
			code, m = invoke(t, srv.URL, `{"input":"hi"}`)
			if code != 503 || errKind(m) != tc.kind {
				t.Fatalf("invoke: %d %v", code, m)
			}
			if !strings.Contains(fmt.Sprint(m), tc.reason) {
				t.Fatalf("reason not surfaced to caller: %v", m)
			}
			if c, _ := get(t, srv.URL+"/healthz"); c != 200 {
				t.Fatal("liveness must stay up so the pod is not restarted into a loop")
			}
		})
	}
}

func TestUnreachableToolServerIsNotReady(t *testing.T) {
	srv := start(t, `{"model":{"provider":"openai-compatible","name":"m","endpoint":"http://127.0.0.1:1/v1"},
	  "instructions":"x","mcpServers":[{"name":"docs","url":"http://127.0.0.1:1/mcp"}]}`, nil)
	time.Sleep(100 * time.Millisecond)
	code, m := get(t, srv.URL+"/readyz")
	if code != 503 || !strings.Contains(fmt.Sprint(m["reason"]), "waiting for tool servers") {
		t.Fatalf("%d %v", code, m)
	}
}

func TestBecomesReadyWhenToolServerAppears(t *testing.T) {
	var up atomic.Bool
	var inner http.Handler
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(flaky.Close)
	real := mcptest.New(t, false, mcptest.Tool{Name: "search", Handler: func(json.RawMessage) (string, bool) { return "x", false }})
	inner = real.Config.Handler
	model := llmtest.NewOpenAI(t)
	srv := start(t, fmt.Sprintf(`{"model":{"provider":"openai-compatible","name":"m","endpoint":%q},
	  "instructions":"x","mcpServers":[{"name":"docs","url":%q}]}`, model.URL+"/v1", flaky.URL), nil)
	time.Sleep(100 * time.Millisecond)
	if code, _ := get(t, srv.URL+"/readyz"); code != 503 {
		t.Fatal("must not be ready while the tool server is down")
	}
	up.Store(true)
	waitReady(t, srv.URL)
}

func TestBadRequestBodies(t *testing.T) {
	model := llmtest.NewOpenAI(t)
	srv := start(t, fmt.Sprintf(`{"model":{"provider":"openai-compatible","name":"m","endpoint":%q},"instructions":"x"}`, model.URL+"/v1"), nil)
	waitReady(t, srv.URL)
	for name, body := range map[string]string{
		"not json":      `nope`,
		"unknown field": `{"input":"x","system":"override"}`,
		"empty input":   `{"input":""}`,
	} {
		code, m := invoke(t, srv.URL, body)
		if code != 400 || errKind(m) != "bad_request" {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
}

func TestDrainMakesReadyzFail(t *testing.T) {
	model := llmtest.NewOpenAI(t)
	path := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(path, []byte(fmt.Sprintf(`{"model":{"provider":"openai-compatible","name":"m","endpoint":%q},"instructions":"x"}`, model.URL+"/v1")), 0o600)
	a := app.Start(t.Context(), app.Options{ConfigPath: path, Getenv: func(string) string { return "" }})
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	waitReady(t, srv.URL)
	a.Drain()
	if code, m := get(t, srv.URL+"/readyz"); code != 503 || m["reason"] != "shutting down" {
		t.Fatalf("%d %v", code, m)
	}
}
