package cpbuild

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"freehold/agents"
	"freehold/contract/console"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
)

// update_agent — the CPA's edit surface for the agents it created, the verb
// between create_agent (mint) and manage_agent (drop). Purpose, model and
// channels re-assert through the create flow itself: create is idempotent by
// construction (the identity is first-run-wins, channel joins are guarded by
// IsMemberAuth, the manifest is delete-then-apply and the pod re-reads its
// prompt on every spawn), so a purpose edit IS a registry write plus a pod
// re-apply — the exact legs the reconcile already runs on every build. Rename
// is the only genuinely new machinery: the durable identity dir, the workspace
// dir and the registry row all move to the new name BEFORE create runs under
// it, so the same pubkey is re-applied under the new derived object names and
// chat history, grants and memory (all pubkey-keyed) follow untouched.
//
// Core identities (the CPA + the four departments) are refused outright: their
// names and prompts are repo-embedded (agents/<name>/prompt.md), and a repo PR
// through the update flow is their path. A department rename would also break
// the name-keyed department tables the build and respond gates read.

// BuildUpdateAgentFn binds the update_agent flow to a Spec + the agent-tools
// registry. It reuses BuildCreateAgentFn for every relay-side and pod leg —
// the same call reconcile and revoke_runner use to re-assert a pod.
func BuildUpdateAgentFn(spec *Spec, reg *agenttools.Registry) agent.UpdateAgentFn {
	create := BuildCreateAgentFn(spec)
	return func(args agent.UpdateArgs) (string, error) {
		const verb = "update_agent"
		name := strings.TrimSpace(args.Name)
		if name == "" {
			return "", fmt.Errorf("%s: name is required", verb)
		}
		newName := strings.TrimSpace(args.Rename)
		requestedModel := strings.TrimSpace(args.Model)
		if requestedModel != "" && !slices.Contains(agent.CustomLiteLLMModels, requestedModel) {
			return "", fmt.Errorf("%s: model must be one of [%s]", verb, strings.Join(agent.CustomLiteLLMModels, ", "))
		}
		if err := coreAgentRefused(spec, name, verb); err != nil {
			return "", err
		}
		row, err := registryRow(reg, name)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", verb, name, err)
		}

		// Final field set: absent fields keep the row's current values.
		finalName, finalPurpose, channels, private, finalModel := resolvedUpdate(row, args)

		var legs []string
		renamed := newName != "" && newName != name
		// podMoved is false for a display-only rename ("My Agent" → "My
		// Agent!"): nothing structural moves — the registry row and the relay
		// profile carry the new spelling.
		podMoved := false
		if renamed {
			// The rename legs. Guard the destination from every seat: the
			// registry row, a core identity, the CPA's pod name, and any
			// OTHER row whose name sanitizes to the same pod (a rename that
			// would make two agents share one pod's objects).
			if err := coreAgentRefused(spec, newName, verb); err != nil {
				return "", fmt.Errorf("%s %s → %s: the destination is a core identity — refusing", verb, name, newName)
			}
			if _, err := registryRow(reg, newName); err == nil {
				return "", fmt.Errorf("%s %s → %s: the destination name is already registered", verb, name, newName)
			}
			newPod := agent.PodName(newName)
			if newPod == agent.PodName(spec.cpaNameOrDefault()) {
				return "", fmt.Errorf("%s %s → %s: the pod name %q collides with the control plane agent", verb, name, newName, newPod)
			}
			rows, rerr := reg.Agents()
			if rerr != nil {
				return "", fmt.Errorf("%s: read agent registry: %w", verb, rerr)
			}
			for _, other := range rows {
				if other.Name != name && agent.PodName(other.Name) == newPod {
					return "", fmt.Errorf("%s %s → %s: the pod name %q collides with %s", verb, name, newName, newPod, other.Name)
				}
			}

			// A display-only rename sanitizes to the same pod: skip the
			// structural moves entirely.
			samePod := newPod == agent.PodName(name)
			oldPod := agent.PodName(name)
			// The durable identity dir IS the identity: move it first, and
			// roll the moves back if the registry rename does not land (a row
			// pointing at a pubkey whose dir is gone would re-mint a fresh
			// key on the next reconcile — the one failure this ordering must
			// never allow).
			if !samePod {
				oldDir := AgentIdentityPath(spec.agentIdentityDir(), name)
				newDir := AgentIdentityPath(spec.agentIdentityDir(), newName)
				if _, err := os.Stat(oldDir); err != nil {
					return "", fmt.Errorf("%s %s: the durable identity dir is unreadable (%v) — refusing to rename away from an identity this flow cannot verify", verb, name, err)
				}
				if _, err := os.Stat(newDir); err == nil {
					return "", fmt.Errorf("%s %s → %s: the destination identity dir already exists — refusing to overwrite an identity", verb, name, newName)
				}
				if err := spec.resolveK3sVmid(verb, name); err != nil {
					return "", err
				}
				if err := os.Rename(oldDir, newDir); err != nil {
					return "", fmt.Errorf("%s: move the identity dir %s → %s: %w", verb, oldDir, newDir, err)
				}
				rollback := func() {
					if rerr := os.Rename(newDir, oldDir); rerr != nil {
						legs = append(legs, fmt.Sprintf("[UNVERIFIED] identity-dir rollback failed (%v) — %s must be moved back by hand", rerr, newDir))
					}
				}
				if err := spec.moveWorkspace(oldPod, newPod); err != nil {
					rollback()
					return "", fmt.Errorf("%s: move the workspace dir: %w", verb, err)
				}
				if err := reg.RenameAgent(name, newName); err != nil {
					rollback()
					return "", fmt.Errorf("%s: move the registry row: %w", verb, err)
				}
				// The minted gateway key is POD-keyed (its alias, its Secret,
				// its store record): re-key the record so the same key — and
				// its spend history — follows the renamed identity instead of
				// minting an orphan.
				if err := spec.moveLitellmKeyRecord(oldPod, newPod); err != nil {
					rollback()
					return "", fmt.Errorf("%s: move the litellm key record: %w", verb, err)
				}
				legs = append(legs,
					fmt.Sprintf("[verified] identity dir moved (%s → %s) — the same nsec re-applies under the new name", sanitizeDir(name), sanitizeDir(newName)),
					fmt.Sprintf("[verified] workspace moved (%s/%s → %s/%s) — the agent's files follow the rename", agent.AgentWorkspaceRoot, oldPod, agent.AgentWorkspaceRoot, newPod),
					fmt.Sprintf("[verified] litellm key follows the rename (pod %s → %s) — same minted key, spend history intact", oldPod, newPod),
					"[verified] registry row moved, identity (pubkey) preserved — chat history, grants and memory follow")
			} else if err := reg.RenameAgent(name, newName); err != nil {
				return "", fmt.Errorf("%s: move the registry row: %w", verb, err)
			}
			podMoved = !samePod
		}

		// Everything else rides the create flow under the FINAL name: relay
		// membership (idempotent), profile republished with the final display
		// name, channels re-joined (guarded), the pod deleted + re-applied
		// with the re-rendered prompt and model. For a rename this is the new
		// pod's first apply; for a field edit it is the re-apply.
		pub, cerr := create(finalName, finalPurpose, channels, private, finalModel)
		if cerr != nil {
			if renamed {
				return "", fmt.Errorf("%s: the rename state is moved (registry row%s under %q) but the pod re-apply FAILED (%v) — re-run %s name=%q to finish; it is idempotent", verb, podMovedPhrase(podMoved), newName, cerr, verb, newName)
			}
			return "", fmt.Errorf("%s %s: %w", verb, name, cerr)
		}
		if row.Pubkey != "" && pub != row.Pubkey {
			return "", fmt.Errorf("%s %s: the re-apply came back with a DIFFERENT pubkey (%s → %s) — the identity did not survive the apply; stop and have an operator look before anything else touches this agent", verb, finalName, row.Pubkey, pub)
		}
		if podMoved {
			// The old derived objects are deleted only here: the old pod kept
			// serving until the new one was up, so the rename's downtime is
			// one apply, not two.
			if err := spec.run(agent.AgentRetireScript(spec.K3sVmid, name), 120); err != nil {
				legs = append(legs, fmt.Sprintf("[UNVERIFIED] the old pod objects under %q could not be deleted (%v) — the old pod may still be running as the same identity; delete them by hand inside the k3s guest (kubectl delete pod/service/configmap %s, secret %s-identity, -n agents)", name, err, agent.PodName(name), agent.PodName(name)))
			} else {
				legs = append(legs, fmt.Sprintf("[verified] old pod objects under %q retired (pod, service, prompt configmap, identity secret)", name))
			}
		}
		// RegisterAgent (inside create) re-writes the row from scratch and
		// wipes the purpose/channel/model fields — the same re-assert the
		// reconcile does after every create.
		_ = reg.SetPurpose(finalName, finalPurpose)
		_ = reg.SetChannels(finalName, channels, private)
		_ = reg.SetModel(finalName, finalModel)
		legs = append(legs,
			fmt.Sprintf("[verified] pod re-applied as %q (prompt re-rendered from purpose %q, model %s, channels [%s])", finalName, finalPurpose, modelOrDefault(finalModel), strings.Join(channels, ", ")))
		what := "updated"
		if renamed {
			what = fmt.Sprintf("renamed %s → %s (identity preserved, pubkey %s)", name, newName, row.Pubkey)
		}
		return fmt.Sprintf("update_agent: %s\n%s", what, strings.Join(legs, "\n")), nil
	}
}

