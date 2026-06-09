# Development Guide

This document covers building LittleRed from source, working with a custom registry, and running the full test suite locally.

## Prerequisites

- **Go** 1.24+
- **Podman** (or Docker — set `CONTAINER_TOOL=docker`)
- **Helm** 3.13+
- **kubectl** 1.28+
- **Kind** (for local e2e testing)
- **make**

## Building

Build the operator and chaos-client binaries:

```bash
make build
```

Build container images (without pushing):

```bash
make build-images
```

## Using a Custom Registry

All registry-aware targets accept `LITTLERED_REGISTRY` as an override:

```bash
export LITTLERED_REGISTRY=registry.example.com/myorg

make images           # build + push both images
make helm-push        # package + push Helm chart to registry.example.com/myorg/charts/littlered
make push-latest      # re-tag as :latest (only runs on release tags vX.Y.Z)
```

The images land at:

```
registry.example.com/myorg/littlered:<tag>
registry.example.com/myorg/littlered-chaos-client:<tag>
```

The Helm chart lands at:

```
oci://registry.example.com/myorg/charts/littlered:<version>
```

### Authenticate before pushing

```bash
podman login registry.example.com
helm registry login registry.example.com
```

`podman` and Helm maintain separate credential stores, so both logins are needed.

## Local Development with Kind

### 1. Create a Kind cluster

```bash
make setup-test-e2e
```

This creates a Kind cluster named `littlered-test-e2e`. Skip if you already have one:

```bash
SKIP_KIND_SETUP=true make test-e2e
```

### 2. Build and load images

```bash
make build-images
make kind-load
```

`kind-load` streams images directly into the cluster — no registry push needed.

### 3. Deploy the operator

```bash
make deploy
```

This runs `helm upgrade --install` with the local chart and the current image tag. To deploy with your custom registry instead:

```bash
LITTLERED_REGISTRY=registry.example.com/myorg make deploy
```

Or install from the published OCI chart directly:

```bash
helm install littlered oci://registry.example.com/myorg/charts/littlered \
  --version <version> \
  -n littlered-system --create-namespace \
  --set image.repository=registry.example.com/myorg/littlered \
  --set image.tag=<tag>
```

### 4. Tear down

```bash
make cleanup-test-e2e
```

## Running Tests

### Unit tests

```bash
make test
```

### End-to-end tests

```bash
make test-e2e
```

The e2e suite creates a Kind cluster, builds and loads images, deploys the operator, runs Ginkgo specs, and tears down the cluster. See [E2E Testing](E2E_TESTING.md) for options (`FOCUS`, `DEBUG_ON_FAILURE`, `SKIP_KIND_SETUP`, etc.).

## Release Workflow

```bash
git tag v0.x.y
make images           # build + push versioned images to ghcr.io
make helm-push        # package + push versioned Helm chart
make push-latest      # re-tag images as :latest
```

`push-latest` is a no-op on untagged commits — it checks that `GIT_TAG` matches `vX.Y.Z` before doing anything.

There is no `:latest` tag for the Helm chart: Helm treats the OCI tag (and `--version`) as a semver constraint, so `latest` is rejected (`improper constraint: latest`). To get the newest chart, omit `--version` — Helm resolves it to the highest semver tag.
