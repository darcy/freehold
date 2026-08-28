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
    HetznerVpsSpec, ProxmoxLxcSpec, VultrVpsSpec, bootstrap_hetzner_vps, bootstrap_proxmox_lxc,
    bootstrap_vultr_vps,
};
use freehold_orchestrator::client::McpClient;
use freehold_orchestrator::flows;
use freehold_orchestrator::planebase::MountSpec;
use freehold_orchestrator::{deploy_cp, relay_member};
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
          echo "100 stopped taken"
          echo "102 stopped other"; exit 0;;
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
        relay_url: None,
        relay_pubkey: None,
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
            ("uname", "echo x86_64\n"),
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
            net_ip: None,
            net_gw: None,
            mounts: vec![],
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
async fn proxmox_lxc_bakes_durable_plane_mounts_into_create() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            ("pct", HAPPY_PCT),
            ("uname", "echo x86_64\n"),
            ("pveam", "exit 127\n"),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    // The locked "born on the plane" shape: the relay's two child datasets
    // mount as /var/lib/docker (the daemon data root — named volumes land
    // there) and /srv/buzz-relay (the compose deploy dir + .env).
    let mounts = vec![
        MountSpec {
            source: "rpool/freehold/t-d/relay/docker-root".into(),
            guest_path: "/var/lib/docker".into(),
        },
        MountSpec {
            source: "rpool/freehold/t-d/relay/deploy".into(),
            guest_path: "/srv/buzz-relay".into(),
        },
    ];
    let res = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: Some(101),
            template: None,
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr0".into(),
            net_ip: None,
            net_gw: None,
            mounts,
        },
    )
    .await
    .unwrap();
    assert_eq!(
        res.kind,
        freehold_orchestrator::bootstrap::TargetKind::ProxmoxLxc
    );
    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(
        cmds.contains("--mp0=rpool/freehold/t-d/relay/docker-root,mp=/var/lib/docker"),
        "docker data-root child baked into create: {cmds}"
    );
    assert!(
        cmds.contains("--mp1=rpool/freehold/t-d/relay/deploy,mp=/srv/buzz-relay"),
        "compose deploy-dir child baked into create: {cmds}"
    );
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
if [ "$1" = "config" ]; then echo "Configuration file 'lxc/$2.conf' does not exist" >&2; exit 1; fi
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
            net_ip: None,
            net_gw: None,
            mounts: vec![],
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
async fn proxmox_lxc_reuses_existing_matching_container() {
    // A re-run (installer retry, partial bring-up) hits a vmid that ALREADY
    // exists with OUR hostname: the driver must reuse it — no `pct create`,
    // still started + verified + docker-ensured. A foreign container with
    // the same vmid is refused loudly instead.
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            (
                "pct",
                r#"
case "$1" in
  config) echo "hostname: boom"; echo "memory: 4096"; exit 0;;
  status) echo "state: running"; exit 0;;
  start)  echo "204"; exit 0;;
  exec)
    case "$4" in
      docker) echo "Server Version: 27.0"; exit 0;;
      *)
        case "$6" in
          *"compose version"*) echo "Docker Compose version v2.24.4"; exit 0;;
          *) echo "boom"; echo "Linux"; echo "root"; exit 0;;
        esac
        ;;
    esac
    ;;
  *) echo "unknown pct $*" >&2; exit 2;;
esac
"#,
            ),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let res = bootstrap_proxmox_lxc(
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
            net_ip: None,
            net_gw: None,
            mounts: vec![],
        },
    )
    .await
    .expect("reuse must succeed");

    assert_eq!(res.id, "102");
    let log = std::fs::read_to_string(&log).unwrap();
    assert!(
        !log.contains("pct create 102"),
        "an existing matching container is NOT recreated: {log}"
    );
    assert!(
        log.contains("pct config 102"),
        "probe ran before deciding: {log}"
    );
    assert!(log.contains("pct exec 102"), "guest still verified: {log}");
    assert!(
        log.contains("download.docker.com"),
        "docker ensure still runs on reuse: {log}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_refuses_foreign_container_on_vmid() {
    // Same vmid, DIFFERENT hostname: never touch it.
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            (
                "pct",
                r#"
if [ "$1" = "config" ]; then echo "hostname: someone-elses-box"; exit 0; fi
if [ "$1" = "create" ]; then echo "SHOULD NOT HAPPEN"; exit 0; fi
exit 0
"#,
            ),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

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
            net_ip: None,
            net_gw: None,
            mounts: vec![],
        },
    )
    .await
    .expect_err("a foreign container on the vmid must be refused");

    let msg = format!("{err}");
    assert!(
        msg.contains("someone-elses-box"),
        "names the conflict: {msg}"
    );
    assert!(msg.contains("destroy it"), "actionable: {msg}");

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
            ("uname", "echo x86_64\n"),
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
            net_ip: None,
            net_gw: None,
            mounts: vec![],
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
        &[
            ("pvesm", HAPPY_PVESM),
            ("pct", HAPPY_PCT),
            ("uname", "echo x86_64\n"),
            ("pvesh", "echo 101; exit 0\n"),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let res = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: None, // cluster nextid says 101
            template: None,
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr0".into(),
            net_ip: None,
            net_gw: None,
            mounts: vec![],
        },
    )
    .await
    .unwrap();

    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(cmds.contains("pct list"), "duplicate-name check: {cmds}");
    assert!(
        cmds.contains("pvesh get /cluster/nextid"),
        "cluster-wide nextid used (shared vmid namespace): {cmds}"
    );
    assert!(cmds.contains("pct create 101"), "nextid picked: {cmds}");
    assert!(
        !cmds.contains("pct create 100") && !cmds.contains("pct create 102"),
        "an in-use vmid must never be reused: {cmds}"
    );
    assert_eq!(res.id, "101", "result reports the picked vmid: {res:?}");

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_refuses_duplicate_hostname() {
    let base = tempfile::tempdir().unwrap();
    // pct list shows a container ALREADY named relay-box: a second bootstrap
    // with the same --name must be refused, never duplicated.
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            (
                "pct",
                r#"
if [ "$1" = "list" ]; then
  echo "VMID Status Lock Name"
  echo "100 running relay-box"
  exit 0
fi
case "$1" in
  create) echo "204"; exit 0;;
  start)  echo "204"; exit 0;;
  exec)   echo "testhost-101"; echo "Linux"; echo "root"; exit 0;;
  *)      echo "unknown pct $*" >&2; exit 2;;
