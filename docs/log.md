---
type: Log
title: helm-render-service — log
description: Curated chronological history — notable changes, decisions and incidents; release notes stay in GitHub Releases.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [history]
timestamp: 2026-08-07T00:00:00Z
---

# Log

Curated history, newest first.

## 2026-08-07 — adopted the Krateo Documentation Standard

This bundle: `docs/` + `examples/` + thin README. The old single-README docs
had drifted from the code in three places, all re-grounded: the `/diff`
response contract omitted `valuesSchemaDiff` (shipped in 0.1.2, never
documented); the release section named the retired bespoke
`release-image.yaml` (amd64-only) instead of the shared multi-arch
`release-tag.yaml`; and the install snippet's release name
(`helm-render-service`) produced a Service named
`helm-render-service-krateo-helm-render-service`, so its own verify URL and the
illustrated Endpoint `server-url` pointed at a Service that doesn't exist. The
Makefile's dead-org image default (`ghcr.io/braghettos/…`) was re-pointed too.
The two request-body examples were validated against a live local build.

## 2026-08-05 — CI folded into the shared reusables (#10)

The bespoke `ci.yaml` and `release-image.yaml` were dropped: PR CI is now the
shared `component-image-build` (validate-only) + `component-go-checks`, and the
tag release publishes the image via the same shared build — one source of
truth, no per-repo drift.

## 2026-08-03/04 — 0.3.0: org migration

Go module identity moved to `github.com/krateo-platformops/helm-render-service`
(full independence from the personal-account era), the chart/image/URL
references were re-pointed to the org (a few missed in the repo transfer,
caught in #9), and the chart gained `global.imageRegistry` for mirror /
air-gapped installs. Lesson re-paid en route: the migration was first tagged
`v0.2.0` — the `v` prefix matches no release-workflow trigger, so it shipped
nothing; `0.3.0` is the release that followed. Tag without `v`, always
([release](./release.md)).

## 2026-07-23 — 0.1.7: multi-arch images (#8)

The image build went multi-platform (linux/amd64 + linux/arm64).

## 2026-07-13/14 — 0.1.0 → 0.1.6: the ship week

The v0 service (render core, snowplow-facing `/render` contract, guardrails)
shipped with its chart and snowplow Endpoint wiring, then hardened release by
release: numeric `runAsUser` for distroless nonroot — kubelet can't verify
`runAsNonRoot` on a non-numeric user, `CreateContainerConfigError` otherwise
(0.1.1); the field-level `valuesSchemaDiff` in `POST /diff` (0.1.2); the chart
renamed to `krateo-helm-render-service` per the org convention **and** the test
fixture charts hidden from the release workflow's chart discovery via
`Chart.yaml.tpl` — they were being published as `charts/demo` (0.1.3/0.1.4);
`x-kubernetes-int-or-string` on the numeric caps so CRD generation survives the
union type, and `snowplowEndpoint.enabled` defaulted `true` in the **schema**
too, because the CDC applies schema defaults, not `values.yaml` (0.1.5/0.1.6).
