# Talos Fleet Controller (TFC)

<p align="center">
  <img src="https://mark.sylphx.com/api/v1/banner?type=void&theme=tokyonight&text=talos+fleet+controller&desc=Declarative+node+management+for+Talos+Linux+%E2%80%94+config+convergence+%2B+OS+upgrades+v&height=200&animation=rise&credit=0" alt="talos-fleet-controller — Sylphx Mark banner" width="100%" />
</p>

**Declarative node management for Talos Linux** — config convergence + OS upgrades via Kubernetes CRD.

> The missing GitOps controller for Talos Linux. Detect drift, converge config, roll upgrades — all declarative.

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

## The Problem

Talos Linux provides powerful APIs (`apply-config`, `upgrade`), but no open-source controller automates day-2 operations declaratively. You're left with:

- Ad-hoc `talosctl patch` commands — imperative, no audit trail
- Shell scripts that aren't GitOps
- CAPI rolling updates that destroy and rebuild bare metal servers
- Snowflake nodes with drifted configs

SideroLabs [archived their controller](https://github.com/siderolabs/talos-controller-manager) to push [Omni](https://www.siderolabs.com/platform/saas-for-kubernetes/) (commercial). Community tools handle pieces — [Tuppr](https://github.com/home-operations/tuppr) does upgrades only, [talhelper](https://github.com/budimanjojo/talhelper) generates configs but doesn't apply them.

**TFC fills this gap.** One controller. Config + upgrades. Declarative.

## What TFC Does

```
Day 0: CAPI / manual    →  Provision bare metal, node joins cluster
Day 2: TFC (this)       →  Converge config + OS version to desired state
```

One CRD — `FleetNodeSet` — declares both the desired **machine config** AND **Talos version** for a set of nodes. TFC continuously reconciles actual state to match, with safety guarantees.

## Quick Start

```yaml
apiVersion: fleet.talos.dev/v1alpha1
kind: FleetNodeSet
metadata:
  name: control-plane
  namespace: tfc-system
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/control-plane: ""

  talos:
    version: v1.12.5
    schematic: 3da7f440f279...

  machineConfig:
    inline: |
      machine:
        kernel:
          modules:
            - name: dm_thin_pool
        kubelet:
          extraArgs:
            max-pods: "250"
        nodeLabels:
          topolvm.io/node: "true"
      cluster:
        controlPlane:
          endpoint: https://10.10.0.240:6443

  nodeOverrides:
    - nodeSelector:
        matchLabels:
          kubernetes.io/hostname: cp-node-1
      machineConfig:
        inline: |
          machine:
            network:
              hostname: cp-node-1
              interfaces:
                - deviceSelector:
                    hardwareAddr: "aa:bb:cc:dd:ee:ff"

  updateStrategy:
    maxUnavailable: 1
    configApplyMode: auto
```

```bash
kubectl apply -f fleetnodeset.yaml
kubectl get fleetnodesets

# NAME            PHASE    TOTAL   SYNCED   PROGRESSING   TALOS     AGE
# control-plane   Synced   3       3        0             v1.12.5   5m
```

## Features

- **Unified** — Config changes (`apply-config`) + OS upgrades (`upgrade`) in one controller
- **Safe** — etcd quorum protection, workload drain with PDB respect, health checks
- **Sequential** — One node at a time, configurable `maxUnavailable`
- **Drift detection** — Continuously compares desired vs actual config hashes
- **Maintenance windows** — Cron-based scheduling for update operations
- **Emergency brake** — `spec.paused: true` stops all updates instantly
- **Dry-run** — `spec.dryRun: true` reports drift without applying changes
- **Per-node overrides** — Handle node-specific fields (IP, MAC, hostname)
- **K8s native** — CRD + controller-runtime, standard `kubectl` UX

## Architecture

```
FleetNodeSet CR  →  TFC Controller  →  Talos gRPC API  →  Nodes
    (Git)            (reconcile)       (apply/upgrade)
```

TFC runs as a Deployment inside the cluster. It accesses the Talos API via [`kubernetesTalosAPIAccess`](https://docs.siderolabs.com/kubernetes-guides/advanced-guides/talos-api-access-from-k8s) with `os:admin` role — no external `talosctl` binary or SSH needed.

### Reconcile Loop

```
1. List nodes matching nodeSelector
2. For each node: get actual version + config, diff against desired
3. If drift detected:
   a. Check maintenance window + maxUnavailable
   b. Pre-flight: etcd quorum check (for CPs)
   c. Cordon → Drain → Apply/Upgrade → Health check → Uncordon
4. Report status per node + aggregate
```

## Requirements

- Kubernetes 1.30+
- Talos Linux 1.9+ (live config apply support)
- `kubernetesTalosAPIAccess` enabled with `os:admin` role

## Status

🚧 **Under active development.**

### What ships today

The current `main` binary is a **Cluster API Runtime SDK extension server**
(see `cmd/main.go`): it serves the in-place update hooks — `CanUpdateMachine`,
`CanUpdateMachineSet`, `UpdateMachine` — over TLS (default port 9443) and
applies Talos machine-config changes via the Talos API instead of
reprovisioning machines. Its only flags are `--webhook-port`,
`--webhook-cert-dir`, `--profiler-address`, and logging flags; there is no
leader election, metrics endpoint, or HTTP health endpoint.

The **FleetNodeSet reconciler described above is not in the binary yet.** The
CRD ships with the Helm chart as declarative intent for the future
convergence feature; FleetNodeSet resources are stored but not acted upon.
Deployment details (serving certs, probes, CAPI `ExtensionConfig`
registration) live in
[`charts/talos-fleet-controller/README.md`](charts/talos-fleet-controller/README.md).

| Version | Scope | Status |
|---------|-------|--------|
| v0.1.0 | Config apply + drift detection + sequential updates | 🔨 Building |
| v0.2.0 | OS upgrades + etcd quorum protection | 📋 Planned |
| v0.3.0 | Maintenance windows + dry-run + pause | 📋 Planned |
| v0.4.0 | Prometheus metrics + webhook validation | 📋 Planned |
| v1.0.0 | Production-ready, full test coverage, Helm chart | 📋 Planned |

## License

Apache License 2.0 — see [LICENSE](LICENSE).

## Contributing

Contributions welcome! Please open an issue first to discuss before submitting large PRs.

Built by [SylphxAI](https://github.com/SylphxAI) — dogfooded on our own production bare metal cluster.
