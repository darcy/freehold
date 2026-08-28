# freehold-orchestrator (Go) — Wire pins

The Go orchestrator must reproduce the Rust `core` surface byte-exactly. This
document records the wire pins, kept in sync with the cross-verification
harness (`orchestrator-go/harness/`). The Rust `core`/`runner` crates remain in
the workspace as byte-exact reference oracles; a committed Go↔Rust
cross-verification harness is the release gate for every primitive below.

Pins transcribed from `core/src/*` on the `refactor-go` branch (post-Chunk
2.6.1). If a pin drifts from the harness, the harness is the truth and this
doc must be updated with it.

## Sealed box (`core/src/crypto.rs`)

Wire layout (versioned):

```
[0]       version byte (currently 1)
[1..33]   ephemeral X25519 public key
[33..45]  12-byte nonce
[45..]    ChaCha20-Poly1305 ciphertext (tag appended)
```

- `FORMAT_VERSION = 1`, `KEY_LEN = 32`, `EPHEMERAL_PUB_LEN = 32`,
  `NONCE_LEN = 12`.
- `SALT = b"freehold-sealed-box-v1"`.
- Key schedule:
  `AEAD_key = HKDF-SHA256(salt=SALT, ikm=DH(eph_sk, recip_pk), info=eph_pk ‖ recip_pk)`.
- AEAD AAD = the secret NAME bytes (a blob can only be opened under the name
  it was sealed with).
- Non-contributory DH (all-zero shared secret) → error.
- `seal(recipient_pub, aad, plaintext) -> versioned blob`.
- `open(recipient_secret, aad, blob) -> plaintext`; rejects version != 1,
  short blob, wrong AAD, non-contributory DH, AEAD failure.

## SecretPackage JSON (`core/src/secrets.rs`)

```json
{
  "secrets": { "<name>": "<hexCiphertext>" },
  "targets": { "<name>": { "kind": "...", "address": "...", "secret": "..." } },
  "grants": ["<pubkeyHex>"]
}
```

- `secrets` is a JSON **object keyed by secret name** — Go `map[string]string`.
- `targets` / `grants` are `serde(default)` → serialized even when empty.
- Field order: serde with `BTreeMap` is **sorted** — Go must serialize the
  maps as SORTED (byte-identical JSON requirement).
- Written atomic-0600 into a dir (created 0700); file `secrets.json`.

## MCP signature (`core/src/auth.rs`)

- Headers: `x-freehold-pubkey`, `x-freehold-sig`, `x-freehold-ts`.
- Canonical signed string: `"{runner_pubkey}|{ts}|{raw_body}"` (raw body as
  received — the RAW bytes, never re-serialized).
- Signature: BIP-340 Schnorr over that string (the `audit::SignedEvent` shape
  — SHA-256 of the canonical string, then BIP-340, deterministic no-aux-rand).
- Window: `TS_WINDOW_SECS = 60`.
- Audience bound: runner_pubkey in the string closes cross-runner replay.

## Nostr canonical event (`core/src/nip98.rs`)

- `id = sha256(pkcs7_serialize(["0", pubkey_hex, created_at, kind, tags,
  content]))` where pkcs7_serialize is the array JSON-encoded **without
  whitespace**.
- Signature: BIP-340 Schnorr over the id (no aux rand panic-free).
- Tags are `[[]string]` → `[["k","v"],...]`.
- Kinds:
  - `KIND_HTTP_AUTH = 27235`
  - `CHANNEL_CREATE_KIND = 9007`
  - `PUT_USER_KIND = 9000`
  - `REMOVE_USER_KIND = 9001`
  - `GROUP_META_KIND = 39000`
  - `GROUP_MEMBERS_KIND = 39002`

## NIP-98 (`core/src/nip98.rs`)

- `Authorization: Nostr <base64(eventJson)>`
- Event kind 27235; tags `u` = full URL (`scheme://host:port/path?query`) and
  `t` = HTTP method. Both REQUIRED by buzz's bridge.

## NIP-44 v2 self-encryption (`core/src/memory.rs`)

- Conversation key from the agent's own keypair (sender == receiver).
- Base64 output, `len >= 32`.
- MUST match `rust-nostr`'s `nip44::V2` payload byte-for-byte (harness-proven;
  the one primitive with a full external spec). Decryption: wrong key or
  tamper → fail closed (Err). Kind `MEMORY_KIND = 30174`.

## Relay bridge (`core/src/relay_http.rs`)

- `/events` (POST signed event JSON) and `/query` (POST filters ARRAY), both
  NIP-98-authed.
- Runner channel id = `sha256(runnerNostrPubkey)[0..16]` as **32 lowercase
  hex** (NOT 64 — buzz parses `h` as a UUID; 64-hex → None → 9000 rejected).

## NIP-29 kinds (`core/src/nip98.rs` + `delegate.rs`)

- 9007 create, 9000 put-user, 9001 remove-user, 9021 join.
- 39000 group meta, 39002 roster (**minted with a `d` tag**; filter `#d`,
  never `#h`).
- Kind 9 channel messages (`t`=fh-profile tag for runner meta).
- Kind 30174 engram (d-tag `<agent_pk>#<key>`).
- Runner profile metadata: kind 39000 (replaceable per (author, h)) carries
  identity pubkeys, connector kind/address, status, secret NAME — never
  material.
