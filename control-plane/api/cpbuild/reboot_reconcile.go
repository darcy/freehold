package cpbuild

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"freehold/contract/console"
	"freehold/control-plane/api/agent"
	"freehold/control-plane/api/agenttools"
)

// The boot-time reboot revive. Agent pods are bare Pods with restartPolicy:
// Never (agent.go I5: an intentional clean exit stays terminal), so a hard
// stop/start of the k3s guest kills every container and the kubelet never
// revives them — the CP's delete-then-apply reconcile is the designed
// recovery, but before this hook it only ran on an explicit
// build/world_build, and the CPA that could ask for one is itself one of the
// dead pods. The console serve calls ReviveRebootedAgents at boot and keeps
// a slow periodic re-check for the process lifetime: pods whose last
// container termination is recorded at/after the k3s guest's current boot
// time are re-asserted through the same create-is-idempotent path every
// reconcile uses — whatever the exit code, because a boot-window clean exit
// is a casualty too (the harness starts before the relay/model gateway
// finish booting and exits 0 in seconds, which restartPolicy: Never then
// freezes forever), and so are wedged ContainerStatusUnknown terminations,
// whose FinishedAt the kubelet never records. A clean exit PREDATING the
// boot stays terminal — that is the intentional-exit case I5 protects
// (stopping an agent on purpose deletes the pod, which the revive never
// sees).

const (
	rebootProbeEvery   = 30 * time.Second
	rebootGiveUp       = 10 * time.Minute
	rebootRecheckEvery = 5 * time.Minute
)

type podTermination struct {
	ExitCode   int       `json:"exitCode"`
	FinishedAt time.Time `json:"finishedAt"`
}

type podContainerStatus struct {
	State struct {
		Running    *struct{}       `json:"running"`
		Terminated *podTermination `json:"terminated"`
	} `json:"state"`
	LastState struct {
		Terminated *podTermination `json:"terminated"`
	} `json:"lastState"`
}

