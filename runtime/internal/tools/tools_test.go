package tools_test

import (
	"reflect"
	"testing"

	"github.com/leeclarkuk/openkindred/runtime/internal/tools"
)

func l(s ...string) *[]string { return &s }

func TestEffective(t *testing.T) {
	avail := []string{"a", "b", "c"}
	cases := []struct {
		name          string
		server, agent *[]string
		want          []string
	}{
		{"both omitted allow all", nil, nil, []string{"a", "b", "c"}},
		{"server restricts", l("a", "b"), nil, []string{"a", "b"}},
		{"agent restricts", nil, l("c"), []string{"c"}},
		{"intersection", l("a", "b"), l("b", "c"), []string{"b"}},
		{"server empty denies all", l(), nil, nil},
		{"agent empty denies all", nil, l(), nil},
		{"names not offered are ignored", l("z"), nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tools.Effective(avail, tc.server, tc.agent); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
