//! Chunk 2.6.1 — runners as NIP-29 channels: the CP syncs each runner's
//! private channel (create + membership + metadata) at every lifecycle
//! mutation, the relay holds the relay-signed roster + replaceable channel
//! metas as the rebuildable projection, and `rebuild` folds the metas into
//! a fresh CP deterministically + idempotently.
//!
//! The fake relay (testkit) implements the real wire contract: NIP-98 auth,
//! POST /query with a filters array, POST /events, and the channel/membership
//! semantics. Trust (author gate + local Schnorr verify) is exercised, not
//! hand-waved.

use std::path::Path;

use freehold_control_plane::console::Console;
use freehold_control_plane::provisioner;
use freehold_control_plane::state::{RunnerRecord, RunnerStatus, SecretRecord, StateStore};
use freehold_core::nip98::GROUP_META_KIND;
use freehold_core::relay_http::{RunnerProfile, query_runner_metas};
use freehold_testkit::relay as fake_relay;

fn hex64(c: char) -> String {
    c.to_string().repeat(64)
}

fn store_with(dir: impl AsRef<Path>) -> StateStore {
    StateStore::open(dir.as_ref()).unwrap()
}

fn insert_runner(store: &StateStore, name: &str, status: RunnerStatus) {
    // Deterministic per-name pubkeys — every runner needs its own d-tag.
    let pk = name.chars().next().unwrap_or('z');
    store.insert_runner(
        name,
        RunnerRecord {
            nostr_pubkey: hex64(pk),
            enc_pubkey: hex64('b'),
            status,
            package_dir: Path::new("/tmp/pkg").to_path_buf(),
            created_at: 5,
            mcp_addr: None,
        },
    );
    store.insert_secret(
        name,
        SecretRecord {
            runner: name.to_string(),
            kind: "ssh".to_string(),
            address: "host:22".to_string(),
            ciphertext_hex: "aa".to_string(),
            created_at: 5,
            rotated_at: Some(7),
        },
    );
}

fn console_in(dir: &Path) -> Console {
    Console::load_or_create(dir).unwrap()
}

/// Real wire: provisioner channel sync (HTTP POST /events: create +
/// member + meta) -> relay_http query (HTTP POST /query) round-trips the
/// FULL profile as the channel's group metadata.
#[tokio::test(flavor = "multi_thread")]
async fn channel_sync_and_query_meta_roundtrip() {
    let base = tempfile::tempdir().unwrap();
    let store = store_with(base.path().join("cp")); // note: below opens its own dir
    let cp_state = base.path().join("cp-state");
    let console = console_in(&cp_state);
    let (relay_url, _state, _task) = fake_relay::spawn().await;

    // Records + secret for the runner, then sync the channel via the real
    // provisioner path (console-signed channel create + put-user + meta).
    insert_runner(&store, "relaybox", RunnerStatus::Active);
    provisioner::sync_runner_channel(&store, &relay_url, "relaybox", &cp_state).unwrap();

    // The relay's current view: exactly one meta, full content fidelity.
    let profiles = query_runner_metas(
        &relay_url,
        &console.identity.nostr_pubkey_hex(),
        &console.identity.secret_seed(),
    )
    .unwrap();
    assert_eq!(profiles.len(), 1, "{profiles:?}");
    let p = &profiles[0];
    assert_eq!(p.name, "relaybox");
    assert_eq!(p.kind, "ssh");
    assert_eq!(p.address, "host:22");
    assert_eq!(p.status, "active");
    assert_eq!(p.nostr_pubkey, hex64('r'));
    assert_eq!(p.enc_pubkey, hex64('b'));
    assert_eq!(p.secret, "relaybox"); // secret NAME only — never material
    assert_eq!(p.rotated_at, Some(7));

    // The runner the CP membered can read its own roster (the whitelist).
    let roster = freehold_core::relay_http::query_channel_roster(
        &relay_url,
        &fake_relay::relay_pubkey(),
        &hex64('r'),
        &console.identity.secret_seed(),
    )
    .unwrap();
    assert!(
        roster.contains(&hex64('r')),
        "the runner must be a member of its own channel"
    );
    assert!(
        roster.contains(&console.identity.nostr_pubkey_hex()),
        "the console owner is a member of the channel it created"
    );
}

