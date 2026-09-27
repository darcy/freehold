// Release-test helper (UNTRACKED, never committed).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"freehold/contract/config"
	"freehold/contract/relay"
	"freehold/contract/wire"
	"freehold/platform/provisioning/box"
)

func opSec(profile string) []byte {
	config.SetCurrent(config.Resolve(profile))
	id, err := box.LoadIdentity(filepath.Join(config.StateDir(), "control-plane", "operator"))
	if err != nil {
		panic(err)
	}
	sec, err := hexDec(id.NostrSecretHex)
	if err != nil {
		panic(err)
	}
	return sec
}

func main() {
	switch os.Args[1] {
	case "mint":
		dir := os.Args[2]
		if err := box.MintIdentity(dir); err != nil {
			panic(err)
		}
		pk, err := box.LoadPubkey(dir)
		if err != nil {
			panic(err)
		}
		fmt.Println(pk)
	case "ping":
		sec := opSec(os.Args[2])
		relayURL, agentPK, msg := os.Args[3], os.Args[4], os.Args[5]
		chID, _, ok, err := relay.FindChannelAuth(relayURL, relayURL, sec, "freehold")
		if err != nil || !ok {
			panic(fmt.Sprintf("find #freehold: err=%v ok=%v", err, ok))
		}
		tags := [][]string{{"h", chID}, {"p", agentPK}}
		ts := time.Now().Unix()
		pk, evID, sig, err := wire.SignEvent(sec, 9, ts, tags, msg)
		if err != nil {
			panic(err)
		}
		ev := map[string]interface{}{"id": evID, "pubkey": pk, "created_at": ts, "kind": 9, "tags": tags, "content": msg, "sig": sig}
		b, _ := json.Marshal(ev)
		if err := relay.PublishEventJSONAuth(relayURL, relayURL, sec, string(b)); err != nil {
			panic(err)
		}
		fmt.Println("pinged as", pk[:8])
	case "read":
		sec := opSec(os.Args[2])
		relayURL := os.Args[3]
		since, _ := strconv.ParseInt(os.Args[4], 10, 64)
		chID, _, ok, err := relay.FindChannelAuth(relayURL, relayURL, sec, "freehold")
		if err != nil || !ok {
			panic(fmt.Sprintf("find #freehold: err=%v ok=%v", err, ok))
		}
		evs, err := relay.QueryEventsAuth(relayURL, relayURL, sec, []map[string]interface{}{{
			"kinds": []int{9}, "#h": []string{chID}, "since": since, "limit": 30,
		}})
		if err != nil {
			panic(err)
		}
		for _, e := range evs {
			fmt.Println(fmt.Sprint(e["created_at"]), fmt.Sprint(e["pubkey"])[:8], e["content"])
		}
	default:
		panic("want: mint <dir> | ping <profile> <relay> <agent-pk> <msg> | read <profile> <relay> <since>")
	}
}

func hexDec(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		var b byte
		fmt.Sscanf(s[i:i+2], "%02x", &b)
		out[i/2] = b
	}
	return out, nil
}
