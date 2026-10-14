package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/agent"
	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/anthropic"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/fake"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/llmtest"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm/openai"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp/mcptest"
	"github.com/leeclarkuk/openkindred/runtime/internal/tools"
)

// adapter builds a real provider adapter against a scripted local server.
type adapter struct {
	name string
	make func(t *testing.T, steps ...llmtest.Step) (llm.Provider, *llmtest.Server)
}

var adapters = []adapter{
	{"anthropic", func(t *testing.T, steps ...llmtest.Step) (llm.Provider, *llmtest.Server) {
		srv := llmtest.NewAnthropic(t, steps...)
		p, err := anthropic.New(anthropic.Config{Endpoint: srv.URL, Model: "m", APIKey: "k"})
		if err != nil {
			t.Fatal(err)
		}
		return p, srv
	}},
	{"openai-compatible", func(t *testing.T, steps ...llmtest.Step) (llm.Provider, *llmtest.Server) {
		srv := llmtest.NewOpenAI(t, steps...)
		p, err := openai.New(openai.Config{Endpoint: srv.URL + "/v1", Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		return p, srv
	}},
}

func echoTool(name string) mcptest.Tool {
	return mcptest.Tool{Name: name, Description: name, Handler: func(args json.RawMessage) (string, bool) {
		return "result:" + string(args), false
	}}
}

// toolset connects to a fake MCP server and builds the effective tool set.
func toolset(t *testing.T, srv *mcptest.Server, server, agentSide *[]string) *tools.Set {
	t.Helper()
	c, err := mcp.Connect(context.Background(), srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	set, err := tools.Build(context.Background(),
		[]config.MCPServer{{Name: "docs", URL: srv.URL, AllowedTools: server, AgentAllowedTools: agentSide}},
		map[string]tools.Caller{"docs": c})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func newAgent(t *testing.T, p llm.Provider, set *tools.Set, mut func(*agent.Config)) *agent.Agent {
	t.Helper()
	cfg := agent.Config{
		Provider: p, Tools: set, Instructions: "be useful", MaxOutput: 256, MaxToolCalls: 5,
		RequestTimeout: 5 * time.Second, ToolTimeout: 2 * time.Second,
	}
	if mut != nil {
		mut(&cfg)
	}
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func list(s ...string) *[]string { return &s }

// forEachAdapter runs the same behaviour test through every provider adapter.
func forEachAdapter(t *testing.T, fn func(t *testing.T, ad adapter)) {
	for _, ad := range adapters {
		t.Run(ad.name, func(t *testing.T) { fn(t, ad) })
	}
}

func TestInvokeCompletesThroughAuthorisedTool(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		mcpSrv := mcptest.New(t, false, echoTool("search"))
		p, model := ad.make(t,
			llmtest.Step{Calls: []llmtest.Call{{ID: "c1", Name: "docs__search", Args: `{"q":"x"}`}}},
			llmtest.Step{Text: "final answer"})
		a := newAgent(t, p, toolset(t, mcpSrv, nil, nil), nil)

		res, err := a.Invoke(context.Background(), "hello")
		if err != nil {
			t.Fatal(err)
		}
		if res.Output != "final answer" || res.Stop != llm.StopEnd {
			t.Fatalf("unexpected result %+v", res)
		}
		if res.ModelCalls != 2 || len(res.ToolCalls) != 1 || res.ToolCalls[0].Blocked {
			t.Fatalf("unexpected trace %+v", res)
		}
		if mcpSrv.Calls("search") != 1 {
			t.Fatalf("tool called %d times", mcpSrv.Calls("search"))
		}
		if res.Usage.InputTokens != 20 || res.Usage.OutputTokens != 10 {
			t.Fatalf("usage not accumulated: %+v", res.Usage)
		}
		// The tool output must have been sent back to the model.
		bodies := model.Bodies()
		if len(bodies) != 2 || !strings.Contains(string(bodies[1]), `result:`) {
			t.Fatalf("second model request did not carry the tool result: %s", bodies[len(bodies)-1])
		}
	})
}

func TestForbiddenToolIsBlockedAndNeverReachesServer(t *testing.T) {
	cases := []struct {
		name          string
		server, agent *[]string
	}{
		{"not in server list", list("search"), nil},
		{"not in agent list", nil, list("search")},
		{"empty server list", list(), nil},
		{"empty agent list", nil, list()},
	}
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				mcpSrv := mcptest.New(t, false, echoTool("search"), echoTool("delete"))
				// The model asks for delete, which is never advertised and must be refused.
				p, model := ad.make(t,
					llmtest.Step{Calls: []llmtest.Call{{ID: "c1", Name: "docs__delete", Args: `{}`}}},
					llmtest.Step{Text: "ok, I could not"})
				a := newAgent(t, p, toolset(t, mcpSrv, tc.server, tc.agent), nil)
				res, err := a.Invoke(context.Background(), "delete things")
				if err != nil {
					t.Fatal(err)
				}
				if mcpSrv.Calls("delete") != 0 {
					t.Fatal("forbidden tool reached the MCP server")
				}
				if len(res.ToolCalls) != 1 || !res.ToolCalls[0].Blocked {
					t.Fatalf("call not recorded as blocked: %+v", res.ToolCalls)
				}
				if !strings.Contains(string(model.Bodies()[1]), "not permitted") {
					t.Fatal("model was not told the call was refused")
				}
			})
		}
	})
}

