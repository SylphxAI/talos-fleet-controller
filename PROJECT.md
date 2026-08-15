# talos-fleet-controller

`talos-fleet-controller` is an active domain-service repository for
declarative Talos Linux node management through a Kubernetes controller. It
owns the `FleetNodeSet` CRD, reconciliation logic, Talos API integration,
Helm chart, raw deploy manifests, and image build workflow used to converge
Talos machine configuration and OS versions.

## Lifecycle And Layer

- Lifecycle: `active`
- Layer: `domain-service`

## Goals

- Provide a Kubernetes-native controller for Talos config convergence and OS
  upgrades.
- Keep `FleetNodeSet` API, controller behavior, generated manifests, deploy
  examples, Helm chart, and image build path coherent.
- Preserve day-2 safety controls such as drift detection, maintenance windows,
  dry-run, pause, health checks, and limited unavailability.

## Non-Goals

- Own Talos Linux, Omni, Cluster API provisioning, or bare-metal lifecycle
  outside the documented `FleetNodeSet` surface.
- Encode one cluster's site-specific topology, credentials, IP plan, or
  rollout policy as controller defaults.
- Publish enterprise doctrine, org rulesets, rollout issue reconciliation, or
  shared CI policy from this repository.

## Boundaries

This repository owns the TFC CRD and controller implementation. Consumers must
use the documented Kubernetes API, Helm chart, container image, or deploy
manifests, not internal controller packages. Cluster-specific policy belongs in
GitOps configuration or consumer manifests.

## Public Surfaces

- `README.md` describes the controller and `FleetNodeSet` usage.
- `config/crd/bases/fleet.talos.dev_fleetnodesets.yaml` is the generated CRD
  surface.
- `charts/talos-fleet-controller/` defines the Helm distribution surface.
- `deploy/` and `config/` provide raw deployment and sample manifests.
- `.github/workflows/*.yml` define build, lint, unit, and e2e CI.
-  is the machine-readable project manifest.

## Delivery

Pull requests and merge groups run Go lint, unit tests, e2e tests, binary
build, and image build checks. Main pushes and tags build and push a GHCR image.
Production proof for behavior changes requires CI plus controller/image
readback or a cluster smoke proving the affected `FleetNodeSet` reconcile path.
CRD and controller changes need forward-compatibility review because source
revert alone does not remove already-applied Kubernetes API or cluster state.

The authoritative control-plane record is .
