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

# Install the freehold CLI onto PATH (~/.cargo/bin/freehold) — for the TUI and
# the CLI verbs. NOTE: build/teardown must run `./target/debug/freehold` (the
# colocated binary) so resolveRebuildBins finds the sibling binaries.
install: build
    cp target/debug/freehold ~/.cargo/bin/freehold

# Verify every sibling `freehold build`/`teardown` resolves is present.
check-siblings:
    @test -x target/debug/freehold && echo "ok  target/debug/freehold" || (echo "MISSING target/debug/freehold — run `just build`"; exit 1)
    @test -x target/debug/freehold-console && echo "ok  target/debug/freehold-console" || (echo "MISSING target/debug/freehold-console — run `just build`"; exit 1)
    @test -x target/release/freehold-console && echo "ok  target/release/freehold-console" || (echo "MISSING target/release/freehold-console — run `just build`"; exit 1)
    @test -x target/debug/runner && echo "ok  target/debug/runner" || (echo "MISSING target/debug/runner — run `just build`"; exit 1)
    @test -x target/release/runner && echo "ok  target/release/runner" || (echo "MISSING target/release/runner — run `just build`"; exit 1)
    @test -x target/release/freehold-agent-tools && echo "ok  target/release/freehold-agent-tools" || (echo "MISSING target/release/freehold-agent-tools — run `just build`"; exit 1)
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

# Tear the world down (compute-only: keeps coords + /srv/data). Runs the
# COLOCATED binary — resolveRebuildBins finds the sibling binaries relative to
# the running executable (target/debug/ + target/release/), so build/teardown
# must use ./target/debug/freehold, NOT a copy installed elsewhere.
teardown: build check-siblings
    ./target/debug/freehold teardown

# Bring the world up (CP-bring-up + trigger world_build).
build-world: build check-siblings
    ./target/debug/freehold build