func TestToolsAreAdvertisedOnlyWhenPermitted(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		mcpSrv := mcptest.New(t, false, echoTool("search"), echoTool("delete"))
		p, model := ad.make(t, llmtest.Step{Text: "hi"})
		a := newAgent(t, p, toolset(t, mcpSrv, list("search", "delete"), list("search")), nil)
		if _, err := a.Invoke(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
		body := string(model.Bodies()[0])
		if !strings.Contains(body, "docs__search") || strings.Contains(body, "docs__delete") {
			t.Fatalf("advertised tools wrong: %s", body)
		}
	})
}

func TestToolErrorsAreReturnedToTheModel(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		mcpSrv := mcptest.New(t, true, mcptest.Tool{Name: "search", Handler: func(json.RawMessage) (string, bool) {
			return "boom", true
		}})
		p, model := ad.make(t,
			llmtest.Step{Calls: []llmtest.Call{{ID: "c1", Name: "docs__search"}}},
			llmtest.Step{Text: "recovered"})
		a := newAgent(t, p, toolset(t, mcpSrv, nil, nil), nil)
		res, err := a.Invoke(context.Background(), "x")
		if err != nil || res.Output != "recovered" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if !strings.Contains(string(model.Bodies()[1]), "boom") {
			t.Fatal("tool error text did not reach the model")
		}
	})
}

func TestToolCallLimit(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		mcpSrv := mcptest.New(t, false, echoTool("search"))
		call := llmtest.Step{Calls: []llmtest.Call{{ID: "c", Name: "docs__search", Args: `{}`}}}
		p, _ := ad.make(t, call, call, call, call)
		a := newAgent(t, p, toolset(t, mcpSrv, nil, nil), func(c *agent.Config) { c.MaxToolCalls = 2 })
		_, err := a.Invoke(context.Background(), "loop")
		if errs.KindOf(err) != errs.LimitExceeded {
			t.Fatalf("want limit_exceeded, got %v", err)
		}
		if mcpSrv.Calls("search") != 2 {
			t.Fatalf("tool ran %d times, want exactly the limit", mcpSrv.Calls("search"))
		}
	})
}

func TestRequestTimeout(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		p, _ := ad.make(t, llmtest.Step{Hang: true})
		a := newAgent(t, p, nil, func(c *agent.Config) { c.RequestTimeout = 150 * time.Millisecond })
		start := time.Now()
		_, err := a.Invoke(context.Background(), "x")
		if errs.KindOf(err) != errs.Timeout {
			t.Fatalf("want timeout, got %v", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("timeout was not prompt")
		}
	})
}

func TestCancellation(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		p, _ := ad.make(t, llmtest.Step{Hang: true})
		a := newAgent(t, p, nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		_, err := a.Invoke(ctx, "x")
		if errs.KindOf(err) != errs.Canceled {
			t.Fatalf("want canceled, got %v", err)
		}
	})
}

