//! Phase C (Chunk 2), C2: the CP self-adds to the relay it helped create.
//!
//! Membership writes are gated on the RELAY signing key (kind 13534 via
//! `buzz-admin`, BUZZ_SURFACE §3) — so the CP cannot self-add with its own
//! keypair, and it NEVER holds the relay key. The relay-admin RUNNER (the
//! box runner, `pct exec` into the relay LXC) performs the add; the CP
//! drives it as a plain generic exec. Being a member is enforced at the
//! relay's protocol layer regardless of who performed the add.

use crate::bootstrap::{BootstrapError, plain_path};
use crate::client::McpClient;

pub const DEFAULT_BUZZ_COMPOSE_DIR: &str = "/srv/buzz-relay/deploy/compose";

#[derive(Debug, Clone)]
pub struct RelayMemberAddSpec {
    /// Nostr pubkey to add — 64-hex (buzz-admin also accepts npub).
    pub pubkey: String,
    /// "member" (default) or "admin"; the OWNER role comes from
    /// RELAY_OWNER_PUBKEY and cannot be set here (per buzz-admin).
    pub role: Option<String>,
    /// The LXC on the box holding the relay compose stack (None = the
    /// target IS the relay host).
    pub lxc: Option<u32>,
    /// Compose project dir on the relay host (where `.env` lives).
    pub compose_dir: String,
}

#[derive(Debug)]
pub struct RelayMemberResult {
    pub detail: String,
}

pub async fn relay_member_add(
    client: &McpClient,
    target: &str,
    spec: &RelayMemberAddSpec,
) -> Result<RelayMemberResult, BootstrapError> {
    if spec.pubkey.len() != 64 || !spec.pubkey.chars().all(|c| c.is_ascii_hexdigit()) {
        return Err(BootstrapError::Verify(format!(
            "relay member pubkey must be a 64-hex Nostr pubkey (got {:?})",
            spec.pubkey
        )));
    }
    plain_path(&spec.compose_dir)?;
    let role = match &spec.role {
        None => String::new(),
        Some(r) => {
            if r != "admin" && r != "member" {
                return Err(BootstrapError::Verify(format!(
                    "relay member role must be 'member' or 'admin' (got {r:?})"
                )));
            }
            format!("--role {r}")
        }
    };

    // All single-quote-free: relay::lxc_cmd wraps the payload in single
    // quotes when deploying into the LXC.
    let cmd = format!(
        "cd {dir} && docker compose exec -T relay buzz-admin add-member --pubkey {pk} {role}",
        dir = spec.compose_dir,
        pk = spec.pubkey,
        role = role,
    )
    .trim_end()
    .to_string();
    let out = crate::bootstrap::exec_to_ok(
        client,
        target,
        &crate::relay::lxc_cmd(spec.lxc, &cmd),
        "buzz-admin add-member",
        120,
    )?;

    let pk = &spec.pubkey;
    let role_note = spec
        .role
        .as_deref()
        .map(|r| format!(" ({r})"))
        .unwrap_or_default();
    Ok(RelayMemberResult {
        detail: format!("relay member {pk}{role_note}: {}", out.stdout.trim()),
    })
}
