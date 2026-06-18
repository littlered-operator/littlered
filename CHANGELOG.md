# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Record changes under `[Unreleased]`; see [RELEASING.md](RELEASING.md) for how to
cut a release (`scripts/prepare-release.sh`).

## [Unreleased]

### Changed

- Leader election is now always enabled and is no longer configurable. The
  operator manager hardcodes `LeaderElection: true`, the `--leader-elect`
  command-line flag has been removed, and the Helm chart's
  `leaderElection.enabled` value has been removed. This guarantees that only
  one controller manager reconciles at a time regardless of how the deployment
  is scaled (via Helm or directly with `kubectl`/`k9s`), preventing concurrent
  reconcilers from racing over Sentinel master/failover state. The
  leader-election RBAC (Role/RoleBinding) is now rendered unconditionally.

  **Upgrade note:** installs that previously set `leaderElection.enabled: false`
  will have leader election forced on at the next `helm upgrade`. This is the
  intended hardening and requires no action, but the now-unknown
  `leaderElection.enabled` value should be removed from custom `values.yaml`
  files to avoid confusion.
