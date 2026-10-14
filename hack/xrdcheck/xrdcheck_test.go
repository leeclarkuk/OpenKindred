// Evaluates the admission rules in the Agent XRD with CEL, without a cluster, so that the
// rules that reject unsupported capabilities are tested rather than assumed.
package xrdcheck

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/google/cel-go/cel"
	"gopkg.in/yaml.v3"
)

type rule struct {
	Rule    string `yaml:"rule"`
	Message string `yaml:"message"`
}

func loadRules(t *testing.T) []rule {
	t.Helper()
	data, err := os.ReadFile("../../platform/crossplane/agent/xrd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var x struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Validations []rule `yaml:"x-kubernetes-validations"`
							} `yaml:"spec"`
						} `yaml:"properties"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &x); err != nil {
		t.Fatal(err)
	}
	r := x.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Validations
	if len(r) == 0 {
		t.Fatal("no validation rules found")
	}
	return r
}

// violations returns the messages of the rules that reject spec.
func violations(t *testing.T, rules []rule, specJSON string) []string {
	t.Helper()
	var spec map[string]any
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		t.Fatal(err)
	}
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rules {
		ast, iss := env.Compile(r.Rule)
		if iss.Err() != nil {
			t.Fatalf("rule %q does not compile: %v", r.Rule, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := prg.Eval(map[string]any{"self": spec})
		if err != nil {
			t.Fatalf("rule %q failed on %s: %v", r.Rule, specJSON, err)
		}
		if ok, _ := v.Value().(bool); !ok {
			out = append(out, r.Message)
		}
	}
	return out
}

func TestRules(t *testing.T) {
	rules := loadRules(t)
	base := `"modelRef":{"name":"m"},"instructions":"x"`
	cases := []struct {
		name   string
		spec   string
		reject string // substring of the expected message; empty means accepted
	}{
		{"minimal", `{` + base + `}`, ""},
		{"with tools", `{` + base + `,"tools":[{"mcpServerRef":{"name":"a"}},{"mcpServerRef":{"name":"b"},"allowedTools":[]}]}`, ""},
		{"memory disabled is fine", `{` + base + `,"memory":{"enabled":false,"scope":"session"}}`, ""},
		{"memory scope without enabling is fine", `{` + base + `,"memory":{"scope":"agent"}}`, ""},
		{"memory enabled", `{` + base + `,"memory":{"enabled":true}}`, "memory is not supported"},
		{"browser enabled", `{` + base + `,"browser":{"enabled":true}}`, "browser is not supported"},
		{"code interpreter enabled", `{` + base + `,"codeInterpreter":{"enabled":true}}`, "codeInterpreter is not supported"},
		{"peers", `{` + base + `,"peers":[{"name":"other"}]}`, "peers"},
		{"empty peers is fine", `{` + base + `,"peers":[]}`, ""},
		{"duplicate tool server", `{` + base + `,"tools":[{"mcpServerRef":{"name":"a"}},{"mcpServerRef":{"name":"a"}}]}`, "listed once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := violations(t, rules, tc.spec)
			if tc.reject == "" {
				if len(got) != 0 {
					t.Fatalf("should be accepted, rejected with %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("should be rejected (%s) but was accepted", tc.reject)
			}
			for _, m := range got {
				if contains(m, tc.reject) {
					return
				}
			}
			t.Fatalf("rejected with %v, want a message containing %q", got, tc.reject)
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
