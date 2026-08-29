//! harness-oracle — the byte-exact Rust oracle for the Go orchestrator port.
//!
//! Reads one JSON request per line on stdin, writes one JSON response object
//! per line on stdout, and exits 0 on success (non-zero on parse error).
//!
//! Exposes the freehold-core primitives the Go port must reproduce byte-for-byte
//! (crypto/identity, sealed box, SecretPackage, NIP-98, NIP-44, audit).
//! The Go harness (orchestrator-go/harness) drives this binary with
//! `cargo run -p freehold-harness-oracle` and asserts its own outputs are identical.

use std::io::{self, BufRead, Write};

use freehold_core::{audit, crypto as fcore_crypto, identity, memory, nip98, secrets};
use serde_json::{Value, json};

fn hex_bytes(s: &str) -> Result<Vec<u8>, String> {
    hex::decode(s).map_err(|e| format!("invalid hex: {e}"))
}

fn secret_array(s: &str) -> Result<[u8; 32], String> {
    let b = hex_bytes(s)?;
    let arr: [u8; 32] = b
        .try_into()
        .map_err(|_| "secret must be 32 bytes".to_string())?;
    Ok(arr)
}

fn tags_value(v: &Value) -> Result<Vec<Vec<String>>, String> {
    v.as_array()
        .ok_or_else(|| "tags must be an array".to_string())?
        .iter()
        .map(|t| {
            t.as_array()
                .ok_or_else(|| "tag must be an array".to_string())?
                .iter()
                .map(|s| {
                    s.as_str()
                        .map(|x| x.to_string())
                        .ok_or_else(|| "tag elem must be string".to_string())
                })
                .collect()
        })
        .collect()
}

fn now_tagged(req: &Value) -> Result<i64, String> {
    req.get("now")
        .and_then(Value::as_i64)
        .ok_or_else(|| "missing i64 `now`".to_string())
}

fn created_at_tagged(req: &Value) -> Result<i64, String> {
    req.get("created_at")
        .and_then(Value::as_i64)
        .ok_or_else(|| "missing i64 `created_at`".to_string())
}

