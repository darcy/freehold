//! Phase B acceptance: provision (B1), rotate (B2), revoke (B3), and the
//! no-master-key proof (B4) — the CP state holds no plaintext and no private
//! keys; only the runner's own injected key can open its ciphertext.

use std::collections::BTreeMap;
use std::fs;

use freehold_control_plane::provisioner::{
    self, ProvisionError, ProvisionRequest, adopt_runner, provision_runner,
};
use freehold_control_plane::state::{RunnerStatus, StateStore};
use freehold_core::{crypto, identity::Identity};

fn setup() -> (tempfile::TempDir, StateStore) {
    let base = tempfile::tempdir().unwrap();
    let store = StateStore::open(&base.path().join("state")).unwrap();
    (base, store)
}

fn hex32(s: &str) -> [u8; 32] {
    let b = hex::decode(s).unwrap();
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&b);
    arr
}

fn provision(store: &StateStore, name: &str, secret: &[u8], runner_dir: &std::path::Path) {
    provisioner::provision_runner(
        store,
        &ProvisionRequest {
            name,
            kind: "vultr",
            address: "api.vultr.com",
            secret,
            runner_dir,
            grants: &[],
            risk_level: None,
        },
    )
    .unwrap();
}

#[test]
fn provision_ships_package_and_cp_state_has_no_plaintext_or_keys() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    let secret = b"vultr-api-key-9876";

    let res = provision_runner(
        &store,
        &ProvisionRequest {
            name: "vultr",
            kind: "vultr",
            address: "api.vultr.com",
            secret,
            runner_dir: &runner_dir,
            grants: &[],
            risk_level: None,
        },
    )
    .unwrap();

    // Package shipped: identity (both privkeys) + ciphertext-only secrets.json.
    assert!(runner_dir.join("identity.json").exists());
    assert!(runner_dir.join("secrets.json").exists());
    // Target metadata shipped so the runner knows how to reach the service.
    let shipped = freehold_core::secrets::SecretPackage::load(&runner_dir).unwrap();
    let meta = shipped.targets.get("vultr").expect("target meta");
    assert_eq!(meta.kind, "vultr");
    assert_eq!(meta.address, "api.vultr.com");
    assert_eq!(meta.secret, "vultr");
    let pkg_raw = fs::read_to_string(runner_dir.join("secrets.json")).unwrap();
    assert!(
        !pkg_raw.contains(std::str::from_utf8(secret).unwrap()),
        "plaintext must never land in the runner package"
    );

    // B4: CP state holds pubkeys + ciphertext ONLY.
    let state_raw = fs::read_to_string(base.path().join("state").join("state.json")).unwrap();
    assert!(
        !state_raw.contains(std::str::from_utf8(secret).unwrap()),
        "plaintext must never land in CP state"
    );
    for key in ["nostr_secret", "enc_secret", "private", "secret_hex"] {
        assert!(
            !state_raw.contains(key),
            "CP state must not serialize private keys ({key})"
        );
    }
    assert!(
        state_raw.contains(&res.nostr_pubkey),
        "pubkeys are expected in state"
    );
    assert!(
        state_raw.contains(&res.enc_pubkey),
        "pubkeys are expected in state"
    );

    // The CP cannot decrypt — only the runner's injected key can. The blob is
    // opened through the SHIPPED package, under the map key it was filed
    // under (aad = name contract, exercised through the real path).
    let runner_id = Identity::load(&runner_dir).unwrap();
    let pkg = freehold_core::secrets::SecretPackage::load(&runner_dir).unwrap();
    let (entry_name, entry_ct) = pkg.secrets.iter().next().unwrap();
    let blob = hex::decode(entry_ct).unwrap();
    let opened = crypto::open(
        &hex32(&runner_id.enc_secret_hex()),
        entry_name.as_bytes(),
        &blob,
    )
    .unwrap();
    assert_eq!(opened, secret, "runner opens its own sealed secret");

    // A DIFFERENT key (e.g. a second runner) cannot open it.
    let other = Identity::generate();
    assert!(
        crypto::open(
            &hex32(&other.enc_secret_hex()),
            entry_name.as_bytes(),
            &blob
        )
        .is_err()
    );
}

