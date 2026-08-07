---
type: Runbook
title: helm-render-service — release
description: How a release ships — one plain-semver tag drives the multi-arch image build and the OCI chart publish; what lands where and what to verify.
resource: oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service
tags: [release, ci, oci]
timestamp: 2026-08-07T00:00:00Z
---

# Release

One tag ships everything: the container image and the
`krateo-helm-render-service` chart publish from the same plain-semver tag (one
version line — the chart's `appVersion` resolves to the same tag).

## The runbook

1. **Merge to `main`** with PR CI green
   ([`release-pullrequest.yaml`](../.github/workflows/release-pullrequest.yaml):
   validate-only multi-platform image build + Go checks via the shared
   `krateo-platformops/.github` reusables, plus security scanning and docs
   lint).
2. **Tag with plain semver — `X.Y.Z`, no `v` prefix.** Both release workflows
   trigger on `[0-9]+.[0-9]+.[0-9]+` only; a `v`-prefixed tag ships **nothing**,
   silently (it has happened here: `v0.2.0` produced no artifacts; `0.3.0` is
   the release that followed).

   ```sh
   git tag 0.3.1 && git push origin 0.3.1
   ```

3. **CI builds and publishes**, no manual steps:
   - [`release-tag.yaml`](../.github/workflows/release-tag.yaml) → the shared
     `component-image-build` workflow builds the multi-arch
     (linux/amd64 + linux/arm64) image from the repo-root `Dockerfile` →
     `ghcr.io/krateo-platformops/helm-render-service:X.Y.Z`.
   - [`release-oci.yaml`](../.github/workflows/release-oci.yaml) (the
     canonical, byte-identical org workflow) discovers `chart/` as the repo's
     one first-class chart, substitutes the `Chart.yaml` placeholders
     (`CHART_VERSION` → the tag; `APP_VERSION` → the latest app semver tag —
     the same tag, since app and chart live in one repo), packages, and pushes
     → `oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service:X.Y.Z`.
   - The `testdata/charts/` fixtures are **not** published: they store their
     manifest as `Chart.yaml.tpl`, invisible to the workflow's
     `find . -name Chart.yaml` discovery (the tests rename it in memory).

4. **Verify** the artifacts exist and pair up:

   ```sh
   helm show chart oci://ghcr.io/krateo-platformops/charts/krateo-helm-render-service --version X.Y.Z
   # appVersion in the output must equal X.Y.Z
   ```

5. **Roll it out** by bumping the Krateo installer's
   `krateo-helm-render-service` component pin, or `helm upgrade` on a
   standalone install ([usage](./usage.md)).

## Docs

`docs/llms.txt` pins this bundle to the release tag — update the pin (and
[log](./log.md), when the release is notable) as part of the release PR.
