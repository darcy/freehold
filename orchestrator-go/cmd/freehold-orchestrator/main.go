// Command freehold-orchestrator is the permanent bootstrap/repair engine for
// the freehold appliance. It is a pure HTTP client over the runner's MCP
// endpoint; the runner (Rust) remains the exec target.
package main

import (
	"os"

	"freehold/orchestrator-go/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
