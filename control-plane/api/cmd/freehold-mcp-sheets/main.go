// freehold-mcp-sheets is the MCP capability plane's Google Sheets service:
// a minimal MCP Streamable-HTTP server that speaks the Sheets API for ONE
// pinned spreadsheet, authenticated to the broker by a static bearer token
// and to Google by a service-account key. It exists because the community
// Sheets MCP servers have no auth and no pin — this one answers only its
// broker and only its sheet.
//
// Posture:
//   - Broker auth: every /mcp request must carry Authorization: Bearer
//     $FREEHOLD_BROKER_TOKEN (the token the CP sealed for this capability;
//     LiteLLM attaches it as a static header). Unset = the endpoint is open
//     — slotted capabilities always set it (provision_mcp requires
//     secret_ref; the stage renders the Secret from the CP's sealed store).
//   - Pin: every tool addresses $FREEHOLD_SPREADSHEET_ID — the service
//     cannot name another spreadsheet. The HARD bound is still the SA's
//     Google ACL (share only that sheet with the service account).
//   - Read-only: $FREEHOLD_READ_ONLY=1 advertises only the read tools AND
//     requests the spreadsheets.readonly OAuth scope — belt and suspenders
//     with a Viewer service account.
//   - The SA key arrives as $GOOGLE_SA_JSON (the raw service-account JSON)
//     or $GOOGLE_SA_JSON_B64 (base64). When absent the server runs and
//     answers MCP, but every Google call fails with a clear configuration
//     error — the plumbing is verifiable without a credential.
//
// stdlib only: the service-account flow is a JWT (RSA-SHA256) exchanged at
// the SA's own token_uri; the Sheets calls are plain REST.
package main

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	s, err := newServerFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("FREEHOLD_MCP_ADDR")
	if addr == "" {
		addr = ":8000"
	}
	log.Printf("freehold-mcp-sheets: sheet=%q read_only=%v sa_configured=%v addr=%s",
		s.sheet, s.readOnly, s.auth.key != nil, addr)
	if err := http.ListenAndServe(addr, s.routes()); err != nil {
		log.Fatal(err)
	}
}

// newServerFromEnv assembles the server from the environment (the same
// logic main runs; the tests construct servers this way too).
func newServerFromEnv() (*server, error) {
	saJSON := os.Getenv("GOOGLE_SA_JSON")
	if saJSON == "" {
		if b64 := os.Getenv("GOOGLE_SA_JSON_B64"); b64 != "" {
			raw, err := base64StdDecode(b64)
			if err != nil {
				return nil, fmt.Errorf("GOOGLE_SA_JSON_B64: %w", err)
			}
			saJSON = raw
		}
	}
	var key *saKey
	var rsaKey *rsa.PrivateKey
	if saJSON != "" {
		k := &saKey{}
		if err := json.Unmarshal([]byte(saJSON), k); err != nil {
			return nil, fmt.Errorf("GOOGLE_SA_JSON: %w", err)
		}
		if k.ClientEmail == "" || k.PrivateKey == "" || k.TokenURI == "" {
			return nil, fmt.Errorf("GOOGLE_SA_JSON: client_email, private_key and token_uri are all required")
		}
		rk, err := k.rsaKey()
		if err != nil {
			return nil, fmt.Errorf("GOOGLE_SA_JSON: %w", err)
		}
		key = k
		rsaKey = rk
	}
	readOnly := os.Getenv("FREEHOLD_READ_ONLY") == "1"
	scope := "https://www.googleapis.com/auth/spreadsheets"
	if readOnly {
		scope = "https://www.googleapis.com/auth/spreadsheets.readonly"
	}
	sheetsBase := os.Getenv("FREEHOLD_SHEETS_BASE")
	if sheetsBase == "" {
		sheetsBase = "https://sheets.googleapis.com"
	}
	return &server{
		sheet:    os.Getenv("FREEHOLD_SPREADSHEET_ID"),
		readOnly: readOnly,
		auth: &googleAuth{
			key: key, rsa: rsaKey, scope: scope,
			http: &http.Client{Timeout: 30 * time.Second},
		},
		http:       &http.Client{Timeout: 30 * time.Second},
		sheetsBase: sheetsBase,
	}, nil
}