type podItem struct {
	Metadata struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Status struct {
		ContainerStatuses []podContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type podList struct {
	Items []podItem `json:"items"`
}

// rebootKilled returns the deduped agent display names whose pod's last
// container termination matches a reboot casualty: a termination recorded
// at/after bootEpoch (the k3s guest's current boot time) — clean or not,
// because the harness exits 0 when its dependencies (relay, model gateway)
// are still booting — or a wedged ContainerStatusUnknown (non-clean exit
// with no FinishedAt; "could not be located when the pod was
// terminated/deleted" — undatable, and dead regardless of when it happened).
// A clean exit predating bootEpoch was already terminal before the reboot
// and stays as-is (I5). A container still running (or re-created by the
// kubelet) is alive; a waiting container falls back to its lastState
// termination record (the FailedMount wedge a reboot can leave).
func rebootKilled(pods podList, bootEpoch int64) []string {
	var dead []string
	seen := map[string]bool{}
	for _, p := range pods.Items {
		if p.Metadata.Labels["app.kubernetes.io/managed-by"] != "freehold" {
			continue
		}
		name := p.Metadata.Annotations["freehold.fh/agent-name"]
		if name == "" || seen[name] {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			term := cs.State.Terminated
			if term == nil && cs.State.Running == nil {
				term = cs.LastState.Terminated
			}
			if term == nil {
				continue
			}
			if term.FinishedAt.IsZero() {
				// The kubelet lost the container: no timestamp to date it
				// against the boot. A non-clean exit code is the wedge
				// signature; a clean one stays terminal (the conservative
				// default for an undatable exit).
				if term.ExitCode != 0 {
					dead = append(dead, name)
					seen[name] = true
				}
				break
			}
			if term.ExitCode == 0 && term.FinishedAt.Unix() < bootEpoch {
				continue // pre-boot clean exit stays terminal (I5)
			}
			if term.FinishedAt.Unix() >= bootEpoch {
				dead = append(dead, name)
				seen[name] = true
			}
			break
		}
	}
	return dead
}

// k3sBootEpoch reads the k3s guest's boot time as UTC epoch seconds (now
// minus /proc/uptime — no timezone parsing).
func (s *Spec) k3sBootEpoch() (int64, error) {
	out, err := s.execOut(
		fmt.Sprintf(`pct exec %d -- sh -c 'echo $(( $(date -u +%%s) - $(cut -d" " -f1 /proc/uptime | cut -d. -f1) ))'`, s.K3sVmid), 30)
	if err != nil {
		return 0, err
	}
	var boot int64
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &boot); err != nil {
		return 0, fmt.Errorf("boot time %q not an epoch: %w", strings.TrimSpace(out), err)
	}
	return boot, nil
}

// agentPodList reads the agents namespace's pod states through the
// co-located runner (the same exec path every build stage uses).
func (s *Spec) agentPodList() (podList, error) {
	var pods podList
	cmd := fmt.Sprintf(`pct exec %d -- sh -c "%s get pods -n agents -o json"`, s.K3sVmid, kubeKubectl())
	out, err := s.execOut(cmd, 120)
	if err != nil {
		return pods, err
	}
	if err := json.Unmarshal([]byte(out), &pods); err != nil {
		return pods, fmt.Errorf("parse kubectl pod list: %w", err)
	}
	return pods, nil
}

// errEngineBusy marks a probe skipped because a world-build holds the engine
// lock (cpbuild.go engineMu) — retried on the next probe, never counted as a
// failure.
var errEngineBusy = errors.New("world-build engine busy")

// reconcileRebootedAgents is one revive pass: read the node's boot time and
// the agent pods, re-assert every reboot-killed agent through the registry.
// Agents already alive or gone from the registry are skipped. Takes the
// engine lock non-blocking: a world-build mid-flight owns the registry rows
// this pass would write, so the pass skips and the probe loop retries.
func (s *Spec) reconcileRebootedAgents() error {
	if !engineMu.TryLock() {
		return errEngineBusy
	}
	defer engineMu.Unlock()
	if err := s.resolveK3sVmid("reboot-reconcile", ""); err != nil {
		return err
	}
	boot, err := s.k3sBootEpoch()
	if err != nil {
		return fmt.Errorf("read the k3s guest boot time: %w", err)
	}
	pods, err := s.agentPodList()
	if err != nil {
		return fmt.Errorf("read agent pods: %w", err)
	}
	dead := rebootKilled(pods, boot)
	if len(dead) == 0 {
		return nil
	}
	reg, err := agenttools.OpenRegistry(filepath.Join(s.agentToolsRoot(), "registry.json"))
	if err != nil {
		return fmt.Errorf("open agent registry: %w", err)
	}
	rows, err := reg.Agents()
	if err != nil {
		return fmt.Errorf("read agent registry: %w", err)
	}
	byName := map[string]console.AgentInfo{}
	for _, a := range rows {
		byName[a.Name] = a
	}
	tools := &agent.Tools{Console: reg, Create: BuildCreateAgentFn(s)}
	for _, name := range dead {
		row, ok := byName[name]
		if !ok {
			continue // retired between the pod list and now
		}
		if err := s.reassertAgentRow(reg, tools, row); err != nil {
			log.Printf("reboot-reconcile: revive %s FAILED (%v) — the next build/world_build retries", name, err)
			continue
		}
		log.Printf("reboot-reconcile: revived %s (pod terminal from the node reboot)", name)
	}
	return nil
}

// ReviveRebootedAgents probes fast through the boot window, then keeps a slow
// re-check ticker for the process lifetime. The boot pass alone is not enough
// for two reasons: a pass that succeeds while the API still serves the STALE
// pre-reboot pod statuses (the window between the apiserver answering and the
// fresh kubelet writing the terminations — seconds, and the 30s probe can
// land in it) sees zero dead agents and would never re-check; and a k3s-guest
// stop/start that does NOT take the CP down (no boot hook runs at all) needs
// a later pass to notice. The pass is idempotent and cheap (one boot-time
// read + one pod list), and a build holding the engine lock just skips a
// tick.
func (s *Spec) ReviveRebootedAgents() {
	deadline := time.Now().Add(rebootGiveUp)
	for {
		if err := s.reconcileRebootedAgents(); err != nil {
			if errors.Is(err, errEngineBusy) {
				time.Sleep(rebootProbeEvery)
				continue
			}
			if time.Now().After(deadline) {
				log.Printf("reboot-reconcile: gave up after %v (%v) — the periodic re-check keeps trying", rebootGiveUp, err)
				break
			}
			time.Sleep(rebootProbeEvery)
			continue
		}
		break
	}
	for range time.Tick(rebootRecheckEvery) {
		if err := s.reconcileRebootedAgents(); err != nil && !errors.Is(err, errEngineBusy) {
			log.Printf("reboot-reconcile: periodic pass failed (%v)", err)
		}
	}
}
