# `HyperFleetConfig` reference

`HyperFleetConfig` is the complete partner-facing configuration and
installation-health API for the HyperFleet Operator. This document describes
the current `hyperfleet.redhat.com/v1alpha1` schema and its implemented behavior.
The generated CRD remains the admission-time source of truth.

## Contents

- [Resource identity](#resource-identity)
- [Example](#example)
- [Specification](#specification)
  - [TLS port advertisement](#tls-port-advertisement)
  - [Referenced Secret contracts](#referenced-secret-contracts)
  - [Identity providers](#identity-providers)
  - [Tenant dimensions](#tenant-dimensions)
  - [Current sizing limitation](#current-sizing-limitation)
- [Status reference](#status-reference)
  - [Operator conditions versus API resource conditions](#operator-conditions-versus-api-resource-conditions)
  - [Operator condition types](#operator-condition-types)
  - [Condition reasons](#condition-reasons)

## Resource identity

| Property | Value |
|---|---|
| API version | `hyperfleet.redhat.com/v1alpha1` |
| Kind | `HyperFleetConfig` |
| Scope | Cluster |
| Short name | `hfc` |
| Required name | `cluster` |
| Status subresource | Enabled |

The object has no `metadata.namespace`. Its CEL validation fixes the name to
`cluster`, making it a cluster-scoped singleton.

## Example

```yaml
apiVersion: hyperfleet.redhat.com/v1alpha1
kind: HyperFleetConfig
metadata:
  name: cluster
spec:
  bundle: cloud-capi
  api:
    database:
      secretRef:
        name: hyperfleet-db
    tls:
      secretRef:
        name: hyperfleet-api-tls
    profile: small
  authentication:
    audience: hyperfleet-api
    identityProviders:
      - issuer: https://idcs-example.identity.oraclecloud.com
        tenantDimensions:
          tenancy_ocid:
            claim: tenancy_ocid
      - issuer: https://idcs-example-2.identity.oraclecloud.com
        identityClaim: preferred_username
        tenantDimensions:
          tenancy_ocid:
            value: ocid1.tenancy.oc1..aaaa
          compartment_id:
            claim: compartment_id
```

All referenced Secrets belong in the operator's namespace, not in a namespace
on the CR. The default operator namespace is `hyperfleet-system`, but deployment
configuration can change it.

## Specification

| Field | Required | Default | Validation and behavior |
|---|---:|---|---|
| `spec.bundle` | Yes | None | Immutable. Accepted values are `cloud-capi` and `onprem-agent`. `onprem-agent` is a placeholder. The operator cannot resolve it yet, and reconciliation fails. Use `cloud-capi`. |
| `spec.api` | Yes | None | Partner-facing HyperFleet API configuration. |
| `spec.api.database` | Yes | None | External PostgreSQL configuration; the operator does not provision a database. |
| `spec.api.database.secretRef` | Yes | None | Name-only reference to the database Secret in the operator namespace. |
| `spec.api.database.secretRef.name` | Yes | None | DNS-1123 subdomain, 1 to 253 characters. |
| `spec.api.tls` | No | No `server.tls` block; API serves plain HTTP | Optional API-serving TLS configuration. See [TLS port advertisement](#tls-port-advertisement). |
| `spec.api.tls.secretRef` | Conditional | None | Required when `tls` is present; name-only reference in the operator namespace. |
| `spec.api.tls.secretRef.name` | Conditional | None | DNS-1123 subdomain, 1 to 253 characters. |
| `spec.api.profile` | No | `small` | Mutable. Accepted values are `small`, `medium`, and `large`. See [current sizing limitation](#current-sizing-limitation). |
| `spec.authentication` | Yes | None | Default audience and identity providers. No `enabled` switch. See [identity providers](#identity-providers). |
| `spec.authentication.audience` | No | `hyperfleet-api` | Default audience, 1 to 253 chars when set. Defaults to `hyperfleet-api`. |
| `spec.authentication.identityProviders` | Yes | None | Map-list keyed by `issuer`, 1 to 64 entries, duplicates rejected. |
| `spec.authentication.identityProviders[].issuer` | Yes | None | HTTPS URL, host, no `?`/`#`/trailing `/`, 1 to 2048 chars. |
| `spec.authentication.identityProviders[].identityClaim` | No | `sub` | Claim naming caller, `^[A-Za-z0-9._-]+$`, 1 to 128 chars. |
| `spec.authentication.identityProviders[].tenantDimensions` | No | Bundle preset | Per-dimension source: exactly one of `claim` or `value`. A dimension not listed uses the bundle preset, which trusts the token claim of the same name; see [tenant dimensions](#tenant-dimensions). Dimension name `^[A-Za-z0-9._-]{1,63}$`. |
| `spec.authentication.identityProviders[].tenantDimensions[dim].claim` | Conditional | None | Claim name, `^[A-Za-z0-9._-]+$`, 1 to 256 chars. |
| `spec.authentication.identityProviders[].tenantDimensions[dim].value` | Conditional | None | Fixed value, `^[A-Za-z0-9._-]+$`, 1 to 255 chars. |

### TLS port advertisement

When `spec.api.tls` is set, the API listener serves HTTPS and the rendered
operands advertise that: the API container port and the Service port are named
`https` (instead of `http`) and the Service sets `appProtocol: https`, so
Kubernetes-aware proxies and service meshes route to the listener as TLS.
Clients must call the API Service with `https://` in that case; a plaintext
request fails with `Client sent an HTTP request to an HTTPS server`. With
`spec.api.tls` unset, the ports keep their `http` names and no `appProtocol` is
set, leaving plain-HTTP routing unchanged.

Certificate hostname requirements: the Sentinels reach the API at its
in-cluster Service DNS name, so the certificate in the `tls.secretRef` Secret
must be valid for `hyperfleet-api.<operator-namespace>.svc`. A certificate
whose SANs do not include that name fails TLS verification in the Sentinel
clients.

Known limitation (see HYPERFLEET-1678): the API certificate is commonly issued
by a private CA (for example, cert-manager). The Sentinels currently have no
way to load and trust a custom CA, so with a private-CA-issued certificate the
Sentinel-to-API HTTPS connection fails certificate verification. Until
Sentinel support for trusting a custom CA lands and is wired into the
operator, `spec.api.tls` with a private CA is usable only for clients that
already trust that CA.

### Referenced Secret contracts

| Reference | Required Secret type | Required keys | Notes |
|---|---|---|---|
| `database.secretRef` | No type requirement | `db.host`, `db.port`, `db.name`, `db.user`, `db.password` | Always referenced by the rendered API Deployment. |
| `tls.secretRef` | `kubernetes.io/tls` | `tls.crt`, `tls.key` | Used when `spec.api.tls` is present. |

The schema validates reference names, but it cannot validate another object's
namespace, existence, type, or keys. The reconciler resolves references in the
operator namespace. A missing referenced Secret produces
`Degraded=True/ReferencedSecretMissing`; the desired Deployment still contains
the reference and can recover when the Secret is created.

Creating, updating, or deleting a referenced Secret triggers a later reconcile.
The operator rolls the API pods when a referenced Secret changes so they use
current credential or certificate material, without copying Secret values into
rendered configuration.

### Identity providers

`spec.authentication` is required — there is no way to turn authentication
off. It declares the default audience and the accepted identity providers:

- `audience` is the token audience. It defaults to `hyperfleet-api` when
  omitted. Today it controls the ServiceAccount token projected for the
  operator-managed components that call the API.
- `identityProviders` lists the accepted issuers (1-64, keyed by `issuer`).
  For each tenant dimension, set exactly one of `claim` (JWT claim name) or
  `value` (fixed string); dimensions an entry does not list fall back to the
  bundle preset, which reads the token's claim as is (see
  [tenant dimensions](#tenant-dimensions)).

These fields are validated at admission but not enforced yet: the API itself
runs without JWT validation, so listing an identity provider does not yet
authenticate API requests. HYPERFLEET-1534 consumes this configuration and
turns enforcement on.

### Tenant dimensions

A tenant dimension is one part of the tenant the API scopes a caller to. The
bundle decides which dimensions exist, and the gateway copies each one into the
signed token the API trusts:

| Bundle | Dimension key | Preset claim | Required |
|---|---|---|---|
| `cloud-capi` | `tenancy_ocid` | `tenancy_ocid` | Yes |
| `cloud-capi` | `compartment_id` | `compartment_id` | No |
| `onprem-agent` | `org` | `org_id` | Yes |
| `onprem-agent` | `project` | `project_id` | No |

The presets are defined in `internal/tenant/tenant.go`; the table must match it.

Each provider gets its value for a dimension from, in order:

1. `value`: the fixed string you set. A token cannot override it, even if it
   carries a claim of the same name.
2. `claim`: the token claim you name. A name other than the preset claim is
   mapped onto it.
3. **Nothing listed: the bundle preset.** The gateway takes the token's claim
   named in the *Preset claim* column as is. **The operator does not check or
   fix that value**; whoever can put that claim into a token issued by this
   provider chooses the tenant.

A missing required claim makes the gateway reject the request for human
callers. A missing optional claim leaves that dimension empty, so the API scopes
that caller by its remaining dimensions only and it matches resources in every
value of the missing one. That broader visibility is a supported capability
([ADR-0027](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0027-tenant-dimension-cardinality.md)),
so only let a provider omit an optional claim if it may grant that wider scope.

A dimension claim must be a single string. If the token carries anything else
for it, such as an array from a groups claim mapped onto a dimension, the
gateway denies the request: there is no safe way to pick one value. Map a
single-valued claim, or set `value`.

Trusting the preset claim is right only when the provider's administrators
cannot set it themselves, for example a claim the identity service fills in.
For an identity provider whose claims you do not control, set `value` for
**every** dimension you care about, at least the required one:

```yaml
identityProviders:
  - issuer: https://untrusted.idp.example.com
    tenantDimensions:
      tenancy_ocid:
        value: tenant-a   # fixed; the token's tenancy_ocid is ignored
```

A dimension key that is not in the table for the bundle is an error when the
gateway configuration is rendered, so a misspelled key cannot silently fall back
to the preset.

### Current sizing limitation

The schema accepts `small`, `medium`, and `large`, defaults to `small`, and
allows the field to change. The current API renderer does not branch on that
value: all three profiles render one replica with the same requests
(`100m` CPU, `128Mi` memory) and limits (`500m` CPU, `512Mi` memory), with no
profile-specific HPA or PDB. The field currently expresses future sizing
intent, not a sizing guarantee.

## Status reference

`status` contains:

| Field | Meaning |
|---|---|
| `status.observedGeneration` | Latest `.metadata.generation` for which the operator completed component-health collection and status rollup. |
| `status.conditions` | Map-like list keyed by condition `type`, using Kubernetes `metav1.Condition`. Carries the aggregated health of all bundle components. |
| `status.components` | Map-like list keyed by component `name`, carrying the individual conditions observed for each bundle component. |

### Operator conditions versus API resource conditions

There are two independent condition layers:

- The **operator layer** is `HyperFleetConfig.status.conditions`. It answers
  whether the operator installed and is maintaining the selected operands. Its
  types are `Available`, `Progressing`, and `Degraded`, following the relevant
  OpenShift `ClusterOperator` convention from
  [ADR-0019](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0019-package-hyperfleet-as-operator.md).
- The **HyperFleet API layer** is status on resources managed through the API.
  Its resource-condition vocabulary is non-exhaustive: fixed types include
  `Available`, `Health`, `Reconciled`, `Finalized`, and
  `LastKnownReconciled`, plus per-adapter `<Adapter>Successful` conditions and
  mapped conditions when applicable. An API resource's `Available` condition is
  distinct from the operator's `HyperFleetConfig` `Available` condition. See
  [ADR-0007](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0007-conditions-based-status-model.md)
  and
  [ADR-0008](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0008-dynamic-status-aggregation.md).

An operator `Available=True` does not assert that partner-managed resources are
reconciled. Conversely, an API resource's success does not establish that the
operator can maintain every operand.

### Operator condition types

| Type | True means | False means |
|---|---|---|
| `Available` | Every component reports available. For the current API component, all desired Deployment replicas are available. | At least one component is missing, unavailable, or not fully ready. |
| `Progressing` | At least one component is rolling out. | All observed components have completed rollout. |
| `Degraded` | The latest reconcile found a missing referenced Secret or another reconciliation error. | No failure signal was found. |

For the API component, `/readyz` requires a working database connection.
Consequently, `Available=True` implies the API Deployment has ready replicas
whose readiness checks can reach the configured PostgreSQL database; it is
stronger than the `/healthz` liveness check.

### Condition reasons

Every operator condition write uses one of these constants. Treat these strings
as published vocabulary rather than free-form text.

| Condition | Status | Reason | Meaning |
|---|---|---|---|
| `Available` | `True` | `DeploymentAvailable` | All components are available; the API Deployment reports all desired replicas available. |
| `Available` | `False` | `DeploymentUnavailable` | An operand Deployment is absent or has zero available replicas. |
| `Available` | `False` | `DeploymentNotReady` | A Deployment has some, but not all, desired replicas available. |
| `Progressing` | `True` | `RolloutInProgress` | An operand Deployment has not completed rollout of its current generation. |
| `Progressing` | `False` | `RolloutComplete` | All observed operand Deployments are fully rolled out and stable. |
| `Degraded` | `False` | `AsExpected` | No reconciliation failure was detected. |
| `Degraded` | `True` | `ReferencedSecretMissing` | A database or TLS Secret reference does not resolve in the operator namespace. |
| `Degraded` | `True` | `ReconcileError` | Any other error prevented the latest reconcile from reaching or maintaining the desired state. |

Each condition's `observedGeneration` is the CR generation against which that
specific condition was evaluated. `lastTransitionTime` changes only when the
condition's status changes, not on every reconcile.

If reconciliation fails before component health can be checked, the operator
does not invent new `Available` or `Progressing` values and does not advance the
top-level `status.observedGeneration`. It re-evaluates only `Degraded` for the
new generation, preserving the last actually observed component health.

### Per-component conditions

`status.components` carries the conditions of each bundle component under its
name (e.g. `api` or `sentinel-clusters`), using the same `metav1.Condition`
type and the same reason vocabulary as the top-level conditions, but scoped to
that single component. Recognized types are `Available` and `Progressing`;
`Degraded` is operator-level only, since components cannot observe
controller-level failures such as a missing referenced Secret.

The list is sorted by component name and rebuilt on every successful reconcile:
an entry is removed when its component disappears from the bundle, and a
condition type no longer reported by a component is dropped from its entry.
Each condition's `observedGeneration` and `lastTransitionTime` follow the same
semantics as the top-level conditions. Like the top-level `Available` and
`Progressing`, per-component entries are left untouched when reconciliation
fails before component health is collected. Consequently, a per-component
condition's `observedGeneration` can be behind the current
`.metadata.generation` after a failed reconcile — it is always in lockstep with
`status.observedGeneration`, since both are updated together on a reconcile
that collects component health. It catches up on the next reconcile that
successfully collects that component's health.
