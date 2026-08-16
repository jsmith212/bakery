# Deploying Bakery on Kubernetes

The manifest set lives in `docs/deploy/k8s/`. Apply in this order:

```
kubectl apply -f secret.example.yaml   # or your own Secret named bakery-db
kubectl apply -f pvc.yaml
kubectl apply -f networkpolicy.yaml
kubectl apply -f service.yaml
kubectl apply -f service-metrics.yaml
kubectl apply -f deployment.yaml
kubectl apply -f servicemonitor.yaml   # optional, Prometheus Operator only
kubectl apply -f ingress.example.yaml  # example only -- adapt to your ingress
```

Every YAML in the set carries inline comments citing the invariant it
satisfies, the same discipline the migrations use. This document is the
prose that does not fit in a comment.

## `replicas: 1` and `strategy: Recreate` are mandatory, not tuning knobs

Bakery is a singleton by design. The boot advisory lock, the in-process
route cache, the blob LRU, and the GC's pending-read set are only coherent
if exactly one process is writing the database at a time. `replicas: 1`
makes that true; `strategy: Recreate` is what makes a *deploy* not break it.

Here is the trace for why the default `RollingUpdate` strategy does not just
degrade gracefully -- it **permanently wedges the rollout**, with no
self-resolution:

1. At `replicas: 1`, `RollingUpdate`'s defaults round to `maxSurge: 1,
   maxUnavailable: 0`. Kubernetes therefore creates the **new** pod first,
   before touching the old one.
2. The new pod starts and calls `AcquireBootLock`. The old pod still holds
   it -- it has not been sent SIGTERM yet, because `maxUnavailable: 0` means
   Kubernetes will not kill it until the new pod is Ready.
3. The new pod's `startupProbe` never turns green (the process is blocked
   on the lock, so the public listener never binds), so Kubernetes never
   considers the new pod Ready.
4. Kubernetes is waiting on the new pod to become Ready before it kills the
   old pod. The new pod is waiting on the old pod to release the lock,
   which only happens on termination. **Circular wait. Nothing resolves it.**
   The new pod sits in `CrashLoopBackOff` (once its startupProbe budget is
   exhausted) forever, and the deployment never converges.

`Recreate` breaks the cycle by sending SIGTERM to the old pod **first** and
waiting for it to terminate before creating the new one. The old pod's
`defer lock.Release()` fires during its ordinary graceful shutdown, and the
new pod acquires the lock cleanly on a database with no other writer.

**The cost, stated plainly: every deploy has a real availability gap** --
however long the old pod takes to shut down (bounded by
`terminationGracePeriodSeconds: 60` below) plus however long the new pod
takes to boot and pass its `startupProbe`. This is the accepted price of
the singleton design, not an oversight. If a zero-downtime deploy is a hard
requirement, that is a multi-replica program (an external cache for the
route cache and blob LRU, leader election for GC, a rethink of the
single-writer premise) -- a milestone, not a manifest change.

## `BOOT_LOCK_WAIT` helps two things and cannot help a third

`BOOT_LOCK_WAIT` (`--boot-lock-wait`, default `0s`) makes a booting instance
poll for the lock instead of failing immediately on the first `ErrLocked`.
It genuinely helps:

- a node drain or eviction that takes the old pod down outside the tidy
  SIGTERM sequence, where the lock is released a little late relative to
  the new pod's first attempt;
- an old pod that is slow to terminate but still within its grace period.

**It cannot rescue a deployment left on `RollingUpdate`.** Re-read step 2
above: the old pod is not going anywhere until the new pod is Ready, so the
new pod's wait is contending with a holder that is itself healthy and
waiting on the new pod. No finite `BOOT_LOCK_WAIT` resolves a circular
wait -- it only makes the eventual failure slower. `deployment.yaml` sets
`strategy: Recreate` for exactly this reason; `BOOT_LOCK_WAIT` is a
belt-and-suspenders timing cushion on top of it, not a substitute for it.
When the wait budget is exhausted with the lock still held, the boot error
names `RollingUpdate` as the likely cause so this is diagnosable from the
pod's own logs, not just from this document.

**Sizing `startupProbe` against `BOOT_LOCK_WAIT`:** the probe has to budget
the *whole* boot sequence, not just "the process started" -- see the next
section. Its total budget (`failureThreshold * periodSeconds`) must exceed
`BOOT_LOCK_WAIT` plus a normal boot. `deployment.yaml` ships
`BOOT_LOCK_WAIT=45s` against a `startupProbe` budget of `60 * 5s = 300s`,
which is wide margin; tighten both together if you need faster failure
detection, but keep the probe budget comfortably above the wait.

## Why `startupProbe` checks `/healthz`, not `/readyz`

The public listener does not bind until the **entire** boot sequence has
finished: connect and ping the database, take the boot lock (+ up to
`BOOT_LOCK_WAIT`), run migrations, open the serving pool, construct auth
(including OIDC discovery, bounded at 30s -- see below), storage, and every
M2-M5 cache backend. So the first successful `/healthz` is proof all of
that finished, not merely that the binary started. Once the listener is up,
`/readyz` (which really pings the database under a 2s bound) takes over as
the readiness signal for ordinary DB blips.

`livenessProbe` also checks `/healthz`, deliberately never `/readyz`: a
liveness check on `/readyz` would restart a perfectly healthy binary because
Postgres blinked, which is the wrong-direction conflation from checking the
same thing at the same layer twice for two different purposes.

## OIDC discovery is bounded at 30 seconds

`auth.NewProvider`'s discovery call (`GET
{issuer}/.well-known/openid-configuration`) is wrapped in a 30s
`context.WithTimeout`. Without it, an unreachable or black-holing identity
provider would hang `Boot` forever -- **before any listener binds** -- so no
probe could ever connect, and the pod would sit as an eternally-failing
`startupProbe` on a container that printed no error at all. The bound turns
a bad IdP into a fast, loud boot failure instead.

## `METRICS_ADDR=0.0.0.0:9090` and `GRPC_ADDR=0.0.0.0:9092` are required here, and that is not a violation

`internal/config`'s own comment says exposing `/metrics` "has to be an
explicit act," and the default bind is loopback. In this cluster,
rebinding to `0.0.0.0` is exactly that explicit act -- a Service cannot
reach a loopback-only port from a different pod's network namespace, so
`METRICS_ADDR=0.0.0.0:9090` is what makes the separate `bakery-metrics`
Service (and gRPC, `bakery`'s `grpc` port) work at all.

**The invariant this satisfies is listener separation, not bind interface**:
`/metrics` is served on its own port, its own listener, and reached through
its own Service (`service-metrics.yaml`), never merged onto the public
Service or an Ingress. What actually keeps it off-limits to everything but
Prometheus is `networkpolicy.yaml` -- **the Service is discovery, the
NetworkPolicy is the control.** A Service is a name and a load-balancing
rule; once the container listens on `0.0.0.0`, any pod that can resolve the
Service (or reach the pod IP directly) can hit the port with no Service or
Ingress in the way. See `networkpolicy.yaml`'s own comments for the
default-deny-then-narrow-allows shape.

### Set the node CIDR, or your probes are dropped

A default-deny ingress policy denies the **kubelet** too, and every one of
this Deployment's three probes (`startupProbe`, `readinessProbe`,
`livenessProbe`) is an `httpGet` on port 8080. Probes do not originate in a
namespace -- they come from the kubelet on the node, with the node's own IP
-- so a `namespaceSelector` cannot admit them and there is no selector that
can. `networkpolicy.yaml` therefore carries a fourth rule: an `ipBlock`
allowing the node CIDR to reach 8080. **Set it to your cluster's node
subnet.**

Whether you need it depends on your CNI, and the ones that enforce
host-to-pod traffic are the common ones: **Calico** (default felix config),
**Weave Net**, **Antrea** and **kube-router** all apply NetworkPolicy to
traffic arriving from the node, so on those the probes are dropped without
this rule. **Cilium** and the **AWS VPC CNI** do not enforce host-to-pod by
default, so the rule is a harmless no-op there. If you are unsure, keep it.

The failure is worth recognising because it mimics the one above: all three
probes fail, the `startupProbe` burns its full 60 x 5s budget, the container
is killed and restarts -- a `CrashLoopBackOff` with a healthy-looking
process, no application error, and nothing in the log. That is exactly what
a lost boot lock looks like, and it is the wrong place to start debugging.
`kubectl describe pod` naming `Startup probe failed: ... connection refused`
or a timeout, while `kubectl exec`/port-forward reaches `/healthz` fine, is
the tell.

Never route `/metrics` or the gRPC port through an Ingress. See
`ingress.example.yaml`'s comment.

## `STORAGE_DIR=/data`

The default (`./data`) resolves under `/` in the distroless final image (no
`WORKDIR` is set), which is unwritable under `readOnlyRootFilesystem: true`
-- and unwritable, full stop, even without that setting, since distroless
has no shell to have created a `./data` relative to whatever the container's
working directory happens to be. `deployment.yaml` sets
`STORAGE_DIR=/data` and mounts `pvc.yaml`'s `bakery-data` claim there. If
you switch to the S3 storage driver (`STORAGE_DRIVER=s3`), the PVC and this
env var are unnecessary and can be dropped from the manifest.

**If you do switch, read [`s3.md`](s3.md) first.** Two bucket lifecycle rules
are required operator setup, not optimizations: an expiry on the `staging/`
prefix, and `AbortIncompleteMultipartUpload`. Without them a pod killed
mid-upload leaves objects and multipart parts you are billed for indefinitely
and that nothing ever reads. The credentials come from the standard AWS chain
(on EKS: an IRSA service account, nothing in a Secret) -- there are no
credential flags -- and Bakery `HeadBucket`s at boot, so a misspelled bucket
fails the pod's startup rather than the first cache write.

## `DB_URL` is a required Secret with no default

`internal/config` gives `DB_URL` no default value; `Boot` fails fast without
it. `secret.example.yaml` documents the one key `deployment.yaml` reads via
`secretKeyRef` -- generate the real Secret with `kubectl create secret
generic bakery-db --from-literal=DB_URL=...` or your cluster's secret
manager, never by applying the example file verbatim.

## Alert on `boot lock lost`

`Lost()` on the boot lock only fires in the genuine two-writers case --
ordinary Postgres restarts and blips are absorbed by the lock's own
recovery logic and never reach it. **This log line must never fire in
normal operation, which is exactly why it is worth alerting on**: if it
does, this process is no longer the sole writer, its route cache, blob LRU
and GC pending-read set are all stale, and it is shutting itself down as
the only safe response. Wire a log-based alert on the line `"boot lock
lost: another instance now holds it"`.

During an actual Postgres outage, `/readyz` also fails and the pod goes
NotReady -- both signals degrade in the same direction, which is correct
and is not what this alert is for.