// podMovedPhrase words the create-failure recovery note by what actually moved.
func podMovedPhrase(podMoved bool) string {
	if podMoved {
		return ", identity dir, workspace and pod objects"
	}
	return ""
}

// resolvedUpdate merges an update_agent call's args over its registry row —
// the absent-means-keep contract in one pure place: purpose and model fall
// back to the row (a purpose edit must never silently re-resolve a Code or
// ExtraThinking agent onto General), the channel list overrides only when one
// was given, and Private applies only then (a bare private:true with no list
// is ignored). Rename resolves to the row's own name when absent.
func resolvedUpdate(row console.AgentInfo, args agent.UpdateArgs) (name, purpose string, channels []string, private bool, model string) {
	name = row.Name
	if r := strings.TrimSpace(args.Rename); r != "" {
		name = r
	}
	purpose = row.Purpose
	if p := strings.TrimSpace(args.Purpose); p != "" {
		purpose = p
	}
	channels, private = reconciledChannels(row)
	if len(args.Channels) > 0 || strings.TrimSpace(args.Channel) != "" {
		channels = append([]string{}, args.Channels...)
		if strings.TrimSpace(args.Channel) != "" {
			channels = append([]string{args.Channel}, channels...)
		}
		channels = channelNames(channels)
		private = args.Private
	}
	model = row.Model
	if m := strings.TrimSpace(args.Model); m != "" {
		model = m
	}
	return
}

