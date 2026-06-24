# Releasing

Releases are published by [`.github/workflows/publish.yml`](.github/workflows/publish.yml),
which triggers on pushing a `v*` git tag. The workflow builds and pushes the
container images and Helm chart, builds the `lrctl` binaries, and creates a
GitHub Release. The release notes are generated from the matching section of
[`CHANGELOG.md`](CHANGELOG.md) (falling back to `[Unreleased]` if no
version-specific section exists).

## Keeping the changelog

Record user-facing changes under the `## [Unreleased]` section of
`CHANGELOG.md` as they are merged, grouped under `Added` / `Changed` /
`Deprecated` / `Removed` / `Fixed` / `Security`
([Keep a Changelog](https://keepachangelog.com/en/1.1.0/)).

## Cutting a release

Use the helper script to promote `[Unreleased]` to the new version, bump the
Helm chart version to match (required — `publish.yml` fails the release if the
tag and `charts/littlered/Chart.yaml` version differ), and optionally commit and
tag:

```bash
# Edit CHANGELOG.md / Chart.yaml only, then print the git commands to run:
scripts/prepare-release.sh 0.3.0

# Or have the script commit and create the tag in one step:
scripts/prepare-release.sh 0.3.0 --tag
```

Then push to trigger the publish workflow:

```bash
git push origin main --follow-tags
```

The script never pushes — pushing the tag is what kicks off the release, so it
is left as an explicit, deliberate step.

### What the script does

- Promotes `## [Unreleased]` to `## [<version>] - <YYYY-MM-DD>` and leaves a
  fresh empty `## [Unreleased]` on top.
- Sets `version:` in `charts/littlered/Chart.yaml` to `<version>`.
- Refuses to run if the `[Unreleased]` section is empty (override with
  `--allow-empty`), if a section for the version already exists, or if the
  version is not valid semver.
- With `--commit` it commits both files; with `--tag` it also creates the
  annotated `v<version>` tag (implies `--commit`).

Run `scripts/prepare-release.sh --help` for the full flag list.
