// Package fake is a scripted llm.Provider for tests of the execution loop.
package fake

import (
	"context"
	"sync"

	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
)

type Provider struct {
	mu       sync.Mutex
	caps     llm.Capabilities
	script   []llm.Response
	Requests []llm.Request
}

func New(caps llm.Capabilities, script ...llm.Response) *Provider {
	return &Provider{caps: caps, script: script}
}

func (p *Provider) Name() string                   { return "fake" }
func (p *Provider) Capabilities() llm.Capabilities { return p.caps }

func (p *Provider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ce := errs.FromContext(ctx); ce != nil {
		return llm.Response{}, ce
	}
	p.Requests = append(p.Requests, req)
	if len(p.script) == 0 {
		return llm.Response{}, errs.New(errs.Internal, "fake provider script exhausted")
	}
	r := p.script[0]
	p.script = p.script[1:]
	return r, nil
}