#[test]
fn rotated_secret_reencrypts_and_replaces_everywhere() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "b2", b"old-key-value", &runner_dir);

    let before = store.get_secret("b2").unwrap().ciphertext_hex;
    provisioner::rotate_secret(&store, "b2", b"new-key-value").unwrap();
    let after = store.get_secret("b2").unwrap();
    assert_ne!(
        before, after.ciphertext_hex,
        "rotation must produce fresh ciphertext"
    );
    assert!(after.rotated_at.is_some());

    // Package re-shipped; the runner's SAME key opens the new ciphertext via
    // the shipped package's map key (aad = name, real path).
    let pkg_raw = fs::read_to_string(runner_dir.join("secrets.json")).unwrap();
    assert!(!pkg_raw.contains("old-key-value") && !pkg_raw.contains("new-key-value"));
    let runner_id = Identity::load(&runner_dir).unwrap();
    let pkg = freehold_core::secrets::SecretPackage::load(&runner_dir).unwrap();
    let (entry_name, entry_ct) = pkg.secrets.iter().next().unwrap();
    let blob = hex::decode(entry_ct).unwrap();
    let opened = crypto::open(
        &hex32(&runner_id.enc_secret_hex()),
        entry_name.as_bytes(),
        &blob,
    )
    .unwrap();
    assert_eq!(opened, b"new-key-value");
}

#[test]
fn revoked_runner_cannot_be_rotated_or_reprovisioned() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "vultr", b"key", &runner_dir);

    provisioner::revoke_runner(&store, "vultr").unwrap();
    assert_eq!(
        store.get_runner("vultr").unwrap().status,
        RunnerStatus::Revoked
    );
    assert_eq!(
        store.get_runner("vultr").unwrap().nostr_pubkey.len(),
        64,
        "revocation keeps the record (pubkey still listed, status revoked)"
    );

    assert!(matches!(
        provisioner::rotate_secret(&store, "vultr", b"whatever"),
        Err(ProvisionError::RunnerRevoked(_))
    ));
    assert!(matches!(
        provision_runner(
            &store,
            &ProvisionRequest {
                name: "vultr",
                kind: "vultr",
                address: "x",
                secret: b"x",
                runner_dir: &runner_dir,
                grants: &[],
                risk_level: None,
            }
        ),
        Err(ProvisionError::RunnerRevoked(_))
    ));
}

#[test]
fn duplicate_provision_is_rejected() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "vultr", b"key", &runner_dir);
    let err = provision_runner(
        &store,
        &ProvisionRequest {
            name: "vultr",
            kind: "vultr",
            address: "x",
            secret: b"x",
            runner_dir: &runner_dir,
            grants: &[],
            risk_level: None,
        },
    )
    .unwrap_err();
    assert!(
        matches!(err, ProvisionError::RunnerExists(_)),
        "got {err:?}"
    );
}

#[test]
fn swapped_package_entries_are_rejected() {
    // The aad = map-key contract, made structural: seal two secrets for one
    // runner under DIFFERENT names, ship them as a two-entry package, LOAD it
    // back through the real path, and prove each entry opens only under its
    // own map key — a swapped entry fails.
    let base = tempfile::tempdir().unwrap();
    let runner_dir = base.path().join("runner");
    freehold_core::futil::ensure_private_dir(&runner_dir).unwrap();
    let id = Identity::generate();
    let pubk = hex32(&id.enc_pubkey_hex());
    let ct_a = crypto::seal(&pubk, b"a", b"cred-a").unwrap();
    let ct_b = crypto::seal(&pubk, b"b", b"cred-b").unwrap();
    let pkg = freehold_core::secrets::SecretPackage {
        secrets: BTreeMap::from([
            ("a".to_string(), hex::encode(&ct_a)),
            ("b".to_string(), hex::encode(&ct_b)),
        ]),
        targets: BTreeMap::new(),
        grants: Vec::new(),
    };
    pkg.write_to_dir(&runner_dir).unwrap();
    let loaded = freehold_core::secrets::SecretPackage::load(&runner_dir).unwrap();

    let key = hex32(&id.enc_secret_hex());
    let blob_a = hex::decode(&loaded.secrets["a"]).unwrap();
    let blob_b = hex::decode(&loaded.secrets["b"]).unwrap();
    assert_eq!(crypto::open(&key, b"a", &blob_a).unwrap(), b"cred-a");
    assert_eq!(crypto::open(&key, b"b", &blob_b).unwrap(), b"cred-b");
    // The swap: open entry a's blob under b's name (and vice versa).
    assert!(
        crypto::open(&key, b"b", &blob_a).is_err(),
        "swapped entry must fail"
    );
    assert!(
        crypto::open(&key, b"a", &blob_b).is_err(),
        "swapped entry must fail"
    );
}

