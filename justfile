# freehold — the AI-operated appliance
#
# The build recipes a UAT / rebuild / teardown box needs. `just build` compiles
# every binary the pipeline resolves (freehold + 4 siblings), `just install`
# places the CLI on PATH. Go runs through mise (the modules pin go 1.25.0).

set shell := ["bash", "-c"]

# The version identity, from git. CI overrides VERSION with the tag it is
# building; local builds get the describe string. Both stamp identically.
version := `echo "${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"`
commit := `echo "${COMMIT:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)}"`
ldflags := "-X freehold/contract/version.Version=" + version + " -X freehold/contract/version.Commit=" + commit

# Build every binary a rebuild/teardown/install box needs (freehold + 4 siblings;
# the `install` surface is folded into the freehold CLI).
build:
    @echo "→ freehold (CLI + TUI + install)   [{{version}}]"
    @cd freehold-cli && mise exec go@1.25.0 -- go build -ldflags "{{ldflags}}" -o ../target/debug/freehold ./cmd/freehold
    @echo "→ freehold-console (debug + release — the Go CP CLI the box stages call, and what deploy-cp ships)"
    @cd control-plane && mise exec go@1.25.0 -- go build -ldflags "{{ldflags}}" -o ../target/debug/freehold-console ./api/cmd/freehold-console
    @cd control-plane && mise exec go@1.25.0 -- go build -ldflags "{{ldflags}}" -o ../target/release/freehold-console ./api/cmd/freehold-console
    @echo "→ runner (Rust — debug + release)"
    @FREEHOLD_VERSION="{{version}}" FREEHOLD_COMMIT="{{commit}}" mise exec rust@1.98.0 -- cargo build --bin runner
    @FREEHOLD_VERSION="{{version}}" FREEHOLD_COMMIT="{{commit}}" mise exec rust@1.98.0 -- cargo build --release --bin runner
    @echo "→ freehold-agent-tools (static release — the CP ships it to agent pods)"
    @cd control-plane && mise exec go@1.25.0 -- sh -c 'CGO_ENABLED=0 go build -ldflags "{{ldflags}}" -o ../target/release/freehold-agent-tools ./api/cmd/freehold-agent-tools'
    @echo "✓ all binaries built"

# Install the freehold CLI + ALL its siblings onto PATH (~/.cargo/bin) so the
# installed binaries resolve the sibling binaries (ResolveBins looks for them
# relative to the running executable). The operator runs `freehold build` for
# world bring-up; `freehold install` for CP creation (box one).
install: build
    mkdir -p ~/.cargo/bin ~/.cargo/release
    # install (temp+rename, not cp) so a RUNNING sibling is replaced atomically
    # instead of "Text file busy".
    install -m 755 target/debug/freehold ~/.cargo/bin/freehold
    install -m 755 target/debug/freehold-console ~/.cargo/bin/freehold-console
    install -m 755 target/debug/runner ~/.cargo/bin/runner
    install -m 755 target/release/freehold-console ~/.cargo/release/freehold-console
    install -m 755 target/release/runner ~/.cargo/release/runner
    install -m 755 target/release/freehold-agent-tools ~/.cargo/release/freehold-agent-tools
    @echo "✓ freehold + siblings installed (~/.cargo/bin + ~/.cargo/release)"

# Verify every sibling `box.ResolveBins` requires is present (AGENTS.md's full
# binary set). One list, so the gate can't silently lag the ResolveBins set.
check-siblings:
    @for f in target/debug/freehold target/debug/freehold-console target/release/freehold-console target/debug/runner target/release/runner target/release/freehold-agent-tools; do \
      if test -x "$f"; then echo "ok  $f"; else echo "MISSING $f (run: just build)"; exit 1; fi; \
    done
    @echo "✓ all siblings present"

# Run the full gate: cargo fmt/build/test (runner + core) + Go build/vet/test
# across the six modules (incl. the hermetic Chunk-1 acceptance gate, which
# is `go test ./acceptance/…` in the control-plane module) + the harness
# byte-gate.
test:
    @echo "→ cargo fmt / build / test"
    @mise exec rust@1.98.0 -- cargo fmt --all --check
    @mise exec rust@1.98.0 -- cargo build --workspace
    @mise exec rust@1.98.0 -- cargo test --workspace
    @echo "→ Go build / vet / test (agents, contract, platform, providers, freehold-cli, control-plane)"
    @cd agents && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @cd contract && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @cd platform && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @cd providers && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @cd freehold-cli && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @cd control-plane && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @echo "✓ all gates green"
