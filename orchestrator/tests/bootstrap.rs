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
/// guest answers hostname + Linux via `pct exec`, with docker+compose
/// already installed (`docker info` answers "Server Version").
const HAPPY_PVESM: &str = r#"
if [ "$1" = "list" ]; then
  echo "Volid Format Type Size VMID"
  echo "local:vztmpl/debian-12-standard_12.7-1_amd64.tar.zst dir vztmpl 227829836 -"
  exit 0
fi
exit 1
"#;
// Dispatch on the exec argv: `docker info` is `pct exec <id> -- docker info`
// ($4=docker), the sh -c payloads arrive in $6. The compose-version payload
// runs the driver's install-if-missing wrapper ("docker already present").
const HAPPY_PCT: &str = r#"
case "$1" in
  list)   echo "VMID Status Lock Name"
          echo "100 stopped - taken"
          echo "102 stopped - other"; exit 0;;
  create) echo "204"; exit 0;;
  start)  echo "204"; exit 0;;
  exec)
    case "$4" in
      docker) echo "Server Version: 27.0"; exit 0;;
      *)
        case "$6" in
          *"compose version"*) echo "Docker Compose version v2.24.4"; exit 0;;
          *) echo "testhost-101"; echo "Linux"; echo "root"; exit 0;;
        esac
        ;;
    esac
    ;;
  *)      echo "unknown pct $*" >&2; exit 2;;
esac
"#;
/// The PVE catalog + download endpoint: `pveam update` syncs, `available`
/// lists a NEWER-version ARM64 template the driver MUST skip (real catalogs
/// mix arches — observed live) plus two debian-12 amd64 rows (12.7 and the
/// NEWER 12.10), `download` succeeds. Columns mirror the REAL pveam output:
/// `system` is the FIRST token, the template name the SECOND (nth(1)).
const PVEAM: &str = r#"
case "$1" in
  update)    echo "ok"; exit 0;;
  available) echo "system debian-13-standard_13.6-1_arm64.tar.zst 123M 0"
             echo "system debian-12-standard_12.7-1_amd64.tar.zst 227M 0"
             echo "system debian-12-standard_12.10-1_amd64.tar.zst 228M 0"; exit 0;;
  download)  echo "204"; exit 0;;
  *)         echo "unknown pveam $*" >&2; exit 2;;