#[test]
fn package_dir_in_use_is_refused() {
    let (base, store) = setup();
    let runner_dir = base.path().join("shared");
    provision(&store, "a", b"key-a", &runner_dir);
    // Same dir via explicit path (the FREEHOLD_RUNNER_STATE_DIR footgun):
    // shipping b here would replace a's identity.json + secrets.json while
    // state still lists a active with undecryptable ciphertext.
    let err = provision_runner(
        &store,
        &ProvisionRequest {
            name: "b",
            kind: "vultr",
            address: "x",
            secret: b"key-b",
            runner_dir: &runner_dir,
            grants: &[],
            risk_level: None,
        },
    )
    .unwrap_err();
    assert!(
        matches!(err, ProvisionError::PackageDirInUse(_)),
        "got {err:?}"
    );
    // a's package is intact and still decryptable.
    let runner_id = Identity::load(&runner_dir).unwrap();
    let blob = hex::decode(store.get_secret("a").unwrap().ciphertext_hex).unwrap();
    assert_eq!(
        crypto::open(&hex32(&runner_id.enc_secret_hex()), b"a", &blob).unwrap(),
        b"key-a"
    );
}

#[test]
fn revoke_removes_shipped_credential() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "ssh", b"key", &runner_dir);
    assert!(runner_dir.join("identity.json").exists());
    assert!(
        runner_dir
            .join(freehold_core::secrets::SECRETS_FILE)
            .exists()
    );

    provisioner::revoke_runner(&store, "ssh").unwrap();
    assert_eq!(
        store.get_runner("ssh").unwrap().status,
        RunnerStatus::Revoked
    );
    assert!(
        !runner_dir
            .join(freehold_core::secrets::SECRETS_FILE)
            .exists(),
        "revoke must remove the shipped credential capability"
    );
    assert!(
        runner_dir.join("identity.json").exists(),
        "the runner's own identity stays"
    );
}

#[cfg(unix)]
#[test]
fn revoke_save_failure_restores_prior_status() {
    use std::os::unix::fs::PermissionsExt;
    // 0o500 only blocks writes for non-root — skip honestly in a root
    // container (e.g. `docker run rust`) instead of failing.
    if unsafe { libc::geteuid() } == 0 {
        eprintln!("skipping: root ignores read-only dir perms");
        return;
    }
    let base = tempfile::tempdir().unwrap();
    let state_dir = base.path().join("state");
    let store = StateStore::open(&state_dir).unwrap();
    let runner_dir = base.path().join("runner");
    provision(&store, "ssh", b"key", &runner_dir);

    // Force save() to fail: state dir read-only (fails as non-root).
    fs::set_permissions(&state_dir, fs::Permissions::from_mode(0o500)).unwrap();
    let _err = provisioner::revoke_runner(&store, "ssh").unwrap_err();
    fs::set_permissions(&state_dir, fs::Permissions::from_mode(0o700)).unwrap();

    // Memory reverted to the PRIOR status; disk untouched (still active) —
    // a failed revoke must never have flipped anything.
    assert_eq!(
        store.get_runner("ssh").unwrap().status,
        RunnerStatus::Active
    );
    let reopened = StateStore::open(&state_dir).unwrap();
    assert_eq!(
        reopened.get_runner("ssh").unwrap().status,
        RunnerStatus::Active
    );
}

#[test]
fn revoke_is_idempotent_and_never_grants() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "ssh", b"key", &runner_dir);
    provisioner::revoke_runner(&store, "ssh").unwrap();
    // A secrets.json that reappeared (config mgmt restore) must be removed by
    // re-revoking — the idempotent path still attempts the cleanup.
    freehold_core::secrets::SecretPackage::default()
        .write_to_dir(&runner_dir)
        .unwrap();
    let rec = provisioner::revoke_runner(&store, "ssh").unwrap();
    assert_eq!(rec.status, RunnerStatus::Revoked);
    assert!(
        !runner_dir
            .join(freehold_core::secrets::SECRETS_FILE)
            .exists(),
        "re-revoke must clean a reappeared secrets.json"
    );
    assert!(
        provisioner::rotate_secret(&store, "ssh", b"x").is_err(),
        "still revoked"
    );
}

#[test]
fn invalid_names_are_rejected() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    for bad in ["", "../x", "a/b", ".hidden"] {
        let err = provision_runner(
            &store,
            &ProvisionRequest {
                name: bad,
                kind: "vultr",
                address: "x",
                secret: b"x",
                runner_dir: &runner_dir,
                grants: &[],
                risk_level: None,
            },
        )
        .unwrap_err();
        assert!(
            matches!(err, ProvisionError::InvalidName(_)),
            "{bad:?} -> {err:?}"
        );
    }
}

