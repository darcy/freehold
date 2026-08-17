//! Phase A bootstrap tests — hermetic, end-to-end through the REAL runner:
//! - proxmox-lxc: the driver execs `pvesm`/`pct` over a real ssh channel to
//!   the in-process sshd, which runs the commands with FAKE `pvesm`/`pct`
//!   planted in a per-test bin dir. The sshd prepends that dir to PATH for
//!   ITS OWN execs only (testkit `SshdOpts.path_prefix`) — no process-global
//!   env mutation, safe under concurrent tokio tests. The template
//!   discovery, create, start, and `pct exec` verify all run as if the
//!   laptop were answering.
//! - vultr-vps: the proven curl driver against the testkit Vultr mock.

use std::io::Write;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use freehold_core::crypto;
use freehold_core::identity::Identity;
use freehold_core::secrets::{SecretPackage, TargetMeta};
use freehold_orchestrator::bootstrap::{
    ProxmoxLxcSpec, VultrVpsSpec, bootstrap_proxmox_lxc, bootstrap_vultr_vps,
};
use freehold_orchestrator::client::McpClient;
use freehold_orchestrator::flows;
use freehold_runner::mcp::{self, RunnerContext};
use freehold_testkit::mock::{self, VultrState};
use freehold_testkit::sshd::{self, SshdOpts};

fn hex32(s: &str) -> [u8; 32] {
    let b = hex::decode(s).unwrap();
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&b);
    arr
}

/// Plant executable stub scripts into a fresh bin dir; returns
/// (bin_dir, keepalive). Every script first appends `$0 $*` to `log`.
fn plant_bin(log: &Path, scripts: &[(&str, &str)]) -> (PathBuf, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let bin = dir.path().join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    let logp = log.to_path_buf();
    for (name, body) in scripts {
        let p = bin.join(name);
        let mut f = std::fs::File::create(&p).unwrap();
        writeln!(f, "#!/bin/sh").unwrap();
        writeln!(f, "echo \"$0 $*\" >> '{}'", logp.display()).unwrap();
        write!(f, "{body}").unwrap();
        let mut perms = std::fs::metadata(&p).unwrap().permissions();
        perms.set_mode(0o755);
        std::fs::set_permissions(&p, perms).unwrap();
    }
    (bin, dir)
}

/// A happy PVE host: one template cached; create/start/exec succeed and the
/// guest answers hostname + Linux via `pct exec`.
const HAPPY_PVESM: &str = r#"
if [ "$1" = "list" ]; then
  echo "Volid Format Type Size VMID"
  echo "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst dir vztmpl 227829836 -"
  exit 0
fi
exit 1
"#;
const HAPPY_PCT: &str = r#"
case "$1" in
  create) echo "204"; exit 0;;
  start)  echo "204"; exit 0;;
  exec)   echo "testhost-101"; echo "Linux"; echo "root"; exit 0;;
  *)      echo "unknown pct $*" >&2; exit 2;;
esac
"#;

