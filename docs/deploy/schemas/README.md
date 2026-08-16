# Vendored kubeconform schemas

`just k8s-check` validates `docs/deploy/k8s/*.yaml` against these files, not
against the network.

## Why they are committed

kubeconform's default `-schema-location` is
`https://raw.githubusercontent.com/yannh/kubernetes-json-schema/...`, fetched
**per resource, per run**. `k8s-check` is part of `just check` -- the fastest and
most frequently run recipe, and the CI job that gates `build`, `test-db` and
every conformance job. Left on the default, `just check` hard-fails with no
network (where `go build` succeeds from the module cache), and a GitHub blip reds
the gate for a lint failure nobody introduced.

The recipe's own comment used to claim otherwise. It was true of the kubeconform
BINARY -- it resolves through the module proxy like every other Go dependency,
via go.mod's `tool` directive -- and false of the schemas it fetches.

## What is here, and how to add to it

One file per (kind, group, version) shipped under `docs/deploy/k8s/`, in the
layout kubeconform's template expects:

    {{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json

Adding a manifest of a new kind means adding its schema here, or `k8s-check`
fails -- deliberately. The gate asserts a **positive** valid count and a skip
budget of exactly one, so a manifest that silently validates against nothing is
a failure rather than a green tick.

The one tolerated skip is `servicemonitor.yaml`: `monitoring.coreos.com/v1` is a
Prometheus Operator CRD, not a Kubernetes builtin, and its own controller
validates it at apply time.

Source: <https://github.com/yannh/kubernetes-json-schema>, `master` branch,
`v1.31.0-standalone-strict/`. To refresh (or to move to a new Kubernetes
version), create the matching directory and re-download the same file names --
and bump `-kubernetes-version` in the `k8s-check` recipe in the same commit, or
every resource becomes a skip and the gate says so.