// BuildRemoveAgentFn binds the pod-retire half of manage_agent remove: the
// agent's derived k8s objects are deleted before the row drops, closing the
// gap where a removed agent's pod kept running with no row to name it. The
// durable workspace dir is deliberately kept — it is data.
func BuildRemoveAgentFn(spec *Spec) agent.RemoveAgentFn {
	return func(name string) (string, error) {
		const verb = "manage_agent remove"
		name = strings.TrimSpace(name)
		if err := coreAgentRefused(spec, name, verb); err != nil {
			return "", err
		}
		// The minted gateway key must not outlive the row: revoke it (and drop
		// the store record) BEFORE retiring the pod objects — a failure leaves
		// the verb unfinished with the agent intact, the same loud-retry shape
		// as the retire itself. A world without litellm, or a key never
		// minted, is a no-op.
		if err := spec.revokeAgentLitellmKey(name); err != nil {
			return "", fmt.Errorf("%s %s: revoking the litellm key FAILED (%v) — the registry row is untouched; retry once the gateway answers", verb, name, err)
		}
		if err := spec.resolveK3sVmid(verb, name); err != nil {
			return "", err
		}
		if err := spec.run(agent.AgentRetireScript(spec.K3sVmid, name), 120); err != nil {
			return "", fmt.Errorf("%s %s: retiring the pod FAILED (%v) — the registry row is untouched; fix the k3s guest and retry, or drop the row from the console if the pod is already gone", verb, name, err)
		}
		return fmt.Sprintf("%s %s: pod %q retired (pod, service, prompt configmap, identity secret); the durable workspace dir %s is kept — it is data", verb, name, agent.PodName(name), agent.AgentWorkspaceDir(agent.PodName(name))), nil
	}
}