fn dispatch(req: Value) -> Result<Value, String> {
    let op = req.get("op").and_then(Value::as_str).ok_or("missing op")?;
    match op {
        "pubkey" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let id =
                identity::Identity::from_secrets(secret, [0u8; 32]).map_err(|e| e.to_string())?;
            Ok(json!({"pubkey": id.nostr_pubkey_hex()}))
        }
        "enc_pubkey" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            // enc_pubkey_hex depends only on the ENC secret; use a fixed valid
            // nostr scalar (1) so from_secrets' scalar check passes.
            let nostr_valid = [
                1u8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
                0, 0, 0, 0, 0,
            ];
            let id =
                identity::Identity::from_secrets(nostr_valid, secret).map_err(|e| e.to_string())?;
            Ok(json!({"enc_pubkey": id.enc_pubkey_hex()}))
        }
        "seal" => {
            let recip = hex_bytes(
                req.get("recipientPub")
                    .and_then(Value::as_str)
                    .ok_or("missing recipientPub")?,
            )?;
            let recip: [u8; 32] = recip
                .try_into()
                .map_err(|_| "recipientPub must be 32 bytes")?;
            let aad = req
                .get("aad")
                .and_then(Value::as_str)
                .ok_or("missing aad")?
                .as_bytes();
            let plaintext = req
                .get("plaintext")
                .and_then(Value::as_str)
                .ok_or("missing plaintext")?
                .as_bytes();
            let blob = fcore_crypto::seal(&recip, aad, plaintext).map_err(|e| e.to_string())?;
            Ok(json!({"blob": hex::encode(blob)}))
        }
        "open" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let aad = req
                .get("aad")
                .and_then(Value::as_str)
                .ok_or("missing aad")?
                .as_bytes();
            let blob = hex_bytes(
                req.get("blob")
                    .and_then(Value::as_str)
                    .ok_or("missing blob")?,
            )?;
            let pt = fcore_crypto::open(&secret, aad, &blob).map_err(|e| e.to_string())?;
            Ok(json!({"plaintext": String::from_utf8_lossy(&pt).into_owned()}))
        }
        "nip98_auth" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let method = req
                .get("method")
                .and_then(Value::as_str)
                .ok_or("missing method")?
                .to_string();
            let url = req
                .get("url")
                .and_then(Value::as_str)
                .ok_or("missing url")?
                .to_string();
            let now = now_tagged(&req)?;
            let auth = nip98::nip98_auth(&secret, &method, &url, now).map_err(|e| e.to_string())?;
            Ok(json!({"auth": auth}))
        }
        "sign_event" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let created_at = created_at_tagged(&req)?;
            let kind = req
                .get("kind")
                .and_then(Value::as_u64)
                .ok_or("missing kind")? as u32;
            let tags = tags_value(req.get("tags").ok_or("missing tags")?)?;
            let content = req
                .get("content")
                .and_then(Value::as_str)
                .ok_or("missing content")?
                .to_string();
            let (pubkey, id, sig) = nip98::sign_event(&secret, kind, created_at, tags, &content)
                .map_err(|e| e.to_string())?;
            Ok(json!({"pubkey": pubkey, "id": id, "sig": sig}))
        }
        "verify_event" => {
            let pubkey = req
                .get("pubkey")
                .and_then(Value::as_str)
                .ok_or("missing pubkey")?
                .to_string();
            let created_at = created_at_tagged(&req)?;
            let kind = req
                .get("kind")
                .and_then(Value::as_u64)
                .ok_or("missing kind")? as u32;
            let tags = tags_value(req.get("tags").ok_or("missing tags")?)?;
            let content = req
                .get("content")
                .and_then(Value::as_str)
                .ok_or("missing content")?
                .to_string();
            let sig = req
                .get("sig")
                .and_then(Value::as_str)
                .ok_or("missing sig")?
                .to_string();
            let pk = nip98::verify_event(&pubkey, created_at, kind, &tags, &content, &sig)
                .map_err(|e| e.to_string())?;
            Ok(json!({"pubkey": pk}))
        }
        "audit_sign" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let content = req
                .get("content")
                .and_then(Value::as_str)
                .ok_or("missing content")?
                .to_string();
            let ev = audit::sign_event(&secret, &content).map_err(|e| e.to_string())?;
            Ok(json!({"pubkey": ev.pubkey, "content": ev.content, "sig": ev.sig}))
        }
        "audit_build" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let kind = req
                .get("kind")
                .and_then(Value::as_u64)
                .ok_or("missing kind")? as u32;
            let tags = tags_value(req.get("tags").ok_or("missing tags")?)?;
            let content = req
                .get("content")
                .and_then(Value::as_str)
                .ok_or("missing content")?
                .to_string();
            let a = audit::Auditor::new(secret);
            let ev = a.event(kind, tags, &content).map_err(|e| e.to_string())?;
            Ok(ev)
        }
        "nip44_seal" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let value = req
                .get("value")
                .and_then(Value::as_str)
                .ok_or("missing value")?
                .to_string();
            let sealed = memory::seal_memory(&secret, &value).map_err(|e| e.to_string())?;
            Ok(json!({"sealed": sealed}))
        }
        "nip44_open" => {
            let secret = secret_array(
                req.get("secret")
                    .and_then(Value::as_str)
                    .ok_or("missing secret")?,
            )?;
            let sealed = req
                .get("sealed")
                .and_then(Value::as_str)
                .ok_or("missing sealed")?
                .to_string();
            let value = memory::open_memory(&secret, &sealed).map_err(|e| e.to_string())?;
            Ok(json!({"value": value}))
        }
        "secretpackage_new" => {
            let pkg: secrets::SecretPackage =
                serde_json::from_value(req.get("package").cloned().unwrap_or(Value::Null))
                    .map_err(|e| e.to_string())?;
            let s = serde_json::to_string(&pkg).map_err(|e| e.to_string())?;
            Ok(json!({"json": s}))
        }
        _ => Err(format!("unknown op: {op}")),
    }
}

fn main() {
    let stdin = io::stdin();
    let stdout = io::stdout();
    let mut out = stdout.lock();
    let handle = stdin.lock();
    for line in handle.lines() {
        let line = match line {
            Ok(l) => l,
            Err(e) => {
                eprintln!("invalid stdin: {e}");
                std::process::exit(1);
            }
        };
        if line.trim().is_empty() {
            continue;
        }
        let req: Value = match serde_json::from_str(&line) {
            Ok(v) => v,
            Err(e) => {
                eprintln!("invalid json: {e}");
                std::process::exit(1);
            }
        };
        let resp = match dispatch(req) {
            Ok(v) => v,
            Err(e) => json!({"error": e}),
        };
        if let Err(e) = writeln!(out, "{resp}") {
            eprintln!("write error: {e}");
            std::process::exit(1);
        }
        if let Err(e) = out.flush() {
            eprintln!("flush error: {e}");
            std::process::exit(1);
        }
    }
}