func TestCancellationDuringToolCall(t *testing.T) {
	mcpSrv := mcptest.New(t, false, mcptest.Tool{Name: "slow", Delay: 5 * time.Second, Handler: func(json.RawMessage) (string, bool) { return "late", false }})
	p, _ := adapters[0].make(t, llmtest.Step{Calls: []llmtest.Call{{ID: "c", Name: "docs__slow"}}})
	a := newAgent(t, p, toolset(t, mcpSrv, nil, nil), nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := a.Invoke(ctx, "x")
	if errs.KindOf(err) != errs.Canceled {
		t.Fatalf("want canceled, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancellation did not interrupt the tool call")
	}
}

func TestToolTimeoutIsReportedToModelNotFatal(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		mcpSrv := mcptest.New(t, false, mcptest.Tool{Name: "slow", Delay: 5 * time.Second, Handler: func(json.RawMessage) (string, bool) { return "late", false }})
		p, model := ad.make(t,
			llmtest.Step{Calls: []llmtest.Call{{ID: "c", Name: "docs__slow"}}},
			llmtest.Step{Text: "gave up on the tool"})
		a := newAgent(t, p, toolset(t, mcpSrv, nil, nil), func(c *agent.Config) { c.ToolTimeout = 150 * time.Millisecond })
		res, err := a.Invoke(context.Background(), "x")
		if err != nil || res.Output != "gave up on the tool" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if !strings.Contains(string(model.Bodies()[1]), "timed out") {
			t.Fatal("model was not told the tool timed out")
		}
	})
}

func TestProviderErrorsMapToKinds(t *testing.T) {
	cases := map[int]errs.Kind{
		http.StatusUnauthorized:        errs.Auth,
		http.StatusTooManyRequests:     errs.RateLimited,
		http.StatusBadRequest:          errs.BadRequest,
		http.StatusInternalServerError: errs.Unavailable,
	}
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		for status, want := range cases {
			p, _ := ad.make(t, llmtest.Step{Status: status})
			a := newAgent(t, p, nil, nil)
			_, err := a.Invoke(context.Background(), "x")
			if got := errs.KindOf(err); got != want {
				t.Errorf("status %d: want %s, got %s (%v)", status, want, got, err)
			}
		}
	})
}

func TestMaxTokensIsReportedNotHidden(t *testing.T) {
	forEachAdapter(t, func(t *testing.T, ad adapter) {
		p, _ := ad.make(t, llmtest.Step{Text: "cut of", MaxTokens: true})
		a := newAgent(t, p, nil, nil)
		res, err := a.Invoke(context.Background(), "x")
		if err != nil || res.Stop != llm.StopMaxTokens {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
}

func TestEmptyInputRejected(t *testing.T) {
	a := newAgent(t, fake.New(llm.Capabilities{}), nil, nil)
	if _, err := a.Invoke(context.Background(), "  "); errs.KindOf(err) != errs.BadRequest {
		t.Fatalf("got %v", err)
	}
}

func TestUnsupportedCapabilityRejectedAtConstruction(t *testing.T) {
	mcpSrv := mcptest.New(t, false, echoTool("search"))
	set := toolset(t, mcpSrv, nil, nil)
	noTools := fake.New(llm.Capabilities{ToolCalling: false})
	_, err := agent.New(agent.Config{
		Provider: noTools, Tools: set, Instructions: "x", MaxOutput: 1, MaxToolCalls: 1,
		RequestTimeout: time.Second, ToolTimeout: time.Second,
	})
	if errs.KindOf(err) != errs.Unsupported {
		t.Fatalf("want unsupported, got %v", err)
	}
	// With no tools configured the same provider is acceptable.
	if _, err := agent.New(agent.Config{
		Provider: noTools, Instructions: "x", MaxOutput: 1, MaxToolCalls: 1,
		RequestTimeout: time.Second, ToolTimeout: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLoopWithFakeModel(t *testing.T) {
	mcpSrv := mcptest.New(t, false, echoTool("search"))
	p := fake.New(llm.Capabilities{ToolCalling: true},
		llm.Response{Stop: llm.StopToolUse, Message: llm.Message{Role: llm.Assistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "docs__search", Args: json.RawMessage(`{"q":1}`)}}}},
		llm.Response{Stop: llm.StopEnd, Message: llm.Message{Role: llm.Assistant, Text: "done"}})
	a := newAgent(t, p, toolset(t, mcpSrv, nil, nil), nil)
	res, err := a.Invoke(context.Background(), "go")
	if err != nil || res.Output != "done" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	last := p.Requests[1].Messages
	tr := last[len(last)-1].ToolResults
	if len(tr) != 1 || tr[0].CallID != "1" || !strings.Contains(tr[0].Content, `{"q":1}`) {
		t.Fatalf("tool result not threaded back: %+v", tr)
	}
}
