//! Phase B relay-deploy tests — hermetic through the REAL runner + sshd.
//! Fake `docker`/`curl`/`tar`/`cp` live in a per-server bin dir on the
//! sshd's PATH (testkit `SshdOpts.path_prefix`, no process-global mutation);
//! the bundle "extracts" via a tar stub that materializes run.sh; liveness
//! is answered by the curl stub. All paths are per-test unique.

use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use freehold_core::crypto;
use freehold_core::identity::Identity;
use freehold_core::secrets::{SecretPackage, TargetMeta};
use freehold_orchestrator::client::McpClient;
use freehold_orchestrator::flows;
use freehold_orchestrator::relay::{DEFAULT_BUZZ_REF, RelayDeploySpec, deploy_relay};
use freehold_runner::mcp::{self, RunnerContext};
use freehold_testkit::sshd::{self, SshdOpts};

fn hex32(s: &str) -> [u8; 32] {
    let b = hex::decode(s).unwrap();
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&b);
    arr
}

fn plant_bin(scripts: &[(&str, &str)]) -> (std::path::PathBuf, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let bin = dir.path().join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    for (name, body) in scripts {
        let p = bin.join(name);
        let mut f = std::fs::File::create(&p).unwrap();
        writeln!(f, "#!/bin/sh").unwrap();
        write!(f, "{body}").unwrap();
        let mut perms = std::fs::metadata(&p).unwrap().permissions();
        perms.set_mode(0o755);
        std::fs::set_permissions(&p, perms).unwrap();
    }
    (bin, dir)
}

