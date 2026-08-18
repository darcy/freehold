//! Phase E: the CPA -> peer delegation flow over the relay (kind 9 channel
//! messages in a CPA-created OPEN channel; request/result correlated by id).
//! The peer's runner-direct exec leg is exercised by the LIVE proof; this
//! hermetic test proves the relay wire + envelope correlation end to end.

use freehold_core::{delegate, identity::Identity};

const CHANNEL: &str = "00000000-0000-4000-8000-00000000f0ee";

#[tokio::test(flavor = "multi_thread")]
async fn cpa_delegates_to_peer_and_gets_the_result_back() {
    let cpa = Identity::generate();
    let peer = Identity::generate();
    let peer_pk = peer.nostr_pubkey_hex();
    let (relay_url, state, server) = freehold_testkit::relay::spawn().await;

    // CPA creates the OPEN auto-ops channel + posts the request @peer.
    delegate::ensure_channel(&relay_url, &cpa.secret_seed(), CHANNEL, "freehold-auto-ops").unwrap();
    let since = freehold_core::auth::now_secs();
    let id = "job-0001".to_string();
    delegate::post_message(
        &relay_url,
        &cpa.secret_seed(),
        CHANNEL,
        &peer_pk,
        &delegate::request_content(&id, "hostname"),
    )
    .unwrap();

    // The PEER sees the request (p-tagged for itself) and correlates it.
    let msgs =
        delegate::poll_stream_p(&relay_url, &peer.secret_seed(), CHANNEL, &peer_pk, since).unwrap();
    assert_eq!(msgs.len(), 1, "peer sees exactly the request");
    let (_ts, content, author) = &msgs[0];
    assert_eq!(
        author,
        &cpa.nostr_pubkey_hex(),
        "request authored by the CPA"
    );
    let env = delegate::parse_envelope(content).unwrap();
    assert_eq!(env.ty, delegate::JOB_REQUEST);
    assert_eq!(env.id, id);
    assert_eq!(env.task.as_deref(), Some("hostname"));

    // The PEER posts the result @CPA (all members see the message; the
    // requester filters by its own request id).
    let result_since = freehold_core::auth::now_secs();
    delegate::post_message(
        &relay_url,
        &peer.secret_seed(),
        CHANNEL,
        &cpa.nostr_pubkey_hex(),
        &delegate::result_content(&id, true, "librem\n"),
    )
    .unwrap();

    // The CPA polls for its OWN p-tag (the result is addressed to it; the
    // request's p-tag is the peer's).
    let cpa_pk = cpa.nostr_pubkey_hex();
    let replies = delegate::poll_stream_p(
        &relay_url,
        &cpa.secret_seed(),
        CHANNEL,
        &cpa_pk,
        result_since,
    )
    .unwrap();
    assert_eq!(replies.len(), 1, "the result only: {replies:?}");
    assert_eq!(replies[0].2, peer_pk, "result authored by the peer");
    let env = delegate::parse_envelope(&replies[0].1).unwrap();
    assert_eq!(env.ty, delegate::JOB_RESULT);
    assert_eq!(env.id, id);
    assert_eq!(env.ok, Some(true));
    assert_eq!(env.out.as_deref(), Some("librem\n"));

    // A SIGNAture-tampered message is skipped by the reader (never trusted).
    let mut events = state.events.lock();
    let ev = events.last_mut().unwrap();
    let content = ev["content"].as_str().unwrap().to_string();
    ev["content"] = serde_json::Value::String(format!("{content} "));
    drop(events);
    let replies = delegate::poll_stream_p(
        &relay_url,
        &cpa.secret_seed(),
        CHANNEL,
        &cpa_pk,
        result_since,
    )
    .unwrap();
    assert_eq!(
        replies.len(),
        0,
        "tampered message rejected at the signature"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn agents_join_the_freehold_channel_and_the_greeting_is_signed() {
    let cpa = Identity::generate();
    let peer = Identity::generate();
    let (relay_url, state, server) = freehold_testkit::relay::spawn().await;
    let channel = "00000000-0000-4000-8000-00000000f0ef";

    // The bootstrap setup step: ensure the OPEN #freehold channel, join each
    // agent, greet as the CPA.
    delegate::ensure_channel(&relay_url, &cpa.secret_seed(), channel, "freehold").unwrap();
    freehold_core::relay_http::join_channel(&relay_url, &cpa.secret_seed(), channel).unwrap();
    freehold_core::relay_http::join_channel(&relay_url, &peer.secret_seed(), channel).unwrap();
    delegate::post_message(
        &relay_url,
        &cpa.secret_seed(),
        channel,
        &cpa.nostr_pubkey_hex(),
        "freehold agents online",
    )
    .unwrap();

    let events = state.events.lock().clone();
    let kinds = |k: u64| {
        events
            .iter()
            .filter(|e| e["kind"].as_u64() == Some(k))
            .count()
    };
    assert_eq!(kinds(9007), 1, "channel ensured once");
    assert_eq!(kinds(9021), 2, "both agents joined");
    assert_eq!(kinds(9), 1, "the greeting");
    let greeting = events
        .iter()
        .find(|e| e["kind"].as_u64() == Some(9))
        .unwrap();
    assert_eq!(
        greeting["pubkey"].as_str().unwrap(),
        &cpa.nostr_pubkey_hex()
    );
    assert_eq!(greeting["tags"][0][0], "h");
    assert_eq!(greeting["tags"][0][1], channel);
    // The channel create is OPEN (any member may post — no roster gate).
    let create = events
        .iter()
        .find(|e| e["kind"].as_u64() == Some(9007))
        .unwrap();
    assert!(create["content"].as_str().is_some());
    let create_tags: Vec<Vec<String>> =
        serde_json::from_value(create["tags"].clone()).unwrap_or_default();
    assert!(
        create_tags
            .iter()
            .any(|t| t.first() == Some(&"visibility".to_string())
                && t.get(1) == Some(&"open".to_string()))
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn profile_publish_lands_kind_zero_with_the_name() {
    let id = Identity::generate();
    let (relay_url, state, server) = freehold_testkit::relay::spawn().await;

    freehold_core::relay_http::publish_profile(
        &relay_url,
        &id.secret_seed(),
        "freehold",
        "the CPA",
    )
    .unwrap();

    let events = state.events.lock().clone();
    let profile = events
        .iter()
        .find(|e| e["kind"].as_u64() == Some(0))
        .expect("kind-0 profile published");
    assert_eq!(profile["pubkey"].as_str().unwrap(), id.nostr_pubkey_hex());
    let content: serde_json::Value =
        serde_json::from_str(profile["content"].as_str().unwrap()).unwrap();
    assert_eq!(content["name"], "freehold");
    // The event is self-consistent (id + BIP-340) — the wire shape clients
    // trust.
    freehold_core::nip98::verify_event(
        profile["pubkey"].as_str().unwrap(),
        profile["created_at"].as_i64().unwrap(),
        0,
        &[],
        profile["content"].as_str().unwrap(),
        profile["sig"].as_str().unwrap(),
    )
    .expect("profile verifies");
    let _ = id;

    server.abort();
}
