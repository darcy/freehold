package cpbuild

import (
	"encoding/base64"
	"fmt"
)

// FreeholdChannelID is the shared #freehold channel id (the agent workroom —
// the fallback wake's target when a mention kick cannot run).
const FreeholdChannelID = relayFreeholdChannel

// BuildMentionKicker returns the mention watcher's Kick: one buzz-acp
// `run --task` exec'd into the agent's pod through the same box-runner path
// every build stage uses (kubectl on the k3s guest; the pod env carries the
// agent's identity + litellm key, so the task session is the agent's own).
// The task JSON rides base64 (shell-safe); buzz-acp's version-1 terminal
// record is the command's stdout. The pod name is validated as DNS-1123 —
// a registry row cannot inject shell.
func BuildMentionKicker(spec *Spec) func(pod, taskJSON string, timeoutS uint64) (string, error) {
	return func(pod, taskJSON string, timeoutS uint64) (string, error) {
		if !validPodName(pod) {
			return "", fmt.Errorf("mention kick: pod name %q is not a DNS-1123 label", pod)
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(taskJSON))
		cmd := fmt.Sprintf(`pct exec %d -- %s exec -n agents %s -- sh -c "printf '%%s' '%s' | base64 -d | buzz-acp run --task -"`,
			spec.K3sVmid, kubeKubectl(), pod, b64)
		return spec.execOut(cmd, timeoutS)
	}
}

// validPodName is the DNS-1123 label the k8s object names use.
func validPodName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
