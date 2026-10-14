// Command configcheck validates a runtime configuration file with the same parser the
// runtime uses, and prints its content hash. It is for tests and CI, for example to check
// what a composition renders.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/leeclarkuk/openkindred/runtime/internal/config"
	"github.com/leeclarkuk/openkindred/runtime/internal/errs"
)

func main() {
	var data []byte
	var err error
	if len(os.Args) > 1 && os.Args[1] != "-" {
		data, err = os.ReadFile(os.Args[1])
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_, hash, err := config.Parse(data)
	if err != nil {
		msg := err.Error()
		if e, ok := err.(*errs.Error); ok {
			msg = e.Detail()
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
	fmt.Println(hash)
}
