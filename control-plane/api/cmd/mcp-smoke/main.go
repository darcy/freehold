// mcp-smoke drives the MCP capability plane's tools against a world's
// agent-tools server, signed as the operator (the break-glass peer). A TEST
// DRIVER — never committed.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"freehold/contract/crypto"
)

func main() {
	url := os.Getenv("SMOKE_URL")
	pub := os.Getenv("SMOKE_PUBKEY")
	nsecHex := os.Getenv("SMOKE_NSEC")
	verb := os.Args[1]
	args := os.Args[2]

	rawSec, err := hex.DecodeString(nsecHex)
	if err != nil {
		panic(err)
	}

	if pub == "" {
		pub = pubkeyOf(rawSec)
	}
	// The audience is the SERVER's pubkey (the signature binds to the peer).
	audience := os.Getenv("SMOKE_AUDIENCE")
	if audience == "" {
		audience = pub
	}

	tool := map[string]string{"provision": "provision_mcp", "grant": "grant_capability", "revoke": "revoke_capability", "runner": "provision_runner", "unrunner": "revoke_runner"}[verb]
	if tool == "" {
		panic("verb: provision|grant|revoke")
	}

	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
	ts := time.Now().Unix()
	canonical := audience + "|" + strconv.FormatInt(ts, 10) + "|" + body
	digest := sha256.Sum256([]byte(canonical))
	sigBytes, err := crypto.SignBIP340(rawSec, digest[:])
	if err != nil {
		panic(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-freehold-pubkey", pub)
	req.Header.Set("x-freehold-sig", hex.EncodeToString(sigBytes))
	req.Header.Set("x-freehold-ts", strconv.FormatInt(ts, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Println("HTTP", resp.StatusCode)
	fmt.Println(string(out))
	_ = bytes.MinRead
}

// pubkeyOf derives the 64-hex nostr pubkey for a raw nsec.
func pubkeyOf(raw []byte) string {
	pk, err := crypto.PubkeyFromSecret(raw)
	if err != nil {
		panic(err)
	}
	return pk
}
