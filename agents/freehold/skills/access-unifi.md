# The access-unifi skill — activating UniFi access for network

This is freehold's runbook for standing up UniFi Network access for the
network department: one door (`unifi-api-admin`), provisioned EMPTY, filled by
the operator in the console, briefed into network's memory, and claimed live
only on an authenticated probe. It exists so the activation is seamless when
the operator asks for it — and so nothing about a live network is ever
improvised.

## The caution (relay this to network verbatim)

> Assume this is a live network — real devices, real people on it. Treat it
> with the utmost caution: before ANY change to UniFi, confirm with the
> operator first and state explicitly whether it could cause downtime. No
> writes without that confirmation. Default to read-only probing. Never sweep
> or guess credentials against the controller. Never touch the admin account
> the door uses.

## The setup flow

1. **Confirm with the operator** (the check-in questions, before anything):
   the controller URL (e.g. `https://192.168.1.1` — the LAN controller,
   self-signed TLS), and the account: who creates it (usually the operator),
   and the least-privilege role — an **Admin** role on the Network
   application, nothing more than the job needs.
2. **Provision the door EMPTY** through `provision_runner`:
   `name=unifi-api-admin, kind=unifi, address=<controller URL>,
   grant_to=[network]`. The tool is credential-blind — it takes no secret;
   never accept one and never put a credential in chat.
3. **DM the operator the door page link** the tool returns
   (`https://cp.<domain>/runner/unifi-api-admin`). They fill **username +
   password**; the console seals it to the door's key and restarts the door.
4. **Brief network — mandatory, not optional.** Post to `#freehold-network`
   (mention network so it triggers): the caution above, the env contract
   below, the probe recipe below — and the explicit instruction to **write
   the rule into core memory plus a UniFi-specific cold memory and re-state
   the rule back**. The re-statement is how the briefing is verified: memory
   rides the relay-persisted store and survives every pod re-apply; prompts
   are fixed at create and cannot carry per-world facts. An unacknowledged
   briefing is a briefing that did not happen.
5. **Verify before claiming.** The door is live only when network's probe
   reaches an AUTHENTICATED session (the recipe below). A green
   reachability/TLS check alone is a partial door — say "partial", not "live".

## The env contract (what the operator filled)

The exec env carries **`UNIFI_API_ADMIN`** — a JSON object with the keys
`username` and `password`. It is a LOGIN PAIR, not an API key: there is no
`X-API-KEY` on this door. Authenticate:

```
POST <controller>/api/auth/login   body: {"username": ..., "password": ...}
```

The response carries the session token (or sets the session cookie) — call
the API with it. If the operator instead provisions a UniFi API Access key
(UniFi OS settings → API Access), the door's contract changes with it — the
tool description and this skill are updated in the same change; never mix the
two shapes in one door.

## The probe recipe (network's verification)

1. Reachability + TLS: `curl -sk -o /dev/null -w '%{http_code}'
   <controller>/` — 200 expected; note the self-signed cert (probe with
   `-k`).
2. Authenticate: parse `UNIFI_API_ADMIN` (JSON), POST it to
   `<controller>/api/auth/login`, capture the token/cookie.
3. One authenticated READ: `GET <controller>/proxy/network/v2/api/site/default/devices`
   (or `/api/s/default/stat/device` on older firmware).
4. Report exactly what was verified — reachability, authentication, and the
   read — and nothing more.

## Where the API docs live

The controller serves its OWN version-exact API documentation at
`https://<controller>/proxy/network/v2/api/swagger` — the best source for the
exact firmware in front of you. The official "UniFi Network API" docs live on
help.ui.com. Community API clients exist; prefer the swagger.

## Rotation

The operator re-fills the same door page (`/runner/unifi-api-admin`); the
console re-seals and restarts the door. Network's memory is unchanged — the
env name and shape do not move.

## State of this skill

The credential shape is enforced in three places that must move together: the
`provision_runner` tool descriptions (the server + the stdio bridge), the
console's fill form, and this skill. A change to one is a change to all
three. The door's self-check reports reachability; only an authenticated
probe makes it "live" — that distinction is the whole point of this skill.
