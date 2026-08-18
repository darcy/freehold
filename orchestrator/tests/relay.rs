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
    // Every stub invocation appends `$0 $*` to <bin>/../cmds.log — the
    // lxc-wrap test counts wrapped commands from it.
    let log = dir.path().join("cmds.log");
    for (name, body) in scripts {
        let p = bin.join(name);
        let mut f = std::fs::File::create(&p).unwrap();
        writeln!(f, "#!/bin/sh").unwrap();
        writeln!(f, "echo \"$0 $*\" >> '{}'", log.display()).unwrap();
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
        relay_url: None,
        grant_author: None,
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
// (the legacy `cp` stub was removed — a REAL `cp` copies .env.example so the
//  CHANGE_ME sweep + leftover guard execute against real content)
#[tokio::test(flavor = "multi_thread")]
async fn relay_deploy_gates_on_docker_and_verifies_liveness() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("relay");
    let dir_s = dir.display().to_string();
    // The tar stub materializes a REAL .env.example with CHANGE_ME values so
    // the install's per-key sweep + leftover guard actually execute; `cp` is
    // NOT stubbed, so a real `cp` copies it into .env first.
    let tar_stub = format!(
        "mkdir -p {dir}/deploy/compose && printf '#!/bin/sh\\necho RUNSH-OK\\nexit 0\\n' \
         > {dir}/deploy/compose/run.sh && chmod +x {dir}/deploy/compose/run.sh && \
         printf 'BUZZ_RELAY_PRIVATE_KEY=CHANGE_ME_64_HEX\\nBUZZ_GIT_HOOK_HMAC_SECRET=CHANGE_ME_64_HEX\\n\
         POSTGRES_PASSWORD=CHANGE_ME_PW\\nREDIS_PASSWORD=CHANGE_ME_PW\\nBUZZ_S3_ACCESS_KEY=CHANGE_ME_AK\\n\
         BUZZ_S3_SECRET_KEY=CHANGE_ME_SK\\nBUZZ_DOMAIN=buzz.example.com\\nRELAY_URL=wss://buzz.example.com\\n\
         BUZZ_MEDIA_BASE_URL=https://buzz.example.com/media\\nBUZZ_MEDIA_SERVER_DOMAIN=buzz.example.com\\n' \
         > {dir}/deploy/compose/.env.example && exit 0\n",
        dir = dir_s
    );
    let scripts: Vec<(&str, String)> = vec![
        ("docker", DOCKER_OK.into()),
        ("curl", CURL_OK.into()),
        ("tar", tar_stub),
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
            deploy_dir: dir_s.clone(),
            http_port: 3000,
            buzz_ref: DEFAULT_BUZZ_REF.into(),
            lxc: None,
            owner_pubkey: "072696bde8f03234433ddcc3587464e92a51f5d6906ade6b2aab2e1313010371".into(),
            relay_url: "http://relay-box:3000".into(),
            domain: None,
            operator_pubkey: "1111111111111111111111111111111111111111111111111111111111111111"
                .into(),
        },
    )
    .await
    .unwrap();

    assert_eq!(res.relay_url, "http://relay-box:3000");
    assert!(res.detail.contains("healthy"), "{res:?}");
    assert!(res.detail.contains("ONE scope"), "{res:?}");
    // The sweep REALLY ran against real content: every placeholder replaced,
    // and the generated secrets are 64-hex (od -N32 from /dev/urandom).
    let env = std::fs::read_to_string(dir.join("deploy/compose/.env")).unwrap();
    assert!(!env.contains("=CHANGE_ME"), "placeholders swept: {env}");
    assert!(
        env.lines()
            .filter(|l| {
                l.starts_with("BUZZ_RELAY_PRIVATE_KEY=") || l.starts_with("BUZZ_S3_SECRET_KEY=")
            })
            .all(|l| l.rsplit('=').next().unwrap().len() == 64),
        "urandom 64-hex secrets: {env}"
    );
    // The relay's OWN URL replaced the example.com placeholders (NOT literal
    // CHANGE_ME — the secret sweep would miss them without these seds).
    assert!(
        env.contains("BUZZ_DOMAIN=relay-box")
            && env.contains("RELAY_URL=ws://relay-box:3000")
            && env.contains("BUZZ_MEDIA_BASE_URL=http://relay-box:3000/media"),
        "real relay URL written: {env}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn relay_deploy_lxc_mode_wraps_every_command_in_pct_exec() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("relay");
    let dir_s = dir.display().to_string();
    let tar_stub = format!(
        "mkdir -p {dir}/deploy/compose && printf '#!/bin/sh\\necho RUNSH-OK\\nexit 0\\n' \
         > {dir}/deploy/compose/run.sh && chmod +x {dir}/deploy/compose/run.sh && \
         printf 'BUZZ_RELAY_PRIVATE_KEY=CHANGE_ME_64_HEX\\n' \
         > {dir}/deploy/compose/.env.example && exit 0\n",
        dir = dir_s
    );
    let scripts: Vec<(&str, String)> = vec![
        ("docker", DOCKER_OK.into()),
        ("curl", CURL_OK.into()),
        ("tar", tar_stub),
        (
            "pct",
            "if [ \"$1\" = \"exec\" ]; then echo \"in-guest ok\"; exit 0; fi; exit 2\n".into(),
        ),
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
            deploy_dir: dir_s.clone(),
            http_port: 3000,
            buzz_ref: DEFAULT_BUZZ_REF.into(),
            lxc: Some(100),
            owner_pubkey: "072696bde8f03234433ddcc3587464e92a51f5d6906ade6b2aab2e1313010371".into(),
            relay_url: "http://relay-box:3000".into(),
            domain: None,
            operator_pubkey: "1111111111111111111111111111111111111111111111111111111111111111"
                .into(),
        },
    )
    .await
    .expect("lxc-mode deploy must succeed");

    assert!(res.detail.contains("healthy"), "{res:?}");
    // All SIX exec sites (gate, download, extract, install, liveness, and the
    // INSTALLER invite) went through the pct exec wrapper — the exact
    // quoting that broke live.
    let log = std::fs::read_to_string(bin.parent().unwrap().join("cmds.log")).unwrap();
    let wrapped = log.matches("pct exec 100 -- sh -c").count();
    assert_eq!(wrapped, 6, "every command wrapped into the LXC: {log}");
    assert!(
        log.contains(
            "buzz-admin add-member --pubkey 1111111111111111111111111111111111111111111111111111111111111111"
        )
            && log.contains(&format!("cd {dir_s}/deploy/compose")),
        "the installer invite went to the SAME compose dir as the deploy: {log}"
    );

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
            relay_url: "http://relay-box:3000".into(),
            domain: None,
            operator_pubkey: "1111111111111111111111111111111111111111111111111111111111111111"
                .into(),
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

