//! Phase A bootstrap tests — hermetic, end-to-end through the REAL runner:
//! - proxmox-lxc: the driver execs `pvesm`/`pct` over a real ssh channel to
//!   the in-process sshd, which runs the commands with a FAKE pvesm/pct on
//!   PATH (the test plants stubs that mimic the PVE host). The template
//!   discovery, create, start, and `pct exec` verify all run as if the
//!   laptop were answering.
//! - vultr-vps: the proven curl driver against the testkit Vultr mock.

use std::io::Write;
use std::sync::Mutex;

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
use freehold_testkit::sshd;

/// PATH is process-global; the fake pvesm/pct must be first for every test
/// in this file. Serialize the tests so the mutation cannot race.
static PATH_LOCK: Mutex<()> = Mutex::new(());

fn hex32(s: &str) -> [u8; 32] {
    let b = hex::decode(s).unwrap();
    let mut arr = [0u8; 32];
    arr.copy_from_slice(&b);
    arr
}

/// Boot a runner serving a CP-shaped package for target `name` (ssh kind,
/// key = the PEM for the in-process sshd). Returns (url, runner_pubkey, task).
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

/// A fake `pvesm`/`pct` bin that mimics a PVE host for our driver's command
/// shapes, logging every invocation so the sequence is assertable.
fn plant_stub_bin(log: &std::path::Path) -> (tempfile::TempDir, String) {
    let dir = tempfile::tempdir().unwrap();
    let bin = dir.path().join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    let logp = log.to_path_buf();

    let write_script = |name: &str, body: &str| {
        let p = bin.join(name);
        let mut f = std::fs::File::create(&p).unwrap();
        writeln!(f, "#!/bin/sh").unwrap();
        writeln!(f, "echo \"$0 $*\" >> '{}'", logp.display()).unwrap();
        write!(f, "{body}").unwrap();
        let mut perms = std::fs::metadata(&p).unwrap().permissions();
        use std::os::unix::fs::PermissionsExt;
        perms.set_mode(0o755);
        std::fs::set_permissions(&p, perms).unwrap();
    };

    write_script(
        "pvesm",
        r#"
if [ "$1" = "list" ]; then
  echo "Volid Format Type Size VMID"
  echo "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst dir vztmpl 227829836 -"
  exit 0
fi
exit 1
"#,
    );
    write_script(
        "pct",
        r#"
case "$1" in
  create) echo "204"; exit 0;;
  start)  echo "204"; exit 0;;
  exec)   echo "testhost-101"; echo "Linux"; echo "root"; exit 0;;
  *)      echo "unknown pct $*" >&2; exit 2;;
esac
"#,
    );
    // Failure-path variant is created on demand by the failing test.
    let mut path = String::from(bin.to_string_lossy());
    unsafe {
        std::env::set_var(
            "PATH",
            format!("{path}:{}", std::env::var("PATH").unwrap_or_default()),
        );
    }
    let _ = &mut path;
    (dir, "ignored".into())
}

fn client(agent_dir: &std::path::Path, url: &str, runner_pubkey: &str) -> McpClient {
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

#[tokio::test(flavor = "multi_thread")]
#[allow(clippy::await_holding_lock)] // the PATH_LOCK deliberately spans the test: PATH is process-global
async fn proxmox_lxc_bootstrap_creates_starts_verifies() {
    let _guard = PATH_LOCK.lock().unwrap();
    let base = tempfile::tempdir().unwrap();
    let log = base.path().join("pct.log");
    let (_stub, _) = plant_stub_bin(&log);

    let adir = agent_dir(base.path());
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();
    let (sshd_addr, _) = sshd::spawn_server(false).await;
    let (pkg_dir, url, rpk, server) = serve_ssh_runner("proxmox-box", &sshd_addr, &agent_pk).await;
    let _ = pkg_dir;
    let client = client(&adir, &url, &rpk);

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
#[allow(clippy::await_holding_lock)] // the PATH_LOCK deliberately spans the test: PATH is process-global
async fn proxmox_lxc_create_failure_is_reported_with_output() {
    let _guard = PATH_LOCK.lock().unwrap();
    let base = tempfile::tempdir().unwrap();
    let log = base.path().join("pct.log");
    let _stub = plant_stub_log_fail(&log);

    let adir = agent_dir(base.path());
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();
    let (sshd_addr, _) = sshd::spawn_server(false).await;
    let (pkg_dir, url, rpk, server) = serve_ssh_runner("proxmox-box", &sshd_addr, &agent_pk).await;
    let _ = pkg_dir;
    let client = client(&adir, &url, &rpk);

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

    server.abort();
}

/// Failure-path stub: pct create exits non-zero after logging.
fn plant_stub_log_fail(log: &std::path::Path) -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let bin = dir.path().join("bin");
    std::fs::create_dir_all(&bin).unwrap();
    let logp = log.to_path_buf();

    let write_script = |name: &str, body: &str| {
        let p = bin.join(name);
        let mut f = std::fs::File::create(&p).unwrap();
        writeln!(f, "#!/bin/sh").unwrap();
        writeln!(f, "echo \"$0 $*\" >> '{}'", logp.display()).unwrap();
        write!(f, "{body}").unwrap();
        let mut perms = std::fs::metadata(&p).unwrap().permissions();
        use std::os::unix::fs::PermissionsExt;
        perms.set_mode(0o755);
        std::fs::set_permissions(&p, perms).unwrap();
    };
    write_script("pvesm", "echo \"Volid Format Type Size VMID\"; exit 0\n");
    write_script(
        "pct",
        r#"
if [ "$1" = "create" ]; then echo "create failed for real"; exit 1; fi
exit 0
"#,
    );
    unsafe {
        std::env::set_var(
            "PATH",
            format!(
                "{}:{}",
                bin.to_string_lossy(),
                std::env::var("PATH").unwrap_or_default()
            ),
        );
    }
    dir
}

#[tokio::test(flavor = "multi_thread")]
async fn vultr_vps_bootstrap_creates_polls_destroys() {
    let base = tempfile::tempdir().unwrap();
    let vultr_state = std::sync::Arc::new(VultrState::default());
    let vultr_addr = mock::spawn_http(mock::vultr_router(vultr_state.clone())).await;
    let vultr_url = format!("http://{vultr_addr}");
    let _ = &vultr_url;

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