#[test]
fn unknown_secret_rotate_fails() {
    let (_, store) = setup();
    let err = provisioner::rotate_secret(&store, "nope", b"x").unwrap_err();
    assert!(
        matches!(err, ProvisionError::SecretNotFound(_)),
        "got {err:?}"
    );
}

#[test]
fn provision_ships_grants_and_grant_adds_live() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    let agent_a = "aa".repeat(32);
    let agent_b = "bb".repeat(32);
    provision_runner(
        &store,
        &ProvisionRequest {
            name: "vultr",
            kind: "vultr",
            address: "api.vultr.com",
            secret: b"key",
            runner_dir: &runner_dir,
            grants: std::slice::from_ref(&agent_a.clone()),
            risk_level: None,
        },
    )
    .unwrap();

    // Shipped package whitelists exactly the granted pubkey.
    let shipped = freehold_core::secrets::SecretPackage::load(&runner_dir).unwrap();
    assert_eq!(shipped.grants, vec![agent_a]);

    // grant_agent adds another pubkey and re-ships (idempotent).
    let grants = provisioner::grant_agent(&store, "vultr", &agent_b).unwrap();
    assert_eq!(grants.len(), 2);
    provisioner::grant_agent(&store, "vultr", &agent_b).unwrap();
    let re_shipped = freehold_core::secrets::SecretPackage::load(&runner_dir).unwrap();
    assert_eq!(re_shipped.grants.len(), 2);
    assert!(re_shipped.grants.contains(&agent_b));

    // Grants are preserved across rotate (the package is rebuilt, not lost).
    provisioner::rotate_secret(&store, "vultr", b"new-key").unwrap();
    let after = freehold_core::secrets::SecretPackage::load(&runner_dir).unwrap();
    assert_eq!(after.grants.len(), 2, "rotate must preserve grants");
}

#[test]
fn invalid_grant_pubkeys_are_rejected() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "vultr", b"key", &runner_dir);
    let err = provisioner::grant_agent(&store, "vultr", "not-hex").unwrap_err();
    assert!(
        matches!(err, ProvisionError::InvalidGrant(_)),
        "got {err:?}"
    );
}

#[test]
fn state_persists_across_reopen() {
    let base = tempfile::tempdir().unwrap();
    let store = StateStore::open(&base.path().join("state")).unwrap();
    provision(&store, "ssh", b"key", &base.path().join("runner"));

    // Reopen from disk: same records, statuses, ciphertext.
    let reopened = StateStore::open(&base.path().join("state")).unwrap();
    let snap = reopened.snapshot();
    assert!(snap.runners.contains_key("ssh"));
    assert_eq!(snap.runners["ssh"].status, RunnerStatus::Active);
    assert_eq!(
        snap.secrets["ssh"].ciphertext_hex,
        store.get_secret("ssh").unwrap().ciphertext_hex
    );
}

#[tokio::test(flavor = "multi_thread")]
async fn grant_and_revoke_are_channel_membership_commands() {
    use freehold_core::nip98::{PUT_USER_KIND, REMOVE_USER_KIND};

    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "relaybox", b"sekrit", &runner_dir);
    let rec = store.get_runner("relaybox").unwrap();

    let (relay_url, state, _task) = freehold_testkit::relay::spawn().await;
    let console_dir = base.path().join("cp-state");
    // The console identity must EXIST (the publish path refuses to mint one)
    freehold_control_plane::console::Console::load_or_create(&console_dir).unwrap();

    let a = "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff";
    let b = "2221222233334444555566667777888899990000aaaabbbbccccddddeeeeffff";
    // The channel must exist before membership commands (owner gate).
    provisioner::sync_runner_channel(&store, &relay_url, "relaybox", &console_dir).unwrap();
    provisioner::grant_agent(&store, "relaybox", a).unwrap();
    provisioner::grant_agent(&store, "relaybox", b).unwrap();
    provisioner::put_user_membership(&store, &relay_url, "relaybox", a, &console_dir).unwrap();
    provisioner::put_user_membership(&store, &relay_url, "relaybox", b, &console_dir).unwrap();

    // The relay executed kind-9000 put-user commands (h = the derived
    // channel; p = the granted agent) — membership, not a grant event.
    let events = state.events.lock().clone();
    let puts: Vec<&serde_json::Value> = events
        .iter()
        .filter(|e| e["kind"].as_u64() == Some(PUT_USER_KIND as u64))
        .collect();
    assert_eq!(puts.len(), 3, "{events:?}"); // the runner + a + b
    let p_tags = |e: &serde_json::Value| -> Vec<String> {
        e["tags"]
            .as_array()
            .unwrap()
            .iter()
            .filter(|t| t[0] == "p")
            .map(|t| t[1].as_str().unwrap().to_string())
            .collect()
    };
    assert!(p_tags(puts[1]).contains(&a.to_string()) || p_tags(puts[2]).contains(&a.to_string()));
    // All members land on the relay-signed roster (the runner whitelist) —
    // read back over the REAL query wire, verified against the relay pubkey.
    let console = freehold_control_plane::console::Console::load(&console_dir).unwrap();
    let roster = || -> Vec<String> {
        freehold_core::relay_http::query_channel_roster(
            &relay_url,
            &freehold_testkit::relay::relay_pubkey(),
            &rec.nostr_pubkey,
            &console.identity.secret_seed(),
        )
        .unwrap()
    };
    let members = roster();
    assert!(members.contains(&a.to_string()));
    assert!(members.contains(&b.to_string()));
    assert!(members.contains(&rec.nostr_pubkey), "the runner itself");

    // Revoke = a kind-9001 remove-user command; the next roster drops b.
    provisioner::remove_user_membership(&store, &relay_url, "relaybox", b, &console_dir).unwrap();
    let events = state.events.lock().clone();
    let removes: Vec<&serde_json::Value> = events
        .iter()
        .filter(|e| e["kind"].as_u64() == Some(REMOVE_USER_KIND as u64))
        .collect();
    assert_eq!(removes.len(), 1);
    let members = roster();
    assert!(
        !members.contains(&b.to_string()),
        "b revoked from the roster"
    );
    assert!(members.contains(&a.to_string()), "a still a member");
}

