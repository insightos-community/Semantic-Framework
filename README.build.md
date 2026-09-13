# Semantic-Framework: reproducible platform builds

## Versions, tools and build layout

The glibc installer pins component tag **v0.5.0-insightos.2026.2** at `81ea016480f099f9db95bd48c083ee99d8806a4d`.
This guide pins the current build-script snapshot at `e4e32fa6d7a5b071c42228ab5364f2f1966cba6c`.
To reconstruct another published release, read its `release.json` and select
both `source_commit` and `build_recipe_commit`; a source tag alone may predate
the CI scripts. This recipe reproduces the build steps, not historical archive bytes.

Prerequisites: Ubuntu 24.04 x86_64; Go 1.25.8, GCC, musl-tools, binutils and Python 3.

The release scripts expect **two sibling checkouts**, `automation/` for build
scripts and `source/` for the component. Run these commands from a fresh working
directory (the scripts themselves are not standalone copies):

```bash
REPRO_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/Semantic-Framework-repro.XXXXXXXX")"
git clone --no-checkout https://github.com/insightos-community/Semantic-Framework.git "$REPRO_ROOT/automation"
GIT_LFS_SKIP_SMUDGE=1 git -C "$REPRO_ROOT/automation" checkout --detach e4e32fa6d7a5b071c42228ab5364f2f1966cba6c
git clone --no-checkout https://github.com/insightos-community/Semantic-Framework.git "$REPRO_ROOT/source"
GIT_LFS_SKIP_SMUDGE=1 git -C "$REPRO_ROOT/source" checkout --detach v0.5.0-insightos.2026.2
cd "$REPRO_ROOT/source"
test "$(git rev-parse HEAD)" = 81ea016480f099f9db95bd48c083ee99d8806a4d
export TARGET_TAG=v0.5.0-insightos.2026.2
export COMPONENT=semantic-framework
export GITHUB_SHA=e4e32fa6d7a5b071c42228ab5364f2f1966cba6c
```

## Linux glibc / standard component Release

The executable build entry is [`.github/scripts/build.sh`](.github/scripts/build.sh);
archive validation is [`.github/scripts/package.py`](.github/scripts/package.py).
From `source/` in the layout above:

```bash
bash ../automation/.github/scripts/build.sh
python3 ../automation/.github/scripts/package.py semantic-framework
(cd .output/release && sha256sum -c SHA256SUMS)
```

Artifacts: `source/.output/release/` (archives/wheels, `release.json`, checksum
inventory and license notices). `release.json` records source and recipe revisions.
The local commands do not publish or overwrite a GitHub Release.

Despite being consumed by the glibc installer, the release applications are
**static musl-linked ELF**: `CGO_ENABLED=1 CC=musl-gcc`, build tags
`musl,netgo,osusergo` and external `-static` linkage. Host race tests use GCC.
For a host-glibc development build instead of the portable release recipe:

```bash
mkdir -p .output/glibc
for name in semantic-server semantic-pilot semantic; do
 CGO_ENABLED=1 CC=gcc go build -p 2 -trimpath -o ".output/glibc/$name" "./cmd/$name"
done
go test -p 2 ./... -race -count=1 -timeout=10m
```

## Linux musl

Use the standard release command above on the Ubuntu build host: it already
produces the static musl-linked Server/Pilot/CLI. No Alpine Go rebuild is required
for the current installer. The full musl package adds its own musl Python, wheels
and Mesa; native API binaries alone do not reproduce that package.

For the complete musl build and offline checks, use the [quick-start musl commands](https://github.com/insightos-community/quick-start/blob/main/README.build.md#linux-musl-x86_64).

## macOS / macosx

Use Apple Silicon arm64 and the native adaptation at `6d524fe4dbdfceb2029f7468619723274465c976`;
the older glibc component tag above may not contain the macOS fixes. Start a
separate checkout and run the native commands:

```bash
git clone https://github.com/insightos-community/Semantic-Framework.git Semantic-Framework-macos
cd Semantic-Framework-macos
git checkout --detach 6d524fe4dbdfceb2029f7468619723274465c976
test "$(uname -s)" = Darwin
test "$(uname -m)" = arm64
```

Prerequisites: Go 1.25.8 and Xcode Command Line Tools.

```bash
mkdir -p .output/bin
for name in semantic-server semantic-pilot semantic; do
 CGO_ENABLED=1 go build -p 2 -trimpath -o ".output/bin/$name" "./cmd/$name"
done
go test -p 2 ./... -count=1 -timeout=10m
tar -czf .output/semantic-framework-macos-arm64-development.tar.gz -C .output/bin .
```

The native workflow is [`.github/workflows/macos.yml`](.github/workflows/macos.yml).
Its artifacts are component development outputs; quick-start assembles and validates
the complete installer.

The complete macOS installer targets Apple Silicon/macOS 15.5+; see the [locked assembly instructions](https://github.com/insightos-community/quick-start/blob/main/README.build.md#macos-apple-silicon).

## GitHub workflow reproduction

The repository’s [CI workflow](.github/workflows/ci.yml) implements the two-checkout
layout. To build a source tag without publishing, create a reproduction branch at the
pinned automation commit. GitHub dispatch expects a branch/tag ref; both tag refs
and default-branch dispatches can enter this workflow’s publishing job. The following
commands require repository write access and use a non-default branch:

```bash
gh auth setup-git
REPRO_BRANCH=reproduce/platform-builds
git -C "$REPRO_ROOT/automation" push origin e4e32fa6d7a5b071c42228ab5364f2f1966cba6c:refs/heads/$REPRO_BRANCH
gh workflow run ci.yml --repo insightos-community/Semantic-Framework --ref "$REPRO_BRANCH" -f tag=v0.5.0-insightos.2026.2
gh run list --repo insightos-community/Semantic-Framework --workflow ci.yml --limit 5
# Set REPRO_RUN_ID to the selected run ID.
gh run watch "$REPRO_RUN_ID" --repo insightos-community/Semantic-Framework --exit-status
gh run download "$REPRO_RUN_ID" --repo insightos-community/Semantic-Framework --name release-assets --dir downloaded-release
```

```bash
git -C "$REPRO_ROOT/automation" push origin 6d524fe4dbdfceb2029f7468619723274465c976:refs/heads/reproduce/macos
gh workflow run macos.yml --repo insightos-community/Semantic-Framework --ref reproduce/macos
```

## Reproduction evidence

Build in a fresh checkout and a separate output directory for each ABI. Preserve
source commits, compiler/tool versions, dependency locks, package inventories and
test logs. Fixed source revisions and a container digest reproduce the recipe;
unlocked OS packages, runner images, timestamps and build tools can still change
archive bytes. Compare a downloaded release against its published `SHA256SUMS`;
do not expect a local rebuild to have the same digest.

See the [complete installer and repository index](https://github.com/insightos-community/quick-start/blob/main/README.build.md) for assembly order,
platform locks and end-to-end validation. Local build commands do not publish a
Release. Publishing requires repository write access and a new version tag;
existing release tags/assets should not be replaced.
