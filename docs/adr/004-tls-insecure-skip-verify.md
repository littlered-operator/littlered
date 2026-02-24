# ADR 004: TLS Without Certificate Verification for Operator-to-Pod Connections

## Status
Accepted

## Context

LittleRed supports TLS-encrypted connections between the operator and Redis/Sentinel pods
(`Spec.TLS.Enabled`). When the Go client connects to a pod, the `crypto/tls` package can
either verify the server's certificate (standard TLS) or skip that check (`InsecureSkipVerify`).

The conventional rule is "always verify". This document explains why that rule does not apply
here, and what "skip verify" actually means in this context.

## What TLS verification does

TLS provides two things:

1. **Encryption** — data on the wire is unreadable to eavesdroppers.
2. **Authentication** — the client confirms it is talking to the intended server, not a
   man-in-the-middle (MITM), by verifying the server's certificate against a trusted CA.

`InsecureSkipVerify` disables (2) only. Encryption is unchanged.

## Why certificate-based authentication is redundant here

The operator connects to Redis and Sentinel using **pod IPs it resolved from the Kubernetes
API**, not from DNS or user input. This changes the trust model fundamentally.

For a MITM attack to succeed against cert-based verification, an attacker would need to:

1. Intercept TCP traffic to a specific, ephemeral pod IP inside the cluster network, **and**
2. Present a certificate that the operator trusts.

Intercepting traffic at the pod-IP level within a Kubernetes cluster requires compromising
the node's networking stack or the CNI plugin. At that point the attacker has cluster-admin
equivalent access — TLS cert verification is far below the severity of what was already
compromised.

**The trust anchor in this architecture is the Kubernetes API + RBAC**, not PKI:

- The K8s API tells the operator which pods exist and what their IPs are.
- RBAC ensures only the operator can query or modify that information.
- Pod networking ensures only the actual pod can receive traffic on its assigned IP.

Cert verification would be a redundant second assertion of something already established
with higher authority.

## Why verification would also fail in practice

Even if verification were desirable, it would not work with the current certificate shape.
TLS secrets in LittleRed are scoped to the **service hostname** (`<name>.<ns>.svc`), not to
individual pod IPs. The operator connects to pod IPs. TLS handshake verification would always
fail because the certificate's Subject Alternative Names do not include pod IPs.

Making verification work would require per-pod certificates with IP SANs — which means
cert-manager or equivalent infrastructure, plus graceful cert rotation in the operator. That
is a substantial commitment not warranted by the threat model above.

## Decision

The operator uses `InsecureSkipVerify: true` for all connections to Redis/Sentinel pods when
TLS is enabled. This provides **encryption in transit** (confidentiality against passive
eavesdroppers with cluster network access) without requiring PKI infrastructure that the
current deployment model does not support.

This is consistent with the `--tls --insecure` flags used in all pod startup scripts for
redis-cli calls.

## For users who need full PKI-based mutual authentication

If the threat model requires cryptographic identity verification of individual pods (e.g.,
regulated environments, zero-trust networks), the recommended path is a **service mesh**
(Istio, Linkerd, Cilium). These handle mTLS, per-workload certificate issuance, and cert
rotation transparently at the network layer, without requiring application-level configuration.

In that case `Spec.TLS.Enabled` can remain `false` (the service mesh handles encryption) or
can be set to `true` alongside the mesh for defence-in-depth.

## Consequences

### Positive
- No dependency on cert-manager or any external certificate authority.
- Users bring their own TLS secret (`Spec.TLS.ExistingSecret`) without needing IP SANs.
- Consistent with how redis-cli is invoked in startup and pre-stop scripts.

### Negative
- Passive eavesdroppers with access to cluster network traffic see encrypted data (good), but
  an active MITM at the pod-IP level would not be detected by the operator's TLS layer.
  This risk is accepted given the prerequisite level of cluster compromise required.

## References
- `internal/redis/client.go` — `makeTLSConfig()`
- ADR 001 — Strict IP-Only Identity (establishes why pod IPs are the identity anchor)
- `docs/USAGE.md` — TLS configuration guide