/// Revocation REPLACES the meta (status flip, same h) AND cuts the runner
/// off its own roster — the relay never accumulates per-runner history a
/// stale fold could resurrect, and the runner's whitelist read fails closed.
#[tokio::test(flavor = "multi_thread")]
async fn revoke_flips_meta_and_cuts_off_the_runner() {
    let base = tempfile::tempdir().unwrap();
    let store = store_with(base.path().join("cp"));
    let cp_state = base.path().join("cp-state");
    console_in(&cp_state);
    let (relay_url, state, _task) = fake_relay::spawn().await;

    insert_runner(&store, "relaybox", RunnerStatus::Active);
    provisioner::sync_runner_channel(&store, &relay_url, "relaybox", &cp_state).unwrap();
    let metas_after_active = state
        .events
        .lock()
        .iter()
        .filter(|e| e["kind"].as_u64() == Some(GROUP_META_KIND as u64))
        .count();

    // Revoke: the CP flips the record, then revokes the channel (removes the
    // runner from its own roster + REPLACES the meta's status).
    store
        .set_runner_status("relaybox", RunnerStatus::Revoked)
        .unwrap();
    provisioner::revoke_runner_channel(&store, &relay_url, "relaybox", &cp_state).unwrap();

    let events = state.events.lock().clone();
    let metas = events
        .iter()
        .filter(|e| e["kind"].as_u64() == Some(GROUP_META_KIND as u64))
        .count();
    assert_eq!(
        metas,
        metas_after_active + 1,
        "the revoke re-publishes the meta ONE more time (same h — replace,          never appended history)"
    );

    // The fold sees ONE meta, revoked (query with the real console key).
    let console = Console::load(&cp_state).unwrap();
    let profiles = query_runner_metas(
        &relay_url,
        &console.identity.nostr_pubkey_hex(),
        &console.identity.secret_seed(),
    )
    .unwrap();
    assert_eq!(profiles.len(), 1, "one record per runner, newest wins");
    assert_eq!(profiles[0].status, "revoked");

    // The cut-off: the revoked runner is no longer on its own roster — its
    // whitelist read returns empty (deny-all), the enforcement point.
    let roster = freehold_core::relay_http::query_channel_roster(
        &relay_url,
        &fake_relay::relay_pubkey(),
        &hex64('r'),
        &console.identity.secret_seed(),
    )
    .unwrap();
    assert!(
        !roster.contains(&hex64('r')),
        "revoked runner exits its own channel"
    );
}

/// A rogue member's NEWER channel meta (any author, any number) is
/// ignored: the author gate + local signature verify are the trust anchor
/// for folds — exactly as a rogue 9000 put-user is refused at the relay.
#[tokio::test(flavor = "multi_thread")]
async fn rogue_author_cannot_mint_or_clobber_metas() {
    use freehold_core::nip98::{GROUP_META_KIND, sign_event};

    let base = tempfile::tempdir().unwrap();
    let store = store_with(base.path().join("cp"));
    let cp_state = base.path().join("cp-state");
    let console = console_in(&cp_state);
    let (relay_url, state, _task) = fake_relay::spawn().await;

    insert_runner(&store, "relaybox", RunnerStatus::Active);
    provisioner::sync_runner_channel(&store, &relay_url, "relaybox", &cp_state).unwrap();

    // Rogue mints a "revoked" meta for the SAME channel with a much newer
    // timestamp (h = the derived channel id).
    let h = freehold_core::relay_http::runner_channel_id(&hex64('r'));
    let rogue_secret = [3u8; 32];
    let rogue = RunnerProfile {
        name: "relaybox".into(),
        kind: "ssh".into(),
        address: "evil:22".into(),
        status: "revoked".into(),
        nostr_pubkey: hex64('r'),
        enc_pubkey: hex64('b'),
        secret: "relaybox".into(),
        created_at: 5,
        rotated_at: None,
    };
    let ts = freehold_core::auth::now_secs() + 100;
    let content = serde_json::to_string(&rogue).unwrap();
    let (pk, id, sig) = sign_event(
        &rogue_secret,
        GROUP_META_KIND,
        ts,
        vec![vec!["h".into(), h.clone()], vec!["d".into(), h.clone()]],
        &content,
    )
    .unwrap();
    state.events.lock().push(serde_json::json!({
        "id": id,
        "pubkey": pk,
        "created_at": ts,
        "kind": GROUP_META_KIND,
        "tags": [["h", h.clone()], ["d", h]],
        "content": content,
        "sig": sig,
    }));

    let profiles = query_runner_metas(
        &relay_url,
        &console.identity.nostr_pubkey_hex(),
        &console.identity.secret_seed(),
    )
    .unwrap();
    assert_eq!(profiles.len(), 1);
    assert_eq!(
        profiles[0].status, "active",
        "rogue-author meta must be ignored despite being newer"
    );
    assert_eq!(profiles[0].address, "host:22");
}