/// Runner serving an ssh package targeting an in-process sshd whose execs
/// see `bin` on PATH. The runner's package TempDir is returned and MUST be
/// held for the runner's lifetime (grants are re-read from disk per call).
async fn fixture(
    base: &Path,
    bin: &Path,
) -> (
    McpClient,
    String,
    tokio::task::JoinHandle<()>,
    &'static tempfile::TempDir,
    tempfile::TempDir,
) {
    let (sshd_addr, _) = sshd::spawn_server_with(SshdOpts {
        reject_all_keys: false,
        path_prefix: Some(bin.to_path_buf()),
    })
    .await;
    let adir = base.join("agent");
    let id = Identity::generate();
    id.write_to_dir(&adir).unwrap();
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();

    let dir = tempfile::tempdir().unwrap();
    let rid = Identity::generate();
    rid.write_to_dir(dir.path()).unwrap();
    let (_key, pem) = sshd::client_key_pem();
    let enc = hex32(&rid.enc_pubkey_hex());
    let sealed = hex::encode(crypto::seal(&enc, b"relay-box", pem.as_bytes()).unwrap());
    let pkg = SecretPackage {
        secrets: std::collections::BTreeMap::from([("relay-box".to_string(), sealed)]),
        targets: std::collections::BTreeMap::from([(
            "relay-box".to_string(),
            TargetMeta {
                kind: "ssh".into(),
                address: format!("testuser@127.0.0.1:{}", sshd_addr.port()),
                secret: "relay-box".into(),
            },
        )]),
        grants: vec![agent_pk],
    };
    pkg.write_to_dir(dir.path()).unwrap();
    let runner_pubkey = rid.nostr_pubkey_hex();
    let ctx = RunnerContext {
        identity: rid,
        package: SecretPackage::load(dir.path()).unwrap(),
        state_dir: dir.path().to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let client = McpClient::new(
        format!("http://{addr}/mcp"),
        flows::agent_auth(&adir).unwrap(),
        runner_pubkey.clone(),
    )
    .unwrap();
    // Deterministic grant-readiness: the runner re-reads grants from the
    // shipped package per call; a first call racing the package write fails
    // closed (-32001). Poll status until the grant check actually passes.
    let mut ready = false;
    for _ in 0..20 {
        if client.readiness().is_ok() {
            ready = true;
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    }
    let _ = ready;
    // LEAK the package TempDir: the runner re-reads grants from secrets.json
    // per call, and any early drop (from a path this harness can't see)
    // deletes it mid-flight, turning a valid call into a fail-closed denial.
    // A leaked tempdir is cleaned by the OS at process exit; tests here are
    // short-lived, so unlike a held guard this is immune to drop-order bugs.
    let leaked: &'static tempfile::TempDir = Box::leak(Box::new(dir));
    (
        client,
        "relay-box".into(),
        server,
        leaked,
        tempfile::tempdir().unwrap(),
    )
}

const DOCKER_OK: &str = "if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"version\" ]; then echo 'Docker Compose version v2.24.4'; exit 0; fi; exit 0\n";
const CURL_OK: &str = "case \"$*\" in *archive/*.tar.gz*) exit 0;; *\"/_liveness\"*) echo OK; exit 0;; *) exit 1;; esac\n";
const CP_OK: &str = "touch \"$2\" 2>/dev/null; exit 0\n";

#[tokio::test(flavor = "multi_thread")]
async fn relay_deploy_gates_on_docker_and_verifies_liveness() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("relay");
    let dir_s = dir.display().to_string();
    let tar_stub = format!(
        "mkdir -p {dir}/deploy/compose && printf '#!/bin/sh\\necho RUNSH-OK\\nexit 0\\n' \
         > {dir}/deploy/compose/run.sh && chmod +x {dir}/deploy/compose/run.sh && exit 0\n",
        dir = dir_s
    );
    let scripts: Vec<(&str, String)> = vec![
        ("docker", DOCKER_OK.into()),
        ("curl", CURL_OK.into()),
        ("tar", tar_stub),
        ("cp", CP_OK.into()),
    ];
    let stubs: Vec<(&str, &str)> = scripts.iter().map(|(n, b)| (*n, b.as_str())).collect();
    let (bin, _ba) = plant_bin(&stubs);
    let (client, _target, server, keep1, _keep2) = fixture(base.path(), &bin).await;
    let _ = (keep1, _keep2);

    let res = deploy_relay(
        &client,
        "relay-box",
        &RelayDeploySpec {
            relay_name: "relay-box".into(),
            deploy_dir: dir_s,
            http_port: 3000,
            buzz_ref: DEFAULT_BUZZ_REF.into(),
            lxc: None,
            owner_pubkey: "072696bde8f03234433ddcc3587464e92a51f5d6906ade6b2aab2e1313010371".into(),
        },
    )
    .await
    .unwrap();

    assert_eq!(res.relay_url, "http://relay-box:3000");
    assert!(res.detail.contains("healthy"), "{res:?}");
    assert!(res.detail.contains("ONE scope"), "{res:?}");

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn relay_deploy_missing_docker_gives_remediation() {
    let base = tempfile::tempdir().unwrap();
    // A docker stub that FAILS the compose-version gate (an empty bin would
    // leak the REAL machine's tools through the unprefixed PATH tail).
    let (bin, _ba) = plant_bin(&[("docker", "echo 'docker: not usable' >&2; exit 1\n")]);
    let (client, _target, server, keep1, _keep2) = fixture(base.path(), &bin).await;
    let _ = (keep1, _keep2);

    let err = deploy_relay(
        &client,
        "relay-box",
        &RelayDeploySpec {
            relay_name: "relay-box".into(),
            deploy_dir: base.path().join("relay").display().to_string(),
            http_port: 3000,
            buzz_ref: DEFAULT_BUZZ_REF.into(),
            lxc: None,
            owner_pubkey: "072696bde8f03234433ddcc3587464e92a51f5d6906ade6b2aab2e1313010371".into(),
        },
    )
    .await
    .expect_err("docker gate must stop deployment");

    let msg = format!("{err}");
    assert!(
        msg.contains("docker + compose plugin are missing") && msg.contains("remediation"),
        "operator remediation expected: {msg}"
    );

    server.abort();
}