/// Boot a runner serving a CP-shaped ssh package; returns
/// (keepalive, url, runner_pubkey, server).
async fn serve_ssh_runner(
    name: &str,
    sshd_addr: &std::net::SocketAddr,
    agent_pubkey: &str,
) -> (
    tempfile::TempDir,
    String,
    String,
    tokio::task::JoinHandle<()>,
) {
    // The TempDir MUST stay alive for the runner's lifetime: the runner
    // re-reads grants from the package on DISK per call, and a dropped
    // TempDir deletes secrets.json -> fail-closed denial.
    let dir = tempfile::tempdir().unwrap();
    let id = Identity::generate();
    id.write_to_dir(dir.path()).unwrap();
    let (_key, pem) = sshd::client_key_pem();
    let enc = hex32(&id.enc_pubkey_hex());
    let sealed = hex::encode(crypto::seal(&enc, name.as_bytes(), pem.as_bytes()).unwrap());
    let pkg = SecretPackage {
        secrets: std::collections::BTreeMap::from([(name.to_string(), sealed)]),
        targets: std::collections::BTreeMap::from([(
            name.to_string(),
            TargetMeta {
                kind: "ssh".into(),
                address: format!("testuser@127.0.0.1:{}", sshd_addr.port()),
                secret: name.to_string(),
            },
        )]),
        grants: vec![agent_pubkey.to_string()],
    };
    pkg.write_to_dir(dir.path()).unwrap();
    let runner_pubkey = id.nostr_pubkey_hex();
    let ctx = RunnerContext {
        identity: id,
        package: SecretPackage::load(dir.path()).unwrap(),
        state_dir: dir.path().to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    (dir, format!("http://{addr}/mcp"), runner_pubkey, server)
}

fn client(agent_dir: &Path, url: &str, runner_pubkey: &str) -> McpClient {
    McpClient::new(
        url.to_string(),
        flows::agent_auth(agent_dir).unwrap(),
        runner_pubkey.to_string(),
    )
    .unwrap()
}

fn agent_dir(base: &std::path::Path) -> std::path::PathBuf {
    let dir = base.join("agent");
    let id = Identity::generate();
    id.write_to_dir(&dir).unwrap();
    dir
}

/// Common fixture for a proxmox-lxc driver test: an sshd with `bin` on its
/// PATH, a runner targeting it, and a ready client. Returns the pct.log
/// path + the client.
async fn proxmox_fixture(
    base: &Path,
    bin: &Path,
) -> (
    PathBuf,
    tempfile::TempDir,
    McpClient,
    tokio::task::JoinHandle<()>,
) {
    let log = base.join("pct.log");
    let (sshd_addr, _) = sshd::spawn_server_with(SshdOpts {
        reject_all_keys: false,
        path_prefix: Some(bin.to_path_buf()),
    })
    .await;
    let adir = agent_dir(base);
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();
    let (_runner_dir, url, rpk, server) =
        serve_ssh_runner("proxmox-box", &sshd_addr, &agent_pk).await;
    let client = client(&adir, &url, &rpk);
    (log, _runner_dir, client, server)
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_bootstrap_creates_starts_verifies() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _bin_alive) = plant_bin(
        &base.path().join("pct.log"),
        &[("pvesm", HAPPY_PVESM), ("pct", HAPPY_PCT)],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let res = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: 101,
            template: None, // exercises template discovery via pvesm
            storage: "local-lvm".into(),
            bridge: "vmbr0".into(),
        },
    )
    .await
    .unwrap();

    assert_eq!(
        res.kind,
        freehold_orchestrator::bootstrap::TargetKind::ProxmoxLxc
    );
    assert!(res.detail.contains("verified via `pct exec`"), "{res:?}");
    // The full driver sequence ran over real ssh: discovery -> create -> start -> exec.
    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(cmds.contains("pvesm list"), "template discovery: {cmds}");
    assert!(
        cmds.contains("pct create 101") && cmds.contains("debian-12-standard"),
        "create with detected template: {cmds}"
    );
    assert!(cmds.contains("pct start 101"), "start: {cmds}");
    assert!(cmds.contains("pct exec 101"), "verify: {cmds}");

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_create_failure_is_reported_with_output() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            (
                "pct",
                r#"
if [ "$1" = "create" ]; then echo "create failed for real"; exit 1; fi
exit 0
"#,
            ),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let err = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "boom".into(),
            vmid: 102,
            template: Some("debian-12-standard_12.7-1_amd64.tar.zst".into()),
            storage: "local-lvm".into(),
            bridge: "vmbr0".into(),
        },
    )
    .await
    .expect_err("pct create failure must surface");

    let msg = format!("{err}");
    assert!(msg.contains("pct create"), "step named: {msg}");
    assert!(msg.contains("create failed"), "output preserved: {msg}");
    assert!(
        std::fs::read_to_string(&log)
            .unwrap()
            .contains("pct create 102"),
        "the failing command actually ran"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_no_template_gate_gives_remediation() {
    let base = tempfile::tempdir().unwrap();
    // Empty template store: pvesm reports header only; pct absent entirely —
    // the driver must stop at the gate and never invoke create.
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[("pvesm", "echo 'Volid Format Type Size VMID'; exit 0\n")],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let err = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "nogate".into(),
            vmid: 103,
            template: None,
            storage: "local-lvm".into(),
            bridge: "vmbr0".into(),
        },
    )
    .await
    .expect_err("empty template store must stop at the gate");

    let msg = format!("{err}");
    assert!(
        msg.contains("no LXC template") && msg.contains("pveam download"),
        "operator remediation expected: {msg}"
    );
    let cmds = std::fs::read_to_string(&log).unwrap_or_default();
    assert!(
        !cmds.contains("pct create"),
        "must not attempt create: {cmds}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_vmid_below_100_is_rejected() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[("pvesm", HAPPY_PVESM), ("pct", HAPPY_PCT)],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let err = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "lowid".into(),
            vmid: 99,
            template: Some("debian-12-standard_12.7-1_amd64.tar.zst".into()),
            storage: "local-lvm".into(),
            bridge: "vmbr0".into(),
        },
    )
    .await
    .expect_err("vmid below 100 must be rejected");

    assert!(
        format!("{err}").contains("below the PVE system range"),
        "{err}"
    );
    // rejected before any exec; the log may not exist at all.
    let cmds = std::fs::read_to_string(&log).unwrap_or_default();
    assert!(
        !cmds.contains("pct create"),
        "must not attempt create: {cmds}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn vultr_vps_bootstrap_creates_polls_destroys() {
    let base = tempfile::tempdir().unwrap();
    let vultr_state = std::sync::Arc::new(VultrState::default());
    let vultr_addr = mock::spawn_http(mock::vultr_router(vultr_state.clone())).await;

    // Serve a vultr-target runner (same shape the G2.2 acceptance used).
    let dir = base.path().join("runner");
    let id = Identity::generate();
    id.write_to_dir(&dir).unwrap();
    let adir = agent_dir(base.path());
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();
    let enc = hex32(&id.enc_pubkey_hex());
    let sealed = hex::encode(crypto::seal(&enc, b"vultr", mock::VULTR_TOKEN.as_bytes()).unwrap());
    let pkg = SecretPackage {
        secrets: std::collections::BTreeMap::from([("vultr".to_string(), sealed)]),
        targets: std::collections::BTreeMap::from([(
            "vultr".to_string(),
            TargetMeta {
                kind: "vultr".into(),
                address: format!("http://{vultr_addr}"),
                secret: "vultr".into(),
            },
        )]),
        grants: vec![agent_pk.clone()],
    };
    pkg.write_to_dir(&dir).unwrap();
    let runner_pubkey = id.nostr_pubkey_hex();
    let ctx = RunnerContext {
        identity: id,
        package: SecretPackage::load(&dir).unwrap(),
        state_dir: dir.to_path_buf(),
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let client = client(&adir, &format!("http://{addr}/mcp"), &runner_pubkey);

    let res = bootstrap_vultr_vps(
        &client,
        "vultr",
        &VultrVpsSpec {
            label: "bootstrap-test".into(),
            region: "atl".into(),
            plan: "vhf-1c-1gb".into(),
            os_id: 1743,
            destroy_after: true,
        },
    )
    .await
    .unwrap();

    assert!(res.detail.contains("active"), "{res:?}");
    assert!(res.detail.contains("destroyed"), "{res:?}");
    assert!(
        vultr_state.instances.lock().is_empty(),
        "destroy-after must remove the instance from the mock"
    );

    server.abort();
}