esac
"#,
            ),
            ("uname", "echo x86_64\n"),
            ("pvesh", "echo 100; exit 0\n"),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let err = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "relay-box".into(),
            vmid: None,
            template: None,
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr0".into(),
            net_ip: None,
            net_gw: None,
            mounts: vec![],
        },
    )
    .await
    .expect_err("an existing same-name container must refuse re-creation");

    let msg = format!("{err}");
    assert!(
        msg.contains("already exists") && msg.contains("relay-box"),
        "duplicate-name error: {msg}"
    );
    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(
        !cmds.contains("pct create"),
        "must not create when the name is taken: {cmds}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_docker_daemon_failure_is_reported() {
    let base = tempfile::tempdir().unwrap();
    // docker info repeatedly fails: the driver must try the fuse-overlayfs
    // fallback, and AFTER it still surface the daemon error with a hint.
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            ("pct", PCT_EXEC_DOCKER_FAIL),
            ("uname", "echo x86_64\n"),
        ],
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
            net_ip: None,
            net_gw: None,
            mounts: vec![],
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
            net_ip: None,
            net_gw: None,
            mounts: vec![],
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
        relay_url: None,
        relay_pubkey: None,
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
        relay_url: None,
        relay_pubkey: None,
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

#[tokio::test(flavor = "multi_thread")]
// sftp-subsystem fixture gap (testkit sshd handles exec only).
#[ignore]
async fn deploy_cp_ships_binary_starts_and_reads_fresh_pubkey() {
    let base = tempfile::tempdir().unwrap();
    // A tiny fake "control-plane": `serve` records a marker and keeps running
    // (so the kill -0 check passes); `identity` prints a pubkey. The driver
    // must NEVER ship a keypair — nothing writes console/identity.json.
    let marker = base.path().join("started.marker");
    let fake_bin = base.path().join("control-plane");
    std::fs::write(
        &fake_bin,
        format!(
            "#!/bin/sh\ncase \"$1\" in\n  identity) echo 1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff; exit 0;;\n  serve) echo started >> '{}'; sleep 300;;\n  *) exit 1;;\nesac\n",
            marker.display()
        ),
    )
    .unwrap();

    // curl is stubbed to answer ok so the /healthz poll succeeds regardless.
    let (bin, _ba) = plant_bin(
        &base.path().join("cp.log"),
        &[("curl", "echo ok; exit 0\n")],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let sd = base.path().join("deploy/cp").display().to_string();
    let bd = base.path().join("deploy/bin").display().to_string();
    let res = deploy_cp::deploy_cp(
        &client,
        "proxmox-box",
        &deploy_cp::DeployCpSpec {
            state_dir: sd.clone(),
            bin_dir: bd.clone(),
            bind_addr: "127.0.0.1:8080".into(),
            binary_path: fake_bin.clone(),
            relay_url: "http://relay-box:3000".into(),
            relay_pubkey: None,
            relay_host_ip: None,
            admin_pubkeys: vec![],
            lxc: None,
            public_origin: None,
            runner_binary: None,
            runner_package: None,
        },
    )
    .await
    .expect("deploy-cp must succeed");

    assert!(res.detail.contains("OPERATE mode"), "{res:?}");
    assert_eq!(
        res.pubkey, "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff",
        "fresh box identity pubkey read back"
    );
    assert!(
        res.detail.contains("relay-member --pubkey"),
        "the add-member action is spelled out: {res:?}"
    );
    // Shipped binary == local bytes; NO keypair was shipped (the box's
    // identity is generated by its own serve, never transferred).
    let shipped = std::fs::read(base.path().join("deploy/bin/control-plane")).unwrap();
    assert_eq!(
        shipped,
        std::fs::read(&fake_bin).unwrap(),
        "binary bytes exact"
    );
    assert!(
        !base.path().join("deploy/cp/console/identity.json").exists(),
        "no identity.json may be shipped to the box"
    );
    // The base64 staging file is removed; the shipped script actually ran.
    assert!(
        !base.path().join("deploy/bin/control-plane.b64").exists(),
        "staging .b64 removed"
    );
    assert!(marker.exists(), "the shipped binary executed on the target");

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn deploy_cp_refuses_non_loopback_bind() {
    let base = tempfile::tempdir().unwrap();
    let fake_bin = base.path().join("c");
    std::fs::write(&fake_bin, b"#!/bin/sh\nexit 0").unwrap();
    let (bin, _ba) = plant_bin(&base.path().join("cp.log"), &[("curl", "echo ok\n")]);
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let err = deploy_cp::deploy_cp(
        &client,
        "proxmox-box",
        &deploy_cp::DeployCpSpec {
            state_dir: base.path().join("deploy/cp").display().to_string(),
            bin_dir: base.path().join("deploy/bin").display().to_string(),
            bind_addr: "0.0.0.0:8080".into(),
            binary_path: fake_bin,
            relay_url: "http://relay-box:3000".into(),
            relay_pubkey: None,
            relay_host_ip: None,
            admin_pubkeys: vec![],
            lxc: None,
            public_origin: None,
            runner_binary: None,
            runner_package: None,
        },
    )
    .await
    .expect_err("non-loopback bind must be refused BEFORE shipping");

    let msg = format!("{err}");
    assert!(
        msg.contains("loopback-only") && msg.contains("SSH tunnel"),
        "C3 refusal message: {msg}"
    );
    let cmds = std::fs::read_to_string(&log).unwrap_or_default();
    assert!(
        !cmds.contains("mkdir"),
        "no commands may run when the bind is refused: {cmds}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
// Requires the testkit sshd to serve the sftp subsystem (the ship
// now streams via sftp; the fixture handles exec only). Follow-up:
// implement russh_sftp::server in testkit.
#[ignore]
async fn deploy_cp_with_admin_relaxes_loopback_guard() {
    // C3.5: with a console admin whitelist configured, the deploy MAY use a
    // non-loopback bind — authn replaces network unreachability.
    let base = tempfile::tempdir().unwrap();
    let marker = base.path().join("started.marker");
    let fake_bin = base.path().join("control-plane");
    std::fs::write(
        &fake_bin,
        format!(
            "#!/bin/sh\ncase \"$1\" in\n  identity) echo 1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff; exit 0;;\n  serve) echo started >> '{}'; sleep 300;;\n  *) exit 1;;\nesac\n",
            marker.display()
        ),
    )
    .unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("cp.log"),
        &[("curl", "echo ok; exit 0\n")],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let sd = base.path().join("deploy/cp").display().to_string();
    let bd = base.path().join("deploy/bin").display().to_string();
    let res = deploy_cp::deploy_cp(
        &client,
        "proxmox-box",
        &deploy_cp::DeployCpSpec {
            state_dir: sd.clone(),
            bin_dir: bd.clone(),
            bind_addr: "0.0.0.0:8080".into(),
            binary_path: fake_bin.clone(),
            relay_url: "http://relay-box:3000".into(),
            relay_pubkey: None,
            relay_host_ip: None,
            admin_pubkeys: vec![
                "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".to_string(),
            ],
            lxc: None,
            public_origin: None,
            runner_binary: None,
            runner_package: None,
        },
    )
    .await
    .expect("admin-configured deploy must allow the non-loopback bind");
    assert!(res.detail.contains("OPERATE mode"), "{res:?}");
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn relay_member_add_builds_buzz_admin_command() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[(
            "pct",
            r#"
case "$1" in
  exec) echo "member 5e3b2f0d60e464b2908d2ab17367db0a35eb0c8997ac60b27d968582c7384bb4 added; roster published (kind 13534)"; exit 0;;
  *)    echo "unknown pct $*" >&2; exit 2;;
esac
"#,
        )],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let res = relay_member::relay_member_add(
        &client,
        "proxmox-box",
        &relay_member::RelayMemberAddSpec {
            pubkey: "5e3b2f0d60e464b2908d2ab17367db0a35eb0c8997ac60b27d968582c7384bb4".into(),
            role: Some("admin".into()),
            lxc: Some(100),
            compose_dir: relay_member::DEFAULT_BUZZ_COMPOSE_DIR.into(),
        },
    )
    .await
    .expect("add-member must succeed");

    assert!(
        res.detail.contains("relay member") && res.detail.contains("added"),
        "{res:?}"
    );
    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(
        cmds.contains("pct exec 100") && cmds.contains("buzz-admin add-member"),
        "wrapped into the relay LXC: {cmds}"
    );
    assert!(
        cmds.contains("--pubkey 5e3b2f0d60e464b2908d2ab17367db0a35eb0c8997ac60b27d968582c7384bb4")
            && cmds.contains("--role admin"),
        "pubkey + role interpolated: {cmds}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn relay_member_add_rejects_bad_pubkey_before_exec() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(&base.path().join("pct.log"), &[("pct", "exit 0\n")]);
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let err = relay_member::relay_member_add(
        &client,
        "proxmox-box",
        &relay_member::RelayMemberAddSpec {
            pubkey: "xyz".into(),
            role: None,
            lxc: Some(100),
            compose_dir: relay_member::DEFAULT_BUZZ_COMPOSE_DIR.into(),
        },
    )
    .await
    .expect_err("a non-hex short pubkey must be refused");

    let msg = format!("{err}");
    assert!(
        msg.contains("64-hex Nostr pubkey"),
        "validation message: {msg}"
    );
    let cmds = std::fs::read_to_string(&log).unwrap_or_default();
    assert!(
        !cmds.contains("buzz-admin"),
        "no add without validation: {cmds}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
// sftp-subsystem fixture gap (testkit sshd handles exec only).
#[ignore]
async fn deploy_cp_lxc_mode_runs_every_remote_command_in_the_guest() {
    // The CP lives in its OWN LXC (different guest than the relay's by
    // default): every remote command must route through `pct exec <id> --`.
    // The pct stub here EXECUTES the payload (guest == host in the fixture)
    // so the full deploy succeeds; cmds.log proves the wrap happened.
    let base = tempfile::tempdir().unwrap();
    let marker = base.path().join("started.marker");
    let fake_bin = base.path().join("control-plane");
    std::fs::write(
        &fake_bin,
        format!(
            "#!/bin/sh\ncase \"$1\" in\n  identity) echo 1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff; exit 0;;\n  serve) echo started >> '{}'; sleep 300;;\n  *) exit 1;;\nesac\n",
            marker.display()
        ),
    )
    .unwrap();
    let pct_marker = base.path().join("pct.saw");
    let exec_thru_pct = format!(
        "touch '{}' && if [ \"$1\" = \"exec\" ]; then shift 5; sh -c \"$*\"; exit $?; fi; echo \"unknown pct $*\" >&2; exit 2\n",
        pct_marker.display()
    );
    let curl_ok = "echo ok; exit 0\n";
    let (bin, _ba) = plant_bin(
        &base.path().join("cp.log"),
        &[("pct", exec_thru_pct.as_str()), ("curl", curl_ok)],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await; // Decisive: the runner MUST reach the pct stub through the wrapped form.
    let probe = client
        .exec(
            "proxmox-box",
            "pct exec 100 -- sh -c 'echo PCTRAN'",
            &["proxmox-box"],
            10,
        )
        .map(|o| o.stdout);
    assert!(
        matches!(probe.as_deref(), Ok(s) if s.contains("PCTRAN")),
        "pct stub reachable: {probe:?}"
    );

    let sd = base.path().join("deploy/cp").display().to_string();
    let bd = base.path().join("deploy/bin").display().to_string();
    let res = deploy_cp::deploy_cp(
        &client,
        "proxmox-box",
        &deploy_cp::DeployCpSpec {
            lxc: Some(100),
            public_origin: None,
            runner_binary: None,
            runner_package: None,
            state_dir: sd.clone(),
            bin_dir: bd.clone(),
            bind_addr: "127.0.0.1:8080".into(),
            binary_path: fake_bin.clone(),
            relay_url: "http://relay-box:3000".into(),
            relay_pubkey: None,
            relay_host_ip: None,
            admin_pubkeys: vec![],
        },
    )
    .await
    .expect("lxc-mode deploy must succeed through pct exec");
    assert!(res.detail.contains("OPERATE mode"), "{res:?}");
    assert_eq!(
        res.pubkey, "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff",
        "identity read back from the guest"
    );
    // Every remote command was wrapped: the log shows pct exec invocations,
    // and the binary/state landed inside the fake guest (== host paths).
    assert!(
        base.path().join("deploy/bin/control-plane").exists(),
        "binary shipped into the guest"
    );
    assert!(
        base.path().join("deploy/cp/console").exists(),
        "state dir in the guest"
    );
    let cmds = std::fs::read_to_string(base.path().join("cp.log")).unwrap_or_default();
    let wrapped = cmds.lines().filter(|l| l.contains("/pct exec 100")).count();
    assert!(
        wrapped >= 6,
        "expected >=6 pct exec wraps, saw {wrapped}:\n{cmds}"
    );
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
// Requires the testkit sshd to serve the sftp subsystem (the ship
// now streams via sftp; the fixture handles exec only). Follow-up:
// implement russh_sftp::server in testkit.
#[ignore]
async fn deploy_cp_co_locates_runner_when_asked() {
    use freehold_core::identity::Identity;
    let base = tempfile::tempdir().unwrap();
    // fake control-plane handles the NEW subcommands (adopt/grant) the
    // co-location steps run in-guest
    let fake_bin = base.path().join("control-plane");
    std::fs::write(
        &fake_bin,
        "#!/bin/sh\ncase \"$1\" in\n  identity) echo 1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff; exit 0;;\n  serve) echo started >> '%s'; sleep 300;;\n  adopt) echo ADOPTED; exit 0;;\n  grant) echo GRANTED; exit 0;;\n  *) exit 1;;\nesac\n"
            .replace("%s", &base.path().join("started.marker").display().to_string()),
    )
    .unwrap();
    let exec_thru_pct = "if [ \"$1\" = \"exec\" ]; then shift 5; sh -c \"$*\"; exit $?; fi; echo \"unknown pct $*\" >&2; exit 2\n";
    let curl_ok = "echo ok; exit 0\n";
    let systemctl_ok = "echo active; exit 0\n";
    let systemdrun_ok = "echo \"Running as unit: freehold-runner.service\"; exit 0\n";
    let (bin, _ba) = plant_bin(
        &base.path().join("cmds.log"),
        &[
            ("pct", exec_thru_pct),
            ("curl", curl_ok),
            ("systemctl", systemctl_ok),
            ("systemd-run", systemdrun_ok),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    // a REAL runner package for the co-located runner
    let pkg_dir = base.path().join("my-runner-pkg");
    std::fs::create_dir_all(&pkg_dir).unwrap();
    let rid = Identity::generate();
    rid.write_to_dir(&pkg_dir).unwrap();
    let enc = hex::decode(rid.enc_pubkey_hex()).unwrap();
    let mut enc32 = [0u8; 32];
    enc32.copy_from_slice(&enc);
    let sealed = hex::encode(freehold_core::crypto::seal(&enc32, b"t", b"cred").unwrap());
    let pkg = freehold_core::secrets::SecretPackage {
        secrets: std::collections::BTreeMap::from([("t".to_string(), sealed)]),
        targets: std::collections::BTreeMap::from([(
            "t".to_string(),
            freehold_core::secrets::TargetMeta {
                kind: "ssh".into(),
                address: "root@192.168.30.224".into(),
                secret: "t".into(),
            },
        )]),
        grants: vec![],
    };
    pkg.write_to_dir(&pkg_dir).unwrap();
    let fake_runner = base.path().join("freehold-runner");
    std::fs::write(&fake_runner, "#!/bin/sh\nexit 0\n").unwrap();

    let sd = base.path().join("deploy/cp").display().to_string();
    let bd = base.path().join("deploy/bin").display().to_string();
    let res = deploy_cp::deploy_cp(
        &client,
        "proxmox-box",
        &deploy_cp::DeployCpSpec {
            lxc: Some(100),
            state_dir: sd.clone(),
            bin_dir: bd.clone(),
            bind_addr: "127.0.0.1:8080".into(),
            binary_path: fake_bin.clone(),
            relay_url: "http://relay-box:3000".into(),
            relay_pubkey: None,
            relay_host_ip: None,
            admin_pubkeys: vec![
                "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".to_string(),
            ],
            public_origin: None,
            runner_binary: Some(fake_runner.clone()),
            runner_package: Some(pkg_dir.clone()),
        },
    )
    .await
    .expect("co-located runner deploy must succeed");
    assert!(res.detail.contains("OPERATE mode"), "{res:?}");
    // The runner really landed in the guest paths + the unit + adopt + grant ran
    assert!(base.path().join("deploy/bin/freehold-runner").exists());
    assert!(
        base.path()
            .join("deploy/cp/runner/my-runner-pkg/identity.json")
            .exists()
    );
    let cmds = std::fs::read_to_string(base.path().join("cmds.log")).unwrap_or_default();
    assert!(cmds.contains("systemd-run"), "runner unit start: {cmds}");
    assert!(cmds.contains("adopt"), "adopt step ran: {cmds}");
    assert!(cmds.contains("grant"), "self-grant step ran: {cmds}");
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn hetzner_vps_bootstrap_creates_polls_destroys() {
    let base = tempfile::tempdir().unwrap();
    let htz_state = std::sync::Arc::new(mock::HetznerState::default());
    let htz_addr = mock::spawn_http(mock::hetzner_router(htz_state.clone())).await;

    let dir = base.path().join("runner");
    let id = Identity::generate();
    id.write_to_dir(&dir).unwrap();
    let adir = agent_dir(base.path());
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();
    let enc = hex32(&id.enc_pubkey_hex());
    let sealed =
        hex::encode(crypto::seal(&enc, b"hetzner", mock::HETZNER_TOKEN.as_bytes()).unwrap());
    let pkg = SecretPackage {
        secrets: std::collections::BTreeMap::from([("hetzner".to_string(), sealed)]),
        targets: std::collections::BTreeMap::from([(
            "hetzner".to_string(),
            TargetMeta {
                kind: "hetzner".into(),
                address: format!("http://{htz_addr}"),
                secret: "hetzner".into(),
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
        relay_url: None,
        relay_pubkey: None,
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let client = client(&adir, &format!("http://{addr}/mcp"), &runner_pubkey);

    let res = bootstrap_hetzner_vps(
        &client,
        "hetzner",
        &HetznerVpsSpec {
            label: "bootstrap-test".into(),
            location: "fsn1".into(),
            server_type: "cx22".into(),
            image: "ubuntu-22.04".into(),
            destroy_after: true,
        },
    )
    .await
    .unwrap();

    assert!(res.detail.contains("active"), "{res:?}");
    assert!(res.detail.contains("destroyed"), "{res:?}");
    assert!(
        htz_state.servers.lock().is_empty(),
        "destroy-after must remove the server from the mock"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn hetzner_vps_falls_back_to_available_server_type_in_location() {
    let base = tempfile::tempdir().unwrap();
    let htz_state = std::sync::Arc::new(mock::HetznerState::default());
    let htz_addr = mock::spawn_http(mock::hetzner_router(htz_state.clone())).await;
    let dir = base.path().join("runner");
    let id = Identity::generate();
    id.write_to_dir(&dir).unwrap();
    let adir = agent_dir(base.path());
    let agent_pk = flows::agent_auth(&adir).unwrap().pubkey.clone();
    let enc = hex32(&id.enc_pubkey_hex());
    let sealed =
        hex::encode(crypto::seal(&enc, b"hetzner", mock::HETZNER_TOKEN.as_bytes()).unwrap());
    let pkg = SecretPackage {
        secrets: std::collections::BTreeMap::from([("hetzner".to_string(), sealed)]),
        targets: std::collections::BTreeMap::from([(
            "hetzner".to_string(),
            TargetMeta {
                kind: "hetzner".into(),
                address: format!("http://{htz_addr}"),
                secret: "hetzner".into(),
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
        relay_url: None,
        relay_pubkey: None,
    };
    let (addr, server) = mcp::serve("127.0.0.1:0", ctx).await.unwrap();
    let client = client(&adir, &format!("http://{addr}/mcp"), &runner_pubkey);

    // cax11 is asked but unavailable in fsn1 (the mock's types) — the driver
    // must discover cpx11 and create with it.
    let res = bootstrap_hetzner_vps(
        &client,
        "hetzner",
        &HetznerVpsSpec {
            label: "fallback-test".into(),
            location: "fsn1".into(),
            server_type: "cax11".into(),
            image: "ubuntu-22.04".into(),
            destroy_after: true,
        },
    )
    .await
    .unwrap();
    assert!(res.detail.contains("active"), "{res:?}");
    assert!(res.detail.contains("destroyed"), "{res:?}");
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn proxmox_lxc_static_net_for_cloud_pve() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("pct.log"),
        &[
            ("pvesm", HAPPY_PVESM),
            ("pct", HAPPY_PCT),
            ("uname", "echo x86_64\n"),
        ],
    );
    let (log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let _res = bootstrap_proxmox_lxc(
        &client,
        "proxmox-box",
        &ProxmoxLxcSpec {
            hostname: "testhost-101".into(),
            vmid: Some(101),
            template: None,
            storage: "local-lvm".into(),
            rootfs_gb: 16,
            memory_mb: 2048,
            bridge: "vmbr1".into(),
            net_ip: Some("10.10.0.6/24".into()),
            net_gw: Some("10.10.0.1".into()),
            mounts: vec![],
        },
    )
    .await
    .unwrap();
    let cmds = std::fs::read_to_string(&log).unwrap();
    assert!(
        cmds.contains("ip=10.10.0.6/24,gw=10.10.0.1"),
        "static net0 on the private bridge: {cmds}"
    );
    assert!(
        !cmds.contains("bridge=vmbr1,ip=dhcp"),
        "no dhcp on the private bridge: {cmds}"
    );
    server.abort();
}

// ---------------------------------------------------------------- Phase 0.12
// Durable-volume-plane hermetic tests: the storage resolution consent gate,
// the reuse-vs-bail ordering, and runner-driven dataset create/destroy —
// against FAKE zpool/vgs/zfs bins through the real sshd + runner (the same
// fixture pattern as the proxmox driver tests). No real storage anywhere.

/// A host with NO existing storage backend: `zpool list` and `vgs` both
/// answer empty. Resolution must bail without consent.
const NO_STORAGE_ZPOOL: &str = "echo ''; exit 1\n";
const NO_STORAGE_VGS: &str = "echo ''; exit 1\n";

/// A host with an existing zpool.
const ZPOOL_HOST: &str = "echo 'rpool'; exit 0\n";

/// A host with an existing LVM VG (no thin pool yet).
const LVM_HOST: &str = "echo 'vg'; exit 0\n";

#[tokio::test(flavor = "multi_thread")]
async fn storage_resolve_reuses_existing_zpool() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("storage.log"),
        &[("zpool", ZPOOL_HOST), ("vgs", NO_STORAGE_VGS)],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let action = freehold_orchestrator::drive::resolve_proxmox(&client, "proxmox-box", false, None)
        .await
        .unwrap();
    assert_eq!(
        action,
        freehold_orchestrator::planebase::ResolveAction::Reuse(
            freehold_orchestrator::planebase::ExistingBackend::Zfs,
            "rpool".into(),
        )
    );
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn storage_resolve_reuses_existing_lvm_when_no_zpool() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("storage.log"),
        &[("zpool", NO_STORAGE_ZPOOL), ("vgs", LVM_HOST)],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let action = freehold_orchestrator::drive::resolve_proxmox(&client, "proxmox-box", false, None)
        .await
        .unwrap();
    assert_eq!(
        action,
        freehold_orchestrator::planebase::ResolveAction::Reuse(
            freehold_orchestrator::planebase::ExistingBackend::LvmThin,
            "vg".into(),
        )
    );
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn storage_resolve_bails_without_consent_when_no_backend() {
    let base = tempfile::tempdir().unwrap();
    let (bin, _ba) = plant_bin(
        &base.path().join("storage.log"),
        &[("zpool", NO_STORAGE_ZPOOL), ("vgs", NO_STORAGE_VGS)],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    let action = freehold_orchestrator::drive::resolve_proxmox(&client, "proxmox-box", false, None)
        .await
        .unwrap();
    match action {
        freehold_orchestrator::planebase::ResolveAction::Bail(m) => {
            assert!(
                m.contains("--confirm-storage"),
                "actionable bail names the consent fix: {m}"
            );
        }
        other => panic!("expected Bail, got {other:?}"),
    }
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn storage_resolve_with_consent_creates_when_no_backend() {
    let base = tempfile::tempdir().unwrap();
    // NOTE: resolution order is zpool-list then vg-list; a VG present ->
    // Reuse(LVM). Consent with NO backend at all returns Create(Zfs):
    let (bin, _ba) = plant_bin(
        &base.path().join("storage.log"),
        &[("zpool", NO_STORAGE_ZPOOL), ("vgs", NO_STORAGE_VGS)],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let action = freehold_orchestrator::drive::resolve_proxmox(&client, "proxmox-box", true, None)
        .await
        .unwrap();
    assert_eq!(
        action,
        freehold_orchestrator::planebase::ResolveAction::Create(
            freehold_orchestrator::planebase::Backend::LvmThin,
            "freehold".into(),
        ),
        "consent + no backend + no device -> create LVM-thin (the locked \
         ZFS->LVM-thin->bail order's fallback), never a silent tier"
    );
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn ensure_dataset_is_idempotent_and_chown_runs() {
    use freehold_orchestrator::drive::{chown_guest_uid, ensure_dataset};
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("zfs.log");
    // Fake `zfs`: `list <ds>` ALWAYS answers present (exit 0, no output
    // needed for the presence probe). Any `create` would be logged.
    let (bin, _ba) = plant_bin(
        &logp,
        &[
            (
                "zfs",
                "echo 'rpool/freehold/t-d/relay'; exit 0
",
            ),
            (
                "chown",
                "echo done; exit 0
",
            ),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let ds = "rpool/freehold/t-d/relay";
    // idempotent: the fake reports present, so NO `zfs create` runs.
    ensure_dataset(&client, "proxmox-box", ds).await.unwrap();
    ensure_dataset(&client, "proxmox-box", ds).await.unwrap();
    let cmds = std::fs::read_to_string(&logp).unwrap_or_default();
    assert!(
        !cmds.contains("create"),
        "existing dataset must be REUSED, no zfs create: {cmds}"
    );
    // chown passes a shifted uid through the runner.
    chown_guest_uid(&client, "proxmox-box", "/srv/data/k8s-volumes", 100000)
        .await
        .unwrap();
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn destroy_tenant_dataset_destroys_relay_parent_recursively() {
    use freehold_orchestrator::drive::destroy_tenant_dataset;
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("zfs.log");
    // The fake `zfs` logs its full argv to $ZFS_LOG on destroy.
    let (bin, _ba) = plant_bin(
        &logp,
        &[(
            "zfs",
            // plant_bin already logs every invocation ("$0 $*") to the test
            // log, so this body just needs to ANSWER: act as a dataset that
            // exists (list exits 0) and allow destroy (any op exits 0).
            "exit 0",
        )],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    destroy_tenant_dataset(
        &client,
        "proxmox-box",
        "rpool",
        "freehold-test.darcydev.net",
        freehold_orchestrator::planebase::Tenant::Relay,
    )
    .await
    .unwrap();
    // The destroy hits the RELAY PARENT (rpool/freehold/.../relay) with -r,
    // so both child datasets (docker-root + deploy) go with it — the locked
    // single-parent-destroy unit.
    let cmds = std::fs::read_to_string(&logp).expect("zfs destroy was invoked");
    assert!(
        cmds.contains("destroy -r rpool/freehold/freehold-test-darcydev-net/relay"),
        "recursive parent-subtree destroy: {cmds}"
    );
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn destroy_tenant_dataset_absent_is_a_noop_not_an_error() {
    use freehold_orchestrator::drive::destroy_tenant_dataset;
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("zfs.log");
    // Fake `zfs`: a dataset that does NOT exist (list exits 1). The destroy
    // must NOT call `zfs destroy` (absent is a no-op, not an error) and must
    // return Ok(false) so the caller can distinguish it from a real destroy.
    let (bin, _ba) = plant_bin(
        &logp,
        &[("zfs", "if [ \"$1\" = \"list\" ]; then exit 1; fi; exit 0\n")],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let destroyed = destroy_tenant_dataset(
        &client,
        "proxmox-box",
        "rpool",
        "freehold-test.darcydev.net",
        freehold_orchestrator::planebase::Tenant::Cp,
    )
    .await
    .unwrap();
    assert!(
        !destroyed,
        "absent dataset is a no-op (Ok(false)), not an error"
    );
    let cmds = std::fs::read_to_string(&logp).unwrap_or_default();
    assert!(
        !cmds.contains("destroy"),
        "absent dataset: no zfs destroy is issued: {cmds}"
    );
    server.abort();
}
#[tokio::test(flavor = "multi_thread")]
async fn resolve_lvm_mounts_creates_thin_lvs_for_stock_pve_host() {
    use freehold_orchestrator::drive::resolve_lvm_mounts;
    use freehold_orchestrator::planebase::Tenant;
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("lvm.log");
    // A VG with NO thin pool yet: `lvs -a` answers nothing, so a fresh
    // `freehold-thin` carve-out is issued; `lvcreate`/`mkfs`/`mount` succeed
    // and log their argv.
    let (bin, _ba) = plant_bin(
        &logp,
        &[
            ("zpool", "echo ''; exit 1\n"),
            ("vgs", "echo 'pve'; exit 0\n"),
            ("lvs", "echo ''; exit 0\n"),
            ("lvcreate", "echo lvcreated; exit 0\n"),
            ("mkfs.ext4", "echo mkfs; exit 0\n"),
            ("mkdir", "exit 0\n"),
            ("mountpoint", "exit 1\n"),
            ("mount", "echo mounted; exit 0\n"),
            ("chown", "exit 0\n"),
            ("blkid", "exit 1\n"),
            ("grep", "exit 1\n"),
            ("tee", "exit 0\n"),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;

    // The CP tenant on the stock-PVE LVM backend: one thin LV + mount.
    let mounts = resolve_lvm_mounts(
        &client,
        "proxmox-box",
        "pve",
        "freehold-test.darcydev.net",
        Tenant::Cp,
    )
    .await
    .unwrap();
    assert_eq!(mounts.len(), 1, "cp gets one mount: {mounts:?}");
    assert!(
        mounts[0].source.starts_with("/freehold/"),
        "host mount source: {:?}",
        mounts[0]
    );
    assert_eq!(mounts[0].guest_path, "/srv/freehold");
    let cmds = std::fs::read_to_string(&logp).unwrap();
    assert!(
        cmds.contains("lvcreate -L 40G -T pve/freehold-thin"),
        "thin pool created once in the pve VG: {cmds}"
    );
    assert!(
        cmds.contains(
            "lvcreate -V 20G -T pve/freehold-thin -n freehold-freehold-test-darcydev-net-cp"
        ),
        "tenant thin LV created: {cmds}"
    );
    assert!(
        cmds.contains("chown -R 100000:100000 /freehold/freehold-test-darcydev-net/cp"),
        "mount chowned to the guest shifted uid: {cmds}"
    );
    assert!(
        cmds.contains("tee -a /etc/fstab"),
        "mount recorded in /etc/fstab so a host reboot restores the plane: {cmds}"
    );
    server.abort();
}
#[tokio::test(flavor = "multi_thread")]
async fn resolve_lvm_mounts_reuses_the_stock_pve_thin_pool() {
    use freehold_orchestrator::drive::resolve_lvm_mounts;
    use freehold_orchestrator::planebase::Tenant;
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("lvm.log");
    // The REAL stock-PVE layout: VG `pve` holds root/swap plus the
    // `pve/data` thin pool (local-lvm). `lvs -a` shows the internal
    // `[data_tdata]`/`[data_tmeta]` segments. The tenant LV must be carved
    // INTO `pve/data` — NEVER a fresh 40G pool on a near-full VG (that is
    // the exact `lvcreate -L 40G -T` failure seen on the live host).
    let (bin, _ba) = plant_bin(
        &logp,
        &[
            ("zpool", "echo ''; exit 1\n"),
            ("vgs", "echo 'pve'; exit 0\n"),
            (
                "lvs",
                "echo '  [data_tdata]'\necho '  [data_tmeta]'\necho '  root'\necho '  swap'\nexit 0\n",
            ),
            ("lvcreate", "echo lvcreated; exit 0\n"),
            ("mkfs.ext4", "echo mkfs; exit 0\n"),
            ("mkdir", "exit 0\n"),
            ("mountpoint", "exit 1\n"),
            ("mount", "echo mounted; exit 0\n"),
            ("chown", "exit 0\n"),
            ("blkid", "exit 1\n"),
            ("grep", "exit 1\n"),
            ("tee", "exit 0\n"),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let mounts = resolve_lvm_mounts(
        &client,
        "proxmox-box",
        "pve",
        "freehold-test.darcydev.net",
        Tenant::Cp,
    )
    .await
    .unwrap();
    assert_eq!(mounts.len(), 1);
    assert_eq!(mounts[0].guest_path, "/srv/freehold");
    let cmds = std::fs::read_to_string(&logp).unwrap();
    assert!(
        cmds.contains("lvcreate -V 20G -T pve/data -n freehold-freehold-test-darcydev-net-cp"),
        "tenant LV carved into the EXISTING pve/data thin pool: {cmds}"
    );
    assert!(
        !cmds.contains("lvcreate -L 40G"),
        "no fresh thin-pool carve-out when one already exists: {cmds}"
    );
    server.abort();
}
#[tokio::test(flavor = "multi_thread")]
async fn resolve_lvm_mounts_is_idempotent_when_lv_exists() {
    use freehold_orchestrator::drive::resolve_lvm_mounts;
    use freehold_orchestrator::planebase::Tenant;
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("lvm.log");
    // LV already exists + already mounted: no lvcreate, no mount.
    let (bin, _ba) = plant_bin(
        &logp,
        &[
            ("zpool", "echo ''; exit 1\n"),
            ("vgs", "echo 'pve'; exit 0\n"),
            (
                "lvs",
                "echo 'freehold-freehold-test-darcydev-net-cp'; exit 0\n",
            ),
            ("lvcreate", "echo lvcreated; exit 0\n"),
            ("mkfs.ext4", "echo mkfs; exit 0\n"),
            ("mkdir", "exit 0\n"),
            ("mountpoint", "exit 0\n"),
            ("mount", "echo mounted; exit 0\n"),
            ("chown", "exit 0\n"),
            ("blkid", "exit 0\n"),
            ("grep", "exit 0\n"),
            ("tee", "exit 0\n"),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let mounts = resolve_lvm_mounts(
        &client,
        "proxmox-box",
        "pve",
        "freehold-test.darcydev.net",
        Tenant::Cp,
    )
    .await
    .unwrap();
    assert_eq!(mounts.len(), 1);
    assert_eq!(mounts[0].guest_path, "/srv/freehold");
    let cmds = std::fs::read_to_string(&logp).unwrap();
    assert!(
        !cmds.contains("lvcreate"),
        "LV already exists: no lvcreate is issued: {cmds}"
    );
    assert!(!cmds.contains("mkfs"), "no mkfs: {cmds}");
    assert!(
        !cmds.lines().any(|l| l.starts_with("mount ")),
        "already mounted: no mount: {cmds}"
    );
    assert!(
        !cmds.contains("tee"),
        "fstab entry already present: no fstab re-add: {cmds}"
    );
    server.abort();
}
#[tokio::test(flavor = "multi_thread")]
async fn destroy_lvm_tenant_unmounts_strips_fstab_then_removes_lvs() {
    use freehold_orchestrator::drive::destroy_lvm_tenant;
    use freehold_orchestrator::planebase::Tenant;
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("lvm.log");
    // Relay: both child LVs exist. Destroy must umount + strip the fstab
    // line BEFORE each lvremove — `lvremove -f` skips the prompt, not the
    // open-count check, so a still-mounted LV would be refused and the
    // teardown would bail with the LXCs already gone.
    let (bin, _ba) = plant_bin(
        &logp,
        &[
            (
                "lvs",
                "echo 'freehold-t-d-relay-docker-root'\necho 'freehold-t-d-relay-deploy'\nexit 0\n",
            ),
            ("umount", "echo unmounted; exit 0\n"),
            ("sed", "exit 0\n"),
            ("lvremove", "echo removed; exit 0\n"),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let destroyed = destroy_lvm_tenant(&client, "proxmox-box", "pve", "t.d", Tenant::Relay)
        .await
        .unwrap();
    assert!(destroyed, "both child LVs existed => destroyed");
    let cmds = std::fs::read_to_string(&logp).unwrap();
    // Order: umount BEFORE lvremove for each child (both children removed).
    let umount_pos = cmds.find("umount /dev/pve/freehold-t-d-relay-docker-root");
    let lvremove_pos = cmds.find("lvremove -f pve/freehold-t-d-relay-docker-root");
    assert!(
        umount_pos.is_some() && lvremove_pos.is_some() && umount_pos < lvremove_pos,
        "umount must precede lvremove: {cmds}"
    );
    assert!(
        cmds.contains("lvremove -f pve/freehold-t-d-relay-deploy"),
        "second child removed: {cmds}"
    );
    assert!(
        cmds.contains("sed -i"),
        "fstab line stripped before removal: {cmds}"
    );
    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn destroy_lvm_tenant_absent_is_a_noop() {
    use freehold_orchestrator::drive::destroy_lvm_tenant;
    use freehold_orchestrator::planebase::Tenant;
    let base = tempfile::tempdir().unwrap();
    let logp = base.path().join("lvm.log");
    // No matching LVs: absent => Ok(false), no lvremove.
    let (bin, _ba) = plant_bin(
        &logp,
        &[
            ("lvs", "echo 'root'; exit 0\n"),
            ("lvremove", "echo removed; exit 0\n"),
        ],
    );
    let (_log, _rd, client, server) = proxmox_fixture(base.path(), &bin).await;
    let destroyed = destroy_lvm_tenant(&client, "proxmox-box", "pve", "t.d", Tenant::Cp)
        .await
        .unwrap();
    assert!(!destroyed, "absent LV is a no-op, not an error");
    let cmds = std::fs::read_to_string(&logp).unwrap();
    assert!(!cmds.contains("lvremove"), "absent: no lvremove: {cmds}");
    server.abort();
}
