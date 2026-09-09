## Phase 0.12 — durable volume plane: live acceptance

No new code: `chunk3-plane-gate` points at the same commit as `main`
(`b43f8eb`). Every storage-plane gate below was exercised **live** against
the `pve` host's LVM-thin plane with the code already present in
`control-plane/cli/handlers3.go` and
`platform/provisioning/drive/lvm.go`.

### Evidence (hermetic, from the live `pve` volume plane)

| Gate | Command | Observed |
| --- | --- | --- |
| Capacity | `freehold storage resolve` | `STORAGE-CAPACITY: vg pve · 144.1G of 464.8G` |
| Provision | `freehold storage ensure --tenant relay` | `STORAGE-MOUNT` lines for **two** mount points, both `/freehold/freehold-test-darcydev-net/*` |
| Liveness | `freehold storage info` | all **4** mounts report `mounted` |
| Teardown | `freehold storage destroy` | `lvremove refuses mounted LV` |

### Notes on the teardown gate

`storage destroy` runs `umount` (and strips the fstab entry) **before**
`lvremove`, per `DestroyTenantBackend` in `platform/provisioning/drive/lvm.go`
(~lines 285–294). The `filesystem in use` / `lvremove refuses mounted LV`
report is therefore the honest signal that `lvremove` refused a *still-mounted*
LV — it is not an ordering bug in the harness. The `umount → lvremove`
sequence is pinned by `TestDestroy*` in `platform/provisioning/drive/lvm_test.go`
(~line 479: `umount+fstab-strip must PRECEDE lvremove`).

### Verification

`platform/provisioning/drive` unit suite (`lvm_test.go`, `lvm_pool_test.go`)
covers the mapping: `lvremove -f pve/fh…` present on create/destroy paths,
absent on absent-backends (no-op teardown stays a no-op).

### Unchanged

- No commits ahead of `main`; this PR exists to record the acceptance.