esac
"#;
/// A guest whose docker daemon never comes up: compose (the install probe)
/// answers, but `docker info` fails — the fallback path must engage.
const PCT_EXEC_DOCKER_FAIL: &str = r#"
case "$1" in
  create) echo "204"; exit 0;;
  start)  echo "204"; exit 0;;
  exec)
    case "$4" in
      docker) echo "cannot connect to the Docker daemon at unix:///var/run/docker.sock"; exit 1;;
      *)
        case "$6" in
          *"compose version"*) echo "Docker Compose version v2.24.4"; exit 0;;
          *) echo "testhost-101"; echo "Linux"; echo "root"; exit 0;;
        esac
        ;;
    esac
    ;;
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
    // Settle: the runner re-reads grants from disk per call; a racing first
    // call could observe a not-yet-visible secrets.json (flaky fail-closed).
    tokio::time::sleep(std::time::Duration::from_millis(60)).await;
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
async fn proxmox_lxc_reuses_present_template_docker_ready() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _bin_alive) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            ("pct", HAPPY_PCT),
            // pveam NOT planted: the happy store already holds a template,
            // so ensurement must reuse it — any pveam call would 127.
            ("pveam", "exit 127\n"),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let res = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: Some(101),
            template: None, // exercises template ensurement via pvesm
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr0".into(),
        },
    )
    .await
    .unwrap();

    assert_eq!(
        res.kind,
        freehold_orchestrator::bootstrap::TargetKind::ProxmoxLxc
    );
    assert!(
        res.detail.contains("docker+compose ready"),
        "docker readiness reported: {res:?}"
    );
    // The full driver sequence ran over real ssh: ensure -> create -> start ->
    // exec verify -> docker install/verify. Template came from the STORE, so
    // no pveam call happened (idempotent).
    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(cmds.contains("pvesm list local"), "template check: {cmds}");
    assert!(
        !cmds.contains("pveam"),
        "present template must be REUSED, no pveam calls: {cmds}"
    );
    assert!(
        cmds.contains("pct create 101") && cmds.contains("debian-12-standard"),
        "create with detected template: {cmds}"
    );
    assert!(
        cmds.contains("--unprivileged 1"),
        "the container must come out UNPRIVILEGED (its host will hold relay + CP): {cmds}"
    );
    assert!(
        cmds.contains("--features fuse=1,keyctl=1,nesting=1"),
        "nesting+keyctl are required for docker-in-LXC: {cmds}"
    );
    assert!(cmds.contains("pct start 101"), "start: {cmds}");
    assert!(
        cmds.contains("docker.io docker-compose-v2"),
        "Debian docker+compose-v2 install wrapper present: {cmds}"
    );
    assert!(
        cmds.contains("export DEBIAN_FRONTEND=noninteractive"),
        "debconf noninteractive must actually reach apt (exported): {cmds}"
    );
    assert!(
        cmds.contains("download.docker.com/linux/debian/gpg"),
        "bookworm fallback to Docker's own repo present: {cmds}"
    );
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
            vmid: Some(102),
            template: Some("debian-12-standard_12.7-1_amd64.tar.zst".into()),
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
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
async fn proxmox_lxc_downloads_template_when_missing() {
    let base = tempfile::tempdir().unwrap();
    // Empty template store: pvesm reports header only, so ensurement must
    // sync the catalog and download the NEWEST available debian template.
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", "echo 'Volid Format Type Size VMID'; exit 0\n"),
            ("pct", HAPPY_PCT),
            ("pveam", PVEAM),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let res = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: Some(104),
            template: None,
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr0".into(),
        },
    )
    .await
    .expect("empty store must download instead of blocking");

    let cmds = std::fs::read_to_string(&log).unwrap();
    assert_eq!(
        cmds.matches("pveam update").count(),
        1,
        "catalog sync exactly once: {cmds}"
    );
    assert!(
        cmds.contains("pveam download local debian-12-standard_12.10-1"),
        "downloads the NEWEST HOST-ARCH version (12.10 > 12.7): {cmds}"
    );
    assert!(
        !cmds.contains("arm64"),
        "the NEWER arm64 row must be skipped (host arch filter): {cmds}"
    );
    assert!(
        cmds.contains("pct create 104") && cmds.contains("debian-12-standard_12.10-1"),
        "create uses the downloaded template: {cmds}"
    );
    assert!(
        res.detail.contains("debian-12-standard_12.10-1")
            && res.detail.contains("docker+compose ready"),
        "result names the downloaded template + docker: {res:?}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_picks_free_vmid_when_omitted() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[("pvesm", HAPPY_PVESM), ("pct", HAPPY_PCT)],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let res = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: None, // 100 and 102 are taken in the stub -> picks 101
            template: None,
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr0".into(),
        },
    )
    .await
    .unwrap();

    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(cmds.contains("pct list"), "vmids probed: {cmds}");
    assert!(
        cmds.contains("pct create 101"),
        "lowest free vmid (100 taken, 102 taken) picked: {cmds}"
    );
    assert!(
        !cmds.contains("pct create 100") && !cmds.contains("pct create 102"),
        "an in-use vmid must never be reused: {cmds}"
    );
    assert_eq!(res.id, "101", "result reports the picked vmid: {res:?}");

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_docker_daemon_failure_is_reported() {
    let base = tempfile::tempdir().unwrap();
    // docker info repeatedly fails: the driver must try the fuse-overlayfs
    // fallback, and AFTER it still surface the daemon error with a hint.
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[("pvesm", HAPPY_PVESM), ("pct", PCT_EXEC_DOCKER_FAIL)],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let err = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: Some(105),
            template: None,
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr0".into(),
        },
    )
    .await
    .expect_err("a daemon that never starts must fail the bootstrap");

    let msg = format!("{err}");
    assert!(
        msg.contains("cannot connect") && msg.contains("consider a privileged container"),
        "raw daemon error + hint surfaced: {msg}"
    );
    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(
        cmds.contains("fuse-overlayfs"),
        "the fuse fallback must have run: {cmds}"
    );
    assert!(
        cmds.contains("daemon.json"),
        "storage-driver pin written: {cmds}"
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
            vmid: Some(99),
            template: Some("debian-12-standard_12.7-1_amd64.tar.zst".into()),
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
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
async fn vultr_vps_env_prefix_derives_from_target_name() {
    // The runner injects the credential as <ENV>/<ENV>_URL named after the
    // TARGET. A target named `box` (kind vultr) must drive with $BOX/$BOX_URL,
    // not hardcoded $VULTR_URL which would expand empty.
    let base = tempfile::tempdir().unwrap();
    let vultr_state = std::sync::Arc::new(VultrState::default());
    let vultr_addr = mock::spawn_http(mock::vultr_router(vultr_state.clone())).await;

    let dir = base.path().join("runner");
    let id = Identity::generate();
    id.write_to_dir(&dir).unwrap();
    let adir = agent_dir(base.path());
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();
    let enc = hex32(&id.enc_pubkey_hex());
    let sealed = hex::encode(crypto::seal(&enc, b"box", mock::VULTR_TOKEN.as_bytes()).unwrap());
    let pkg = SecretPackage {
        secrets: std::collections::BTreeMap::from([("box".to_string(), sealed)]),
        targets: std::collections::BTreeMap::from([(
            "box".to_string(),
            TargetMeta {
                kind: "vultr".into(),
                address: format!("http://{vultr_addr}"),
                secret: "box".into(),
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
        "box", // target named `box`, NOT `vultr` — env must be BOX/BOX_URL
        &VultrVpsSpec {
            label: "env-prefix-test".into(),
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
    let _ = vultr_state.instances.lock().is_empty();

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
