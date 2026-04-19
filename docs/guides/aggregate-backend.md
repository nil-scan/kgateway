# Aggregate Backend

An **aggregate backend** lets a single logical cluster reference an ordered priority list of underlying Envoy clusters. Envoy routes traffic to the highest-priority available cluster and falls back to lower-priority ones on failure. This maps to Envoy's [`envoy.clusters.aggregate`](https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/clusters/aggregate/v3/cluster.proto) extension.

## When to use this

Use an aggregate backend when you need:

- **Ordered failover** between two or more upstreams (e.g., primary/secondary regions, canary -> stable).
- **Mixed backends** — an aggregate can contain both kgateway `Backend` resources and Kubernetes `Service` resources as members.

## How it works

1. Create the individual backends/services that will act as cluster members.
2. Create a `Backend` resource with `type: Aggregate` referencing those members by name, in priority order.
3. Reference the aggregate `Backend` from an `HTTPRoute`.

kgateway resolves each member reference to its Envoy cluster name and emits an `envoy.clusters.aggregate` cluster. Envoy picks the first healthy cluster in the list.

## Example: two Backend members

```yaml
apiVersion: gateway.kgateway.dev/v1alpha1
kind: Backend
metadata:
  name: primary
  namespace: default
spec:
  type: Static
  static:
    hosts:
      - host: primary.example.com
        port: 8080
---
apiVersion: gateway.kgateway.dev/v1alpha1
kind: Backend
metadata:
  name: secondary
  namespace: default
spec:
  type: Static
  static:
    hosts:
      - host: secondary.example.com
        port: 8080
---
apiVersion: gateway.kgateway.dev/v1alpha1
kind: Backend
metadata:
  name: failover
  namespace: default
spec:
  type: Aggregate
  aggregate:
    members:
      - backendRef:
          group: gateway.kgateway.dev
          kind: Backend
          name: primary
      - backendRef:
          group: gateway.kgateway.dev
          kind: Backend
          name: secondary
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: my-route
  namespace: default
spec:
  parentRefs:
    - name: my-gateway
  rules:
    - backendRefs:
        - group: gateway.kgateway.dev
          kind: Backend
          name: failover
          port: 80
```

## Example: two Service members

Members may also be plain Kubernetes `Service` resources. Omit `group` and `kind` (or set them to `""` / `"Service"`) and include a `port`:

```yaml
apiVersion: gateway.kgateway.dev/v1alpha1
kind: Backend
metadata:
  name: failover-svc
  namespace: default
spec:
  type: Aggregate
  aggregate:
    members:
      - backendRef:
          name: primary-svc
          port: 8080
      - backendRef:
          name: secondary-svc
          port: 8080
```

## Cross-namespace members

A member in a different namespace requires a `ReferenceGrant` in the **target** namespace allowing the source aggregate Backend to reference it:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-aggregate
  namespace: other          # target namespace
spec:
  from:
    - group: gateway.kgateway.dev
      kind: Backend
      namespace: default    # namespace of the aggregate Backend
  to:
    - group: gateway.kgateway.dev
      kind: Backend
---
apiVersion: gateway.kgateway.dev/v1alpha1
kind: Backend
metadata:
  name: failover-xns
  namespace: default
spec:
  type: Aggregate
  aggregate:
    members:
      - backendRef:
          group: gateway.kgateway.dev
          kind: Backend
          name: local-backend
      - backendRef:
          group: gateway.kgateway.dev
          kind: Backend
          name: remote-backend
          namespace: other
```

## Field reference

```yaml
spec:
  type: Aggregate
  aggregate:
    members:                 # required; 1-16 entries, evaluated in priority order
      - backendRef:
          group: <string>    # "gateway.kgateway.dev" for Backend, "" for Service
          kind: <string>     # "Backend" or "Service" (default: "Service")
          name: <string>     # required
          namespace: <string> # optional; defaults to the aggregate Backend's namespace
          port: <int>        # required for Service members; unused for Backend members
```

## Status

If a member reference cannot be resolved (resource not found, missing `ReferenceGrant`, or self-reference), the aggregate Backend's `Accepted` condition is set to `False` and the error is reported in the condition message.

## Caveats

- **Self-references are rejected**: an aggregate Backend cannot list itself as a member.
- **Cluster-level policies** (circuit breakers, outlier detection via `BackendConfigPolicy`) apply to the aggregate cluster itself and are respected. Per-endpoint settings (health checks, DNS resolver) have no effect on aggregate clusters.
- **Retries and timeouts** configured on the `HTTPRoute` apply at the aggregate level; Envoy handles per-cluster failover internally.
- **Stateful sessions** (sticky routing) are incompatible with aggregate clusters because Envoy's `CLUSTER_PROVIDED` lb policy does not support session affinity overrides.