/// The disposable-CP fold: `rebuild_from` reconstructs the store from the
/// relay deterministically, and a second run converges (idempotent) to the
/// SAME state — the replay requirement, not a nice-to-have.
#[tokio::test(flavor = "multi_thread")]
async fn rebuild_folds_snapshots_idempotently() {
    use freehold_core::relay_http::publish_runner_meta as core_publish;

    let base = tempfile::tempdir().unwrap();
    let src_store = store_with(base.path().join("cp"));
    let cp_state = base.path().join("cp-state");
    let console = console_in(&cp_state);
    let (relay_url, _state, _task) = fake_relay::spawn().await;
    let console_secret = console.identity.secret_seed();

    // Two runners on the relay: alpha active, beta revoked (a rotated secret).
    for (i, (name, status)) in [
        ("alpha", RunnerStatus::Active),
        ("beta", RunnerStatus::Revoked),
    ]
    .into_iter()
    .enumerate()
    {
        insert_runner(&src_store, name, status);
        provisioner::sync_runner_channel(&src_store, &relay_url, name, &cp_state).unwrap();
        let profiles = {
            // Re-sign through the core publish so rotated_at differs per runner.
            let rec = src_store.get_runner(name).unwrap();
            let sec = src_store.get_secret(name).unwrap();
            RunnerProfile {
                name: name.to_string(),
                kind: sec.kind,
                address: format!("host{}:22", i + 1),
                status: if status == RunnerStatus::Revoked {
                    "revoked"
                } else {
                    "active"
                }
                .into(),
                nostr_pubkey: rec.nostr_pubkey,
                enc_pubkey: rec.enc_pubkey,
                secret: name.to_string(),
                created_at: 10 + i as u64,
                rotated_at: if status == RunnerStatus::Revoked {
                    Some(9)
                } else {
                    None
                },
            }
        };
        core_publish(&relay_url, &console_secret, &profiles).unwrap();
    }

    // Fold into a FRESH cp dir, then fold AGAIN into another: idempotent.
    let fold_once = base.path().join("fold1");
    let fold_twice = base.path().join("fold2");
    let fold = |dir: &Path| -> (StateStore, Vec<(String, RunnerStatus, Option<u64>)>) {
        let store = StateStore::open(dir).unwrap();
        let profiles = query_runner_metas(
            &relay_url,
            &console.identity.nostr_pubkey_hex(),
            &console_secret,
        )
        .unwrap();
        let mut runners = std::collections::BTreeMap::new();
        let mut secrets = std::collections::BTreeMap::new();
        for p in &profiles {
            runners.insert(
                p.name.clone(),
                RunnerRecord {
                    nostr_pubkey: p.nostr_pubkey.clone(),
                    enc_pubkey: p.enc_pubkey.clone(),
                    status: if p.status == "revoked" {
                        RunnerStatus::Revoked
                    } else {
                        RunnerStatus::Active
                    },
                    package_dir: Path::new("").to_path_buf(),
                    created_at: p.created_at,
                    mcp_addr: None,
                },
            );
            secrets.insert(
                p.name.clone(),
                SecretRecord {
                    runner: p.name.clone(),
                    kind: p.kind.clone(),
                    address: p.address.clone(),
                    ciphertext_hex: String::new(),
                    created_at: p.created_at,
                    rotated_at: p.rotated_at,
                },
            );
        }
        store.rebuild_from(runners, secrets).unwrap();
        let collected: Vec<_> = profiles
            .iter()
            .map(|p| {
                (
                    p.name.clone(),
                    if p.status == "revoked" {
                        RunnerStatus::Revoked
                    } else {
                        RunnerStatus::Active
                    },
                    p.rotated_at,
                )
            })
            .collect();
        (store, collected)
    };

    let (store1, view1) = fold(&fold_once);
    let (store2, view2) = fold(&fold_twice);

    // Both folds see both runners, with the right statuses + rotation.
    assert_eq!(view1.len(), 2, "{view1:?}");
    assert!(
        view1
            .iter()
            .any(|(n, s, r)| n == "alpha" && *s == RunnerStatus::Active && r.is_none())
    );
    assert!(
        view1
            .iter()
            .any(|(n, s, r)| n == "beta" && *s == RunnerStatus::Revoked && *r == Some(9))
    );
    // A re-run converges: identical records AND identical persisted state.
    assert_eq!(view1, view2);
    let s1 = store1.snapshot();
    let s2 = store2.snapshot();
    assert_eq!(s1.runners, s2.runners);
    assert_eq!(s1.secrets, s2.secrets);
    // Restored records carry NO ciphertext/package path (relay holds no
    // secret material) — the honest "re-adopt to re-arm" contract.
    assert!(s1.secrets.values().all(|s| s.ciphertext_hex.is_empty()));
    assert!(
        s1.runners
            .values()
            .all(|r| r.package_dir.as_os_str().is_empty())
    );
}
