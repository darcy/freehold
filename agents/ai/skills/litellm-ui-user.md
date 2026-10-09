---
name: litellm-ui-user
description: Mint a LiteLLM UI login for the operator when they want to see the gateway's admin surface (models, keys, spend). Runs through your litellm-api-admin door — the credential is injected, never pasted in chat.
metadata:
  openclaw:
    target: lxc
---

# Mint a LiteLLM UI user

The LiteLLM gateway serves its admin UI at `/ui` on the gateway itself
(`http://<litellm-ip>:4000/ui`; the public edge fronts it at
`https://litellm.cp.<domain>/ui` once the app is exposed). The UI logs in
with LiteLLM USER accounts (not the master key).

## Create the user

Your `litellm-api-admin` door's credential is the gateway MASTER key (or an
admin key) — the exec env carries it injected; reference it by name, never
echo it.

```sh
curl -sS "$LITELLM_URL/user/new" \
  -H "Authorization: Bearer $LITELLM_API_ADMIN" \
  -H "Content-Type: application/json" \
  -d '{"user_email":"<the operator's email>","user_role":"app_owner"}'
```

- `app_owner` can see models, keys, and spend in the UI without being able
  to reconfigure the gateway's core settings — the right default for the
  operator's own account. Use `app_owner` unless they ask for more.
- The response carries the user's `token` (a LiteLLM key). Hand it to the
  operator IN CHAT — a LiteLLM user key is a capability, but it is not a
  freehold secret (it is re-mintable, scoped, and revocable from the same
  API).

## Verify before you report

```sh
curl -sS "$LITELLM_URL/user/info" -H "Authorization: Bearer $LITELLM_API_ADMIN" \
  -d '{"user_id":"<the returned user id>"}'
```

Report: the user's email, role, the UI URL, and the key. Tell the operator
the gate comes FIRST (their Nostr login) and the UI login is second.
