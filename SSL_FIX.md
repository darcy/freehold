# SSL Fix — RESOLVED (2026-09-05)

The public proxy host now serves Caddy's real Let's Encrypt cert for both
`relay.librem.freehold.technology` and `cp.librem.freehold.technology`, **not**
the Traefik default cert. Verified:

```
echo | openssl s_client -connect 192.168.30.8:443 -servername relay.librem.freehold.technology \
  | openssl x509 -noout -subject -issuer
# subject=CN=relay.librem.freehold.technology   issuer=Let's Encrypt (YR1), expires Dec 4 2026
curl -sk https://relay.librem.freehold.technology   # HTTP 200
curl -sk https://cp.librem.freehold.technology     # HTTP 200
```

## What was actually wrong (three stacked bugs)

The SSL_FIX.md's original root cause (Traefik holding 80/443) was real and is
fixed. But removing it exposed two further, previously-hidden problems:

1. **Stale Traefik install**: k3s bundled Traefik + Servicelb owned 80/443.
   Fixed `--disable traefik --disable servicelb` in the k3s install script
   (`orchestrator/internal/cli/rebuild.go`, `k3sInstallScript`) and on the live
   node, and deleted the residual `traefik` deploy/svc + `svclb-*` ds. This got
   Caddy to start binding 80/443.

2. **Caddy certs were always 0-byte**: Caddy then failed on
   `tls: failed to find any PEM data` because the install wrote empty cert
   files. Root causes:
   - `caddyCertInstallScript` fed the files into the pod over
     `kubectl exec ... < file` **stdin**, which stdin is never forwarded through
     the runner→pct→guest→kubectl chain — `cat` read EOF and wrote 0 bytes.
     **Fix**: write the certs directly into the local-path backing dir on the
     k3s node (the directory Caddy's hostNetwork pod mounts at `/data`).
   - The PRIVATE KEY was injected as `$CERT_KEY_<slot>` from the runner, but the
     **runner's SSH connector never forwarded secret env vars to the remote
     host** (`channel.exec(false, cmd)` with no envs) — a documented known gap.
     So the key was always empty over SSH. **Fix**: the runner now resolves
     every requested secret and prefixes the remote command with `export
     <NAME>='<escaped>'` (built after the audit signs the original cmd, so the
     secret never appears in audited command text).

3. **Stale Terminating LoadBalancer svc hijacking `.8:443`**: even with Caddy
   listening on `*:443`, the leftover `service/traefik` (externalIP `.8`, port
   443) was stuck Terminating (its `load-balancer-cleanup` finalizer was never
   cleared because Servicelb is disabled). Its IPVS rule DNATed `.8:443` to the
   deleted Traefik backend → `connection refused`, stealing the port from
   Caddy. **Fix**: cleared the finalizer (`kubectl patch svc traefik -p
   '{"metadata":{"finalizers":[]}}'`), which let the deletion finish and dropped
   the IPVS rule.

## Persistent code fixes shipped (working tree, not yet committed/PR'd)

- `orchestrator/internal/cli/rebuild.go` — `k3sInstallScript` adds
  `--disable traefik --disable servicelb`; `caddyCertInstallScript` writes certs
  into the local-path node dir instead of the broken kubectl/stdin path.
- `runner/src/ssh.rs` + `runner/src/mcp.rs` — SSH connector forwards requested
  secret env vars to the remote command (root-cause fix for empty keys over
  SSH). Runner binary rebuilt; `~/.cargo/bin/runner` updated.
- `orchestrator/internal/cli/rebuild_test.go` — updated
  `TestCaddyCertInstallScript` for the new mechanism.

## Note

The already-issued LE certs were recovered from the runner's signed audit log
(the public fullchain rides the install command) and re-matched against the
runner's sealed keys — no fresh Let's Encrypt issuance was needed, which dodged
the LE `too many certificates` hourly/weekly rate limit until `2026-09-06`.