// coreAgentRefused is the locked two-tier boundary for the agent-side edit
// surface: the CPA and the four departments are repo-defined (agents/<name>/
// prompt.md, name-keyed department tables in the build and the respond gates),
// so no registry-side verb may rename, re-prompt or retire them.
func coreAgentRefused(spec *Spec, name, verb string) error {
	if name == "" {
		return fmt.Errorf("%s: an agent name is required", verb)
	}
	if name == spec.cpaNameOrDefault() {
		return fmt.Errorf("%s %s: the control plane agent is a core identity — its name and prompt live in the repo (agents/freehold), not the registry", verb, name)
	}
	for _, dep := range agents.DepartmentNames() {
		if dep == name {
			return fmt.Errorf("%s %s: %s is a core department — its name and prompt live in the repo (agents/%s), not the registry", verb, name, name, dep)
		}
	}
	return nil
}

// resolveK3sVmid resolves the k3s vmid by hostname when the Spec was recorded
// before k3s booted (the create flow's same resolution, mirrored so update/
// remove do not have to run after a build).
func (s *Spec) resolveK3sVmid(verb, name string) error {
	if s.K3sVmid != 0 {
		return nil
	}
	if err := s.resolveGuestVmids(); err != nil {
		return fmt.Errorf("%s %s: resolve the k3s vmid: %w", verb, name, err)
	}
	if s.K3sVmid == 0 {
		return fmt.Errorf("%s %s: no k3s vmid recorded/resolvable — the pod surface has nothing to apply against", verb, name)
	}
	return nil
}

// moveWorkspace moves an agent's durable workspace dir to the renamed pod's
// dir, inside the k3s guest. An absent source dir is a no-op: a fresh agent's
// first apply creates the destination (the manifest script mkdirs it), and
// there is nothing to carry over.
func (s *Spec) moveWorkspace(oldPod, newPod string) error {
	mv := fmt.Sprintf("pct exec %d -- sh -c 'if [ -d %s/%s ]; then mv %s/%s %s/%s; fi'",
		s.K3sVmid, agent.AgentWorkspaceRoot, oldPod, agent.AgentWorkspaceRoot, oldPod, agent.AgentWorkspaceRoot, newPod)
	if err := s.run(mv, 60); err != nil {
		return fmt.Errorf("mv %s/%s → %s/%s in the k3s guest: %w", agent.AgentWorkspaceRoot, oldPod, agent.AgentWorkspaceRoot, newPod, err)
	}
	return nil
}

// modelOrDefault names a model choice for the report ("" reads as General —
// the create flow's own default).
func modelOrDefault(model string) string {
	if strings.TrimSpace(model) == "" {
		return "General (default)"
	}
	return model
}