#[test]
fn adopt_runner_registers_existing_package_without_reshipping() {
    use std::collections::BTreeMap;
    let base = tempfile::tempdir().unwrap();
    let store_dir = base.path().join("cp");
    let store = StateStore::open(&store_dir).unwrap();

    // Build a runner's EXISTING shipped package: identity + sealed credential
    // + grants, exactly as provision would have left it.
    let package_dir = base.path().join("runner");
    freehold_core::futil::ensure_private_dir(&package_dir).unwrap();
    let rid = Identity::generate();
    rid.write_to_dir(&package_dir).unwrap();
    let enc = hex32(&rid.enc_pubkey_hex());
    let sealed = hex::encode(crypto::seal(&enc, b"relay-box", b"the-ssh-key").unwrap());
    let pkg = freehold_core::secrets::SecretPackage {
        secrets: BTreeMap::from([("relay-box".to_string(), sealed)]),
        targets: BTreeMap::from([(
            "relay-box".to_string(),
            freehold_core::secrets::TargetMeta {
                kind: "ssh".into(),
                address: "root@192.168.30.224".into(),
                secret: "relay-box".into(),
            },
        )]),
        grants: vec!["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".into()],
    };
    pkg.write_to_dir(&package_dir).unwrap();

    // ADOPT: nothing re-shipped — the record is rebuilt from the package.
    let runner = adopt_runner(
        &store,
        "proxmox-box",
        "ssh",
        "root@192.168.30.224",
        &package_dir,
        Some("127.0.0.1:8787".into()),
        None,
    )
    .unwrap();

    assert_eq!(runner.nostr_pubkey, rid.nostr_pubkey_hex());
    assert_eq!(runner.enc_pubkey, rid.enc_pubkey_hex());
    assert_eq!(runner.status, RunnerStatus::Active);
    assert_eq!(runner.mcp_addr.as_deref(), Some("127.0.0.1:8787"));
    assert_eq!(runner.package_dir, package_dir);
    // the secret record carries the sealed credential + target meta
    let rec = store.get_runner("proxmox-box").unwrap();
    assert_eq!(rec.nostr_pubkey, runner.nostr_pubkey);
    let pkg_again = freehold_core::secrets::SecretPackage::load(&package_dir).unwrap();
    assert_eq!(pkg_again.grants.len(), 1, "grants read from the package");
    // the package is untouched by adopt (same sealed blob)
    let pkg_rec = store.get_secret("proxmox-box").unwrap();
    assert_eq!(pkg_rec.ciphertext_hex, pkg_again.secrets["relay-box"]);

    // re-adopt is refused (RunnerExists — the operator cleans up by hand)
    let dup = adopt_runner(
        &store,
        "proxmox-box",
        "ssh",
        "root@192.168.30.224",
        &package_dir,
        None,
        None,
    )
    .unwrap_err();
    assert!(dup.to_string().contains("already exists"), "{dup}");
}
