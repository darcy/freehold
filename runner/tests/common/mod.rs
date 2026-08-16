//! Shared test helpers for MCP-level tests: a test AGENT identity and
//! per-request signing (Phase D grants). Every tools/call in tests is signed,
//! exactly like the real orchestrator will be.

use std::sync::LazyLock;

use freehold_core::auth;
use rand::RngCore;

fn random_secret() -> [u8; 32] {
    let mut b = [0u8; 32];
    rand::rng().fill_bytes(&mut b);
    b
}

/// The shared test agent: secret key + pubkey. Lazy so every test in the
/// process grants the SAME pubkey (packages must whitelist it).
pub static TEST_AGENT: LazyLock<([u8; 32], String)> = LazyLock::new(|| {
    let secret = random_secret();
    let pubkey = freehold_core::audit::sign_event(&secret, "test-agent")
        .unwrap()
        .pubkey;
    (secret, pubkey)
});

pub fn agent_pubkey() -> String {
    TEST_AGENT.1.clone()
}

/// Sign `raw_body` (the exact bytes the runner will receive) for the test
/// agent; returns (pubkey, sig, ts) header values.
pub fn signed_headers(raw_body: &str, runner_pubkey: &str) -> (String, String, String) {
    let ts = auth::now_secs();
    let ev = auth::sign_body(&TEST_AGENT.0, runner_pubkey, ts, raw_body);
    (TEST_AGENT.1.clone(), ev.sig, ts.to_string())
}
