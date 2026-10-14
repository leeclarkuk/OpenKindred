#!/usr/bin/env bash
# Build the tools hack/test-composition.py needs, without docker, into .tools/.
# Takes a few minutes the first time. Needs git and a Go toolchain (Go downloads the newer
# toolchain these projects ask for).
set -euo pipefail
cd "$(dirname "$0")/.."

CROSSPLANE_VERSION="${CROSSPLANE_VERSION:-v2.4.2}"   # the engine; matches the chart pinned on the hub
CLI_VERSION="${CLI_VERSION:-v2.5.0}"                 # the render command
FUNCTION_VERSION="${FUNCTION_VERSION:-v0.13.0}"      # function-go-templating

mkdir -p .tools/src .tools/bin
clone() { # url tag dir
  [ -d ".tools/src/$3" ] || git clone -q --depth 1 --branch "$2" "$1" ".tools/src/$3"
}

clone https://github.com/crossplane/crossplane.git "$CROSSPLANE_VERSION" crossplane
clone https://github.com/crossplane/cli.git "$CLI_VERSION" cli
clone https://github.com/crossplane-contrib/function-go-templating.git "$FUNCTION_VERSION" function-go-templating

(cd .tools/src/crossplane && go build -o ../../bin/crossplane ./cmd/crossplane)
(cd .tools/src/cli && go build -o ../../bin/crossplane-cli ./cmd/crossplane)
(cd .tools/src/function-go-templating && go build -o ../../bin/function-go-templating .)

cat <<MSG
Built into .tools/bin. Run the composition tests with:

  CROSSPLANE_CLI=\$PWD/.tools/bin/crossplane-cli \\
  CROSSPLANE_BIN=\$PWD/.tools/bin/crossplane \\
  FUNCTION_BIN=\$PWD/.tools/bin/function-go-templating \\
  python3 hack/test-composition.py
MSG