#[tokio::test(flavor = "multi_thread")]
async fn relay_deploy_fails_when_unswept_placeholder_remains() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("relay");
    let dir_s = dir.display().to_string();
    // A FUTURE bundle could add a new placeholder key the sweep doesn't know
    // about — the leftover guard must trip (fail closed) instead of letting
    // run.sh start with a CHANGE_ME value.
    let tar_stub = format!(
        "mkdir -p {dir}/deploy/compose && printf '#!/bin/sh\\necho RUNSH-OK\\nexit 0\\n' \
         > {dir}/deploy/compose/run.sh && chmod +x {dir}/deploy/compose/run.sh && \
         printf 'BUZZ_RELAY_PRIVATE_KEY=CHANGE_ME_64_HEX\\nSOMETHING_NEW=CHANGE_ME_X\\n' \
         > {dir}/deploy/compose/.env.example && exit 0\n",
        dir = dir_s
    );
    let scripts: Vec<(&str, String)> = vec![
        ("docker", DOCKER_OK.into()),
        ("curl", CURL_OK.into()),
        ("tar", tar_stub),
    ];
    let stubs: Vec<(&str, &str)> = scripts.iter().map(|(n, b)| (*n, b.as_str())).collect();
    let (bin, _ba) = plant_bin(&stubs);
    let (client, _target, server, keep1, _keep2) = fixture(base.path(), &bin).await;
    let _ = (keep1, _keep2);

    let err = deploy_relay(
        &client,
        "relay-box",
        &RelayDeploySpec {
            relay_name: "relay-box".into(),
            deploy_dir: dir_s.clone(),
            http_port: 3000,
            buzz_ref: DEFAULT_BUZZ_REF.into(),
            lxc: None,
            owner_pubkey: "072696bde8f03234433ddcc3587464e92a51f5d6906ade6b2aab2e1313010371".into(),
            relay_url: "http://relay-box:3000".into(),
            domain: None,
            operator_pubkey: "1111111111111111111111111111111111111111111111111111111111111111"
                .into(),
        },
    )
    .await
    .expect_err("an unswept placeholder must fail the deploy");

    // Fail-closed contract: the install step dies (exit 1) BEFORE run.sh
    // starts. The fixture sshd discards stderr, so assert the step+exit and
    // that the unswept key is still IN the .env — the guard's real output
    // (the placeholder message) was verified against the live relay.
    let msg = format!("{err}");
    assert!(
        msg.contains("step 'run.sh start' failed") && msg.contains("exit Some(1)"),
        "fail-closed at run.sh start: {msg}"
    );
    let env = std::fs::read_to_string(dir.join("deploy/compose/.env")).unwrap();
    assert!(
        env.lines().any(|l| l == "SOMETHING_NEW=CHANGE_ME_X"),
        "unknown placeholder key left untouched (that is what trips the guard): {env}"
    );

    server.abort();
}

