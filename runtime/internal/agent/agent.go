// Package agent runs the execution loop: model, tools, model, until a final answer.
// It depends only on the llm.Provider contract and a tool set.
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
	"github.com/leeclarkuk/openkindred/runtime/internal/tools"
)

const maxToolResultBytes = 32 << 10

type Config struct {
	Provider       llm.Provider
	Tools          *tools.Set
	Instructions   string
	MaxOutput      int
	MaxToolCalls   int
	RequestTimeout time.Duration
	ToolTimeout    time.Duration
}

type Agent struct{ cfg Config }

// New checks that the provider can meet the agent's requirements.
func New(cfg Config) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, errs.New(errs.InvalidConfig, "no model provider")
	}
	if cfg.Tools != nil && len(cfg.Tools.Specs()) > 0 && !cfg.Provider.Capabilities().ToolCalling {
		return nil, errs.New(errs.Unsupported, "agent has tools but provider %q does not support tool calling", cfg.Provider.Name())
	}
	if cfg.MaxOutput <= 0 || cfg.MaxToolCalls < 0 || cfg.RequestTimeout <= 0 || cfg.ToolTimeout <= 0 {
		return nil, errs.New(errs.InvalidConfig, "limits must be positive")
	}
	return &Agent{cfg: cfg}, nil
}

type ToolCallRecord struct {
	Name    string `json:"name"`
	Blocked bool   `json:"blocked,omitempty"`
	Error   string `json:"error,omitempty"`
}

type Result struct {
	Output     string           `json:"output"`
	Stop       llm.StopReason   `json:"stopReason"`
	Usage      llm.Usage        `json:"usage"`
	ModelCalls int              `json:"modelCalls"`
	ToolCalls  []ToolCallRecord `json:"toolCalls,omitempty"`
}

// Invoke runs one request to completion. ctx cancellation and the request timeout
// stop it at the next model or tool call.
func (a *Agent) Invoke(ctx context.Context, input string) (Result, error) {
	if strings.TrimSpace(input) == "" {
		return Result{}, errs.New(errs.BadRequest, "input is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.RequestTimeout)
	defer cancel()

	var specs []llm.ToolSpec
	if a.cfg.Tools != nil {
		specs = a.cfg.Tools.Specs()
	}
	msgs := []llm.Message{{Role: llm.User, Text: input}}
	var res Result
	for {
		if ce := errs.FromContext(ctx); ce != nil {
			return res, ce
		}
		resp, err := a.cfg.Provider.Complete(ctx, llm.Request{
			System: a.cfg.Instructions, Messages: msgs, Tools: specs, MaxTokens: a.cfg.MaxOutput,
		})
		if err != nil {
			if ce := errs.FromContext(ctx); ce != nil {
				return res, ce
			}
			return res, err
		}
		res.ModelCalls++
		res.Usage.InputTokens += resp.Usage.InputTokens
		res.Usage.OutputTokens += resp.Usage.OutputTokens
		msgs = append(msgs, resp.Message)

		if len(resp.Message.ToolCalls) == 0 {
			res.Output = resp.Message.Text
			res.Stop = resp.Stop
			if res.Stop == llm.StopToolUse {
				return res, errs.New(errs.Unavailable, "model signalled tool use without a tool call")
			}
			return res, nil
		}

		results := make([]llm.ToolResult, 0, len(resp.Message.ToolCalls))
		for _, call := range resp.Message.ToolCalls {
			if len(res.ToolCalls) >= a.cfg.MaxToolCalls {
				return res, errs.New(errs.LimitExceeded, "tool call limit of %d reached", a.cfg.MaxToolCalls)
			}
			tr, rec, err := a.runTool(ctx, call)
			if err != nil {
				return res, err
			}
			res.ToolCalls = append(res.ToolCalls, rec)
			results = append(results, tr)
		}
		msgs = append(msgs, llm.Message{Role: llm.User, ToolResults: results})
	}
}

// runTool never returns an error for a tool that merely failed or was refused: the
// model is told, and may recover. It returns an error only when the whole request
// must stop, which is when ctx has ended.
func (a *Agent) runTool(ctx context.Context, call llm.ToolCall) (llm.ToolResult, ToolCallRecord, error) {
	rec := ToolCallRecord{Name: call.Name}
	fail := func(msg string) (llm.ToolResult, ToolCallRecord, error) {
		rec.Error = msg
		return llm.ToolResult{CallID: call.ID, Content: msg, IsError: true}, rec, nil
	}
	if a.cfg.Tools == nil {
		rec.Blocked = true
		return fail(fmt.Sprintf("tool %q is not permitted", call.Name))
	}
	toolCtx, cancel := context.WithTimeout(ctx, a.cfg.ToolTimeout)
	defer cancel()
	out, err := a.cfg.Tools.Call(toolCtx, call.Name, call.Args)
	if err != nil {
		if ce := errs.FromContext(ctx); ce != nil {
			return llm.ToolResult{}, rec, ce
		}
		switch errs.KindOf(err) {
		case errs.Forbidden:
			rec.Blocked = true
			return fail(fmt.Sprintf("tool %q is not permitted", call.Name))
		case errs.Timeout:
			return fail(fmt.Sprintf("tool %q timed out", call.Name))
		default:
			return fail(fmt.Sprintf("tool %q failed", call.Name))
		}
	}
	text := out.Text
	if len(text) > maxToolResultBytes {
		text = text[:maxToolResultBytes] + "\n[truncated]"
	}
	if out.IsError {
		rec.Error = "tool reported an error"
	}
	return llm.ToolResult{CallID: call.ID, Content: text, IsError: out.IsError}, rec, nil
}
