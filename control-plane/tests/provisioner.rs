//! Phase B acceptance: provision (B1), rotate (B2), revoke (B3), and the
//! no-master-key proof (B4) — the CP state holds no plaintext and no private
//! keys; only the runner's own injected key can open its ciphertext.

use std::fs;

use freehold_control_plane::provisioner::{
    self, provision_runner, ProvisionError, ProvisionRequest,
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
        },
    )
    .unwrap();

    // Package shipped: identity (both privkeys) + ciphertext-only secrets.json.
    assert!(runner_dir.join("identity.json").exists());
    assert!(runner_dir.join("secrets.json").exists());
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
        assert!(!state_raw.contains(key), "CP state must not serialize private keys ({key})");
    }
    assert!(state_raw.contains(&res.nostr_pubkey), "pubkeys are expected in state");
    assert!(state_raw.contains(&res.enc_pubkey), "pubkeys are expected in state");

    // The CP cannot decrypt — only the runner's injected key can.
    let rec = store.get_secret("vultr").unwrap();
    let blob = hex::decode(&rec.ciphertext_hex).unwrap();
    let runner_id = Identity::load(&runner_dir).unwrap();
    let opened = crypto::open(&hex32(&runner_id.enc_secret_hex()), &blob).unwrap();
    assert_eq!(opened, secret, "runner opens its own sealed secret");

    // A DIFFERENT key (e.g. a second runner) cannot open it.
    let other = Identity::generate();
    assert!(crypto::open(&hex32(&other.enc_secret_hex()), &blob).is_err());
}

#[test]
fn rotated_secret_reencrypts_and_replaces_everywhere() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "b2", b"old-key-value", &runner_dir);

    let before = store.get_secret("b2").unwrap().ciphertext_hex;
    provisioner::rotate_secret(&store, "b2", b"new-key-value").unwrap();
    let after = store.get_secret("b2").unwrap();
    assert_ne!(before, after.ciphertext_hex, "rotation must produce fresh ciphertext");
    assert!(after.rotated_at.is_some());

    // Package re-shipped; the runner's SAME key opens the new ciphertext.
    let pkg_raw = fs::read_to_string(runner_dir.join("secrets.json")).unwrap();
    assert!(!pkg_raw.contains("old-key-value") && !pkg_raw.contains("new-key-value"));
    let blob = hex::decode(&after.ciphertext_hex).unwrap();
    let runner_id = Identity::load(&runner_dir).unwrap();
    let opened = crypto::open(&hex32(&runner_id.enc_secret_hex()), &blob).unwrap();
    assert_eq!(opened, b"new-key-value");
}

#[test]
fn revoked_runner_cannot_be_rotated_or_reprovisioned() {
    let (base, store) = setup();
    let runner_dir = base.path().join("runner");
    provision(&store, "vultr", b"key", &runner_dir);

    provisioner::revoke_runner(&store, "vultr").unwrap();
    assert_eq!(store.get_runner("vultr").unwrap().status, RunnerStatus::Revoked);
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
        },
    )
    .unwrap_err();
    assert!(matches!(err, ProvisionError::RunnerExists(_)), "got {err:?}");
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
            },
        )
        .unwrap_err();
        assert!(matches!(err, ProvisionError::InvalidName(_)), "{bad:?} -> {err:?}");
    }
}

#[test]
fn unknown_secret_rotate_fails() {
    let (_, store) = setup();
    let err = provisioner::rotate_secret(&store, "nope", b"x").unwrap_err();
    assert!(matches!(err, ProvisionError::SecretNotFound(_)), "got {err:?}");
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