#[tokio::test(flavor = "multi_thread")]
async fn relay_deploy_with_domain_provisions_tls_local_ca() {
    let base = tempfile::tempdir().unwrap();
    let dir = base.path().join("relay");
    let dir_s = dir.display().to_string();
    let tar_stub = format!(
        "mkdir -p {dir}/deploy/compose && \
         printf '#!/bin/sh\\necho RUNSH-OK\\nexit 0\\n' > {dir}/deploy/compose/run.sh && \
         chmod +x {dir}/deploy/compose/run.sh && \
         printf 'BUZZ_RELAY_PRIVATE_KEY=CHANGE_ME_64_HEX\\nBUZZ_DOMAIN=buzz.example.com\\n' \
           > {dir}/deploy/compose/.env.example && \
         printf 'services:\\n  caddy:\\n    volumes:\\n      - ./Caddyfile:/etc/caddy/Caddyfile:ro\\n' \
           > {dir}/deploy/compose/compose.caddy.yml && \
         printf '{{$BUZZ_DOMAIN}} {{\\n  encode zstd gzip\\n  reverse_proxy relay:3000\\n}}\\n' \
           > {dir}/deploy/compose/Caddyfile && exit 0\n",
        dir = dir_s
    );
    let openssl_stub = format!(
        "mkdir -p {dir}/deploy/compose/certs && \
         touch {dir}/deploy/compose/certs/ca.crt {dir}/deploy/compose/certs/ca.key \
              {dir}/deploy/compose/certs/relay.example.crt {dir}/deploy/compose/certs/relay.example.key && exit 0\n",
        dir = dir_s
    );
    let docker_up = "if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"version\" ]; then echo 'Docker Compose version v2.24.4'; exit 0; fi; if [ \"$1\" = \"compose\" ] && [ \"$2\" = \"up\" ]; then echo UP-OK; exit 0; fi; exit 0\n";
    let scripts: Vec<(&str, String)> = vec![
        ("docker", docker_up.into()),
        ("curl", CURL_OK.into()),
        ("tar", tar_stub),
        ("openssl", openssl_stub),
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
            deploy_dir: dir_s.clone(),
            http_port: 3000,
            buzz_ref: DEFAULT_BUZZ_REF.into(),
            lxc: None,
            owner_pubkey: "072696bde8f03234433ddcc3587464e92a51f5d6906ade6b2aab2e1313010371".into(),
            relay_url: "http://relay-box:3000".into(),
            operator_pubkey: "1111111111111111111111111111111111111111111111111111111111111111"
                .into(),
            domain: Some("relay.example".into()),
        },
    )
    .await
    .unwrap();
    assert_eq!(res.relay_url, "https://relay.example");
    assert!(res.detail.contains("LOCAL CA"), "{res:?}");
    assert!(res.detail.contains("relay.example"), "{res:?}");

    // The DOMAIN is the identity: .env carries the domain + wss/https URLs.
    let env = std::fs::read_to_string(dir.join("deploy/compose/.env")).unwrap();
    assert!(env.contains("BUZZ_DOMAIN=relay.example"), "{env}");
    assert!(env.contains("RELAY_URL=wss://relay.example"), "{env}");
    assert!(
        env.contains("BUZZ_MEDIA_BASE_URL=https://relay.example/media"),
        "{env}"
    );
    assert!(
        env.contains("BUZZ_MEDIA_SERVER_DOMAIN=relay.example"),
        "{env}"
    );

    // The TLS local-CA posture landed: certs materialized, the Caddyfile
    // serves the domain cert, compose mounts the certs dir.
    let certs = dir.join("deploy/compose/certs");
    assert!(certs.join("ca.crt").exists() && certs.join("relay.example.crt").exists());
    let caddy = std::fs::read_to_string(dir.join("deploy/compose/Caddyfile")).unwrap();
    assert!(
        caddy.contains("tls /etc/caddy/certs/relay.example.crt"),
        "{caddy}"
    );
    let compose = std::fs::read_to_string(dir.join("deploy/compose/compose.caddy.yml")).unwrap();
    assert!(compose.contains("./certs:/etc/caddy/certs:ro"), "{compose}");

    server.abort();
}
