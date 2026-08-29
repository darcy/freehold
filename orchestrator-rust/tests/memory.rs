//! Phase D3: encrypted memory over the relay (kind 30174, native engram).
//! The relay stores SEALED envelopes only — a relay operator (or the runner)
//! sees ciphertext; only the agent's own enc key opens a value.

use freehold_core::{identity::Identity, memory, relay_http};

#[tokio::test(flavor = "multi_thread")]
async fn memory_writes_encrypted_engrams_and_reads_them_back() {
    let agent = Identity::generate();
    let (relay_url, state, server) = freehold_testkit::relay::spawn().await;

    relay_http::write_memory(
        &relay_url,
        &agent.secret_seed(),
        "last-run",
        "phase-d3-complete",
    )
    .unwrap();

    // The relay holds one kind-30174 event by the agent whose CONTENT never
    // contains the plaintext.
    let events = state.events.lock().clone();
    assert_eq!(events.len(), 1);
    assert_eq!(events[0]["kind"].as_u64(), Some(30174));
    assert_eq!(
        events[0]["pubkey"].as_str().unwrap(),
        agent.nostr_pubkey_hex()
    );
    assert!(
        !events[0]["content"].as_str().unwrap().contains("phase-d3"),
        "relay must only ever see the sealed envelope: {}",
        events[0]["content"]
    );

    // The agent reads its own memory back.
    let got = relay_http::read_memory(&relay_url, &agent.secret_seed(), "last-run").unwrap();
    assert_eq!(got.as_deref(), Some("phase-d3-complete"));

    // A DIFFERENT agent's read is scoped to ITS OWN d-tag: our event is
    // invisible to it (None, not garbage) — stronger than a decrypt error.
    let other = Identity::generate();
    let opened = relay_http::read_memory(&relay_url, &other.secret_seed(), "last-run").unwrap();
    assert_eq!(opened, None, "other agents see no trace of this memory");

    // Tamper the stored envelope: the OWNER's read must fail closed (a
    // corrupted blob is an error, never a partial/garbage value).
    {
        let mut events = state.events.lock();
        let ev = events.last_mut().unwrap();
        let sealed = ev["content"].as_str().unwrap().to_string();
        let mut bytes = sealed.as_bytes().to_vec();
        let last = bytes.len() - 1;
        bytes[last] = if bytes[last] == b'A' { b'B' } else { b'A' };
        ev["content"] = serde_json::Value::String(String::from_utf8(bytes).unwrap());
    }
    let opened = relay_http::read_memory(&relay_url, &agent.secret_seed(), "last-run").unwrap();
    // Tampering the stored event breaks its SIGNATURE, so the reader rejects
    // it outright (None, never a partially-garbage value). The decrypt-level
    // fail-closed (a forged blob with a valid signature) is covered in core's
    // memory tests.
    assert_eq!(opened, None, "tampered event is rejected at the signature");

    // Replacing the value supersedes (same d-tag; newest wins).
    relay_http::write_memory(
        &relay_url,
        &agent.secret_seed(),
        "last-run",
        "phase-d3-done",
    )
    .unwrap();
    let got = relay_http::read_memory(&relay_url, &agent.secret_seed(), "last-run").unwrap();
    assert_eq!(got.as_deref(), Some("phase-d3-done"));

    // An absent key reads as None.
    let none = relay_http::read_memory(&relay_url, &agent.secret_seed(), "never-written").unwrap();
    assert_eq!(none, None);
    let _ = memory::MEMORY_KIND;

    server.abort();
}
