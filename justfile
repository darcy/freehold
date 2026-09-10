# freehold — the AI-operated appliance
#
# The build recipes a UAT / rebuild / teardown box needs. `just build` compiles
# every binary the pipeline resolves (freehold + 4 siblings), `just install`
# places the CLI on PATH. Go runs through mise (the modules pin go 1.25.0).

set shell := ["bash", "-c"]

# Build every binary a rebuild/teardown box needs (freehold + 4 siblings).
build:
    @echo "→ freehold (CLI + TUI)"
    @cd control-plane && mise exec go@1.25.0 -- go build -o ../target/debug/freehold ./cli/cmd/freehold
    @echo "→ freehold-console (debug + release — the Go CP CLI the box stages call, and what deploy-cp ships)"
    @cd control-plane && mise exec go@1.25.0 -- go build -o ../target/debug/freehold-console ./api/cmd/freehold-console
    @cd control-plane && mise exec go@1.25.0 -- go build -o ../target/release/freehold-console ./api/cmd/freehold-console
    @echo "→ runner (Rust — debug + release)"
    @mise exec rust@1.98.0 -- cargo build --bin runner
    @mise exec rust@1.98.0 -- cargo build --release --bin runner
    @echo "→ freehold-agent-tools (static release — the CP ships it to agent pods)"
    @cd control-plane && mise exec go@1.25.0 -- sh -c 'CGO_ENABLED=0 go build -o ../target/release/freehold-agent-tools ./api/cmd/freehold-agent-tools'
    @echo "✓ all binaries built"

# Install the freehold CLI + ALL its siblings onto PATH (~/.cargo/bin) so the
# installed `freehold build`/`teardown` resolve the sibling binaries
# (resolveRebuildBins looks for them relative to the running executable:
# ~/.cargo/bin/{freehold-console,runner} + ~/.cargo/release/{freehold-console,
# runner,freehold-agent-tools}). The operator then runs `freehold build` to
# bring the world up.
install: build
    mkdir -p ~/.cargo/bin ~/.cargo/release
    # install (temp+rename, not cp) so a RUNNING sibling (e.g. the
    # provisioning runner) is replaced atomically instead of "Text file busy".
    install -m 755 target/debug/freehold ~/.cargo/bin/freehold
    install -m 755 target/debug/freehold-console ~/.cargo/bin/freehold-console
    install -m 755 target/debug/runner ~/.cargo/bin/runner
    install -m 755 target/release/freehold-console ~/.cargo/release/freehold-console
    install -m 755 target/release/runner ~/.cargo/release/runner
    install -m 755 target/release/freehold-agent-tools ~/.cargo/release/freehold-agent-tools
    @echo "✓ freehold + siblings installed (~/.cargo/bin + ~/.cargo/release)"

# Verify every sibling `freehold build`/`teardown` resolves is present.
check-siblings:
    @test -x target/debug/freehold && echo "ok  target/debug/freehold" || (echo "MISSING target/debug/freehold (run: just build)"; exit 1)
    @test -x target/debug/freehold-console && echo "ok  target/debug/freehold-console" || (echo "MISSING target/debug/freehold-console (run: just build)"; exit 1)
    @test -x target/release/freehold-console && echo "ok  target/release/freehold-console" || (echo "MISSING target/release/freehold-console (run: just build)"; exit 1)
    @test -x target/debug/runner && echo "ok  target/debug/runner" || (echo "MISSING target/debug/runner (run: just build)"; exit 1)
    @test -x target/release/runner && echo "ok  target/release/runner" || (echo "MISSING target/release/runner (run: just build)"; exit 1)
    @test -x target/release/freehold-agent-tools && echo "ok  target/release/freehold-agent-tools" || (echo "MISSING target/release/freehold-agent-tools (run: just build)"; exit 1)
    @echo "✓ all siblings present"

# Run the full gate: cargo fmt/build/test + Go build/vet/test across the three
# modules + the harness byte-gate.
test:
    @echo "→ cargo fmt / build / test"
    @mise exec rust@1.98.0 -- cargo fmt --all --check
    @mise exec rust@1.98.0 -- cargo build --workspace
    @mise exec rust@1.98.0 -- cargo test --workspace
    @echo "→ Go build / vet / test (contract, platform, control-plane)"
    @cd contract && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @cd platform && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @cd control-plane && mise exec go@1.25.0 -- go build ./... && mise exec go@1.25.0 -- go vet ./... && mise exec go@1.25.0 -- go test ./...
    @echo "→ Chunk-1 acceptance (hermetic)"
    @mise exec rust@1.98.0 -- cargo run -p freehold-acceptance
    @echo "✓ all gates green"
