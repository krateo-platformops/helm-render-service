---
type: Example
title: /diff — upgrade impact between two chart versions
description: POST two inline chart versions to /diff and get added/removed/changed objects plus the field-level values-schema breakdown (valuesSchemaDiff).
resource: http://krateo-helm-render-service.krateo-system.svc:8080/diff
tags: [diff, upgrade-impact, values-schema]
timestamp: 2026-08-07T00:00:00Z
---

# `/diff`: upgrade impact

Diffs two versions of the same chart rendered with the same values. Both sides
here are **inline** chart trees (`files`), so the example is fully offline; in
real use `base`/`head` usually point at two OCI/repo versions of a published
chart. v0.2.0 of the demo chart adds a Service, extends the ConfigMap and
changes the values schema — so every part of the response has something to say.

## Preconditions

A reachable helm-render-service — same as
[render-inline](../render-inline/README.md): `go run .` locally, or the
in-cluster Service of a stock installer deploy.

## Run

```sh
curl -s localhost:8080/diff -X POST \
  -H 'Content-Type: application/json' --data @request.json
```

Expected response (`200`):

- `added`: the new `Service` `preview-svc`;
- `changed`: the `ConfigMap` `preview-cm` with
  `"summary": "changed top-level fields: data"`;
- `valuesSchemaChanged: true` plus the field-level breakdown
  `valuesSchemaDiff`: `added: ["replicas"]`, `nowRequired: ["replicas"]`,
  `changedDefaults: ["greeting"]` — what the portal shows the user before a
  gated version update.
