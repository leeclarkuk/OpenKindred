// Package tools builds the set of tools an agent may use and enforces permissions.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
	"github.com/leeclarkuk/openkindred/runtime/internal/llm"
	"github.com/leeclarkuk/openkindred/runtime/internal/mcp"
)

// Caller is the part of an MCP client the toolset needs.
type Caller interface {
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (mcp.CallResult, error)
}

var exposedRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type entry struct {
	caller Caller
	remote string
	spec   llm.ToolSpec
}

// Set is the effective, immutable tool set for an agent.
type Set struct {
	byName map[string]entry
	order  []string
}

// Effective computes the tools permitted by both sides. A nil list means no
// restriction from that side. A non-nil empty list permits nothing.
func Effective(available []string, server, agent *[]string) []string {
	allowed := func(l *[]string, name string) bool {
		if l == nil {
			return true
		}
		for _, n := range *l {
			if n == name {
				return true
			}
		}
		return false
	}
	var out []string
	for _, n := range available {
		if allowed(server, n) && allowed(agent, n) {
			out = append(out, n)
		}
	}
	return out
}

// ExposedName is the name a model sees: server and tool joined, so servers cannot
// collide.
func ExposedName(server, tool string) string { return server + "__" + tool }

// Build lists each server's tools and keeps only those permitted. callers maps a
// server name to its client.
func Build(ctx context.Context, servers []config.MCPServer, callers map[string]Caller) (*Set, error) {
	s := &Set{byName: map[string]entry{}}
	for _, srv := range servers {
		c, ok := callers[srv.Name]
		if !ok {
			return nil, errs.New(errs.Internal, "no client for mcp server %q", srv.Name)
		}
		listed, err := c.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
		}
		byRemote := map[string]mcp.Tool{}
		names := make([]string, 0, len(listed))
		for _, t := range listed {
			byRemote[t.Name] = t
			names = append(names, t.Name)
		}
		for _, name := range Effective(names, srv.AllowedTools, srv.AgentAllowedTools) {
			t := byRemote[name]
			exposed := ExposedName(srv.Name, name)
			if !exposedRe.MatchString(exposed) {
				return nil, errs.New(errs.InvalidConfig, "tool name %q is not usable with model providers", exposed)
			}
			if _, dup := s.byName[exposed]; dup {
				return nil, errs.New(errs.InvalidConfig, "duplicate tool %q", exposed)
			}
			s.byName[exposed] = entry{caller: c, remote: name, spec: llm.ToolSpec{
				Name: exposed, Description: t.Description, InputSchema: t.InputSchema,
			}}
			s.order = append(s.order, exposed)
		}
	}
	sort.Strings(s.order)
	return s, nil
}

// Specs returns what to advertise to the model.
func (s *Set) Specs() []llm.ToolSpec {
	out := make([]llm.ToolSpec, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.byName[n].spec)
	}
	return out
}

func (s *Set) Names() []string { return append([]string(nil), s.order...) }

// Call runs a tool. A name outside the effective set is refused without contacting
// any server, whatever the model was told.
func (s *Set) Call(ctx context.Context, name string, args json.RawMessage) (mcp.CallResult, error) {
	e, ok := s.byName[name]
	if !ok {
		return mcp.CallResult{}, errs.New(errs.Forbidden, "tool %q is not permitted for this agent", name)
	}
	return e.caller.CallTool(ctx, e.remote, args)
}
