# Container distribution

Each versioned Lazarus release publishes the same build in two forms:

- a multi-platform image at `ghcr.io/jayk56/lazarus` for Linux AMD64 and ARM64;
- a compressed OCI archive attached to the GitHub Release for disconnected or
  internally mirrored environments.

The release also includes the exact image digest, per-platform SPDX software
bills of materials (SBOMs), build provenance, a digest-pinned Helm values file,
the packaged Helm chart, a vulnerability report, release metadata, signing
material, and SHA-256 checksums. `LICENSE` contains the Apache-2.0 terms for Lazarus, while
`THIRD_PARTY_LICENSES` preserves the terms and notices for software compiled or
packaged into the image. Both files are also inside the container and Helm
chart. Use the digest—not a moving tag—as the production image reference.

The image's OCI license annotation identifies the Apache-2.0 project license.
It does not replace the separate terms for bundled software; the SBOM and
`THIRD_PARTY_LICENSES` identify those components and their licenses.

## Pull and verify the image

Download `image-digest.txt` from the selected GitHub Release, then pull that
exact image:

```sh
IMAGE='ghcr.io/jayk56/lazarus'
DIGEST='sha256:REPLACE_WITH_RELEASE_DIGEST'
podman pull "${IMAGE}@${DIGEST}"
```

Verify that GitHub built the image from this repository:

```sh
gh attestation verify "oci://${IMAGE}@${DIGEST}" --repo Jayk56/lazarus
```

The image also has a keyless Sigstore signature. Verify both the expected
workflow identity and GitHub Actions issuer:

```sh
cosign verify "${IMAGE}@${DIGEST}" \
  --certificate-identity-regexp \
  '^https://github\.com/[Jj]ayk56/lazarus/\.github/workflows/release-image\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

The version tags are convenient for discovery, but deployment manifests should
use the digest. In Helm values, set `image.repository` to the registry path and
`image.digest` to the release digest; the chart then ignores `image.tag`.

## Download for a disconnected environment

Download the release assets and verify their checksums:

```sh
VERSION='1.0.0'
gh release download "v${VERSION}" --repo Jayk56/lazarus --dir "lazarus-${VERSION}"
cd "lazarus-${VERSION}"
sha256sum --check SHA256SUMS
```

When GitHub release immutability is enabled, the GitHub CLI can also verify that
a downloaded asset is exactly the file attached to that release:

```sh
gh release verify "v${VERSION}" --repo Jayk56/lazarus
gh release verify-asset "v${VERSION}" "lazarus-${VERSION}.oci.tar.gz" \
  --repo Jayk56/lazarus
```

Verify the archive's keyless signature, then copy its image and attached
evidence into an approved internal registry without rebuilding it:

```sh
cosign verify-blob "lazarus-${VERSION}.oci.tar.gz" \
  --bundle "lazarus-${VERSION}.oci.tar.gz.sigstore.json" \
  --certificate-identity-regexp \
  '^https://github\.com/[Jj]ayk56/lazarus/\.github/workflows/release-image\.yml@refs/tags/v[0-9]+\.[0-9]+\.[0-9]+.*$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
gzip --decompress --keep "lazarus-${VERSION}.oci.tar.gz"
mkdir "lazarus-${VERSION}-oci"
tar -C "lazarus-${VERSION}-oci" -xf "lazarus-${VERSION}.oci.tar"
oras discover --oci-layout "lazarus-${VERSION}-oci:${VERSION}"
oras cp --recursive --from-oci-layout \
  "lazarus-${VERSION}-oci:${VERSION}" \
  "registry.example.com/platform/lazarus:${VERSION}"
```

The recursive copy preserves the multi-platform index and its OCI referrers,
including attestations and signatures. Record the internal registry digest
after import and deploy that digest. Keep all release assets together as the
procurement evidence for the approved version.

## What the release workflow does

Pushing a semantic-version tag such as `v1.0.0` runs
`.github/workflows/release-image.yml`. The workflow:

1. requires the Apache-2.0 `LICENSE` file and a `vMAJOR.MINOR.PATCH` tag;
2. runs the Go tests and static checks;
3. builds one AMD64/ARM64 image and pushes it to GitHub Container Registry;
4. fails if Trivy finds a fixable high- or critical-severity vulnerability;
5. records the source revision, build time, image source, and version as OCI
   metadata;
6. attaches SBOM and provenance data to the image;
7. creates a signed GitHub build attestation and keyless Sigstore signature;
8. recursively copies the registry image and its referrers into a compressed
   OCI archive;
9. generates the third-party license bundle, extracts standalone SBOM and
   provenance files, writes the digest and checksums, packages the Helm chart,
   writes digest-pinned Helm values, and attests the downloadable files;
10. attaches every file to a draft GitHub Release and publishes it only after
   all steps succeed.

The workflow uses only the repository's short-lived `GITHUB_TOKEN` and GitHub
OIDC identity. It does not require a long-lived registry or signing secret.

## Repository setup before the first release

Complete these one-time steps:

1. Push this repository to `github.com/Jayk56/lazarus` or update the documented
   image and repository names if a different location will own the release.
2. Keep GitHub Actions enabled and allow the workflow's declared
   `contents`, `packages`, `attestations`, and `id-token` permissions.
3. Create a GitHub environment named `release` and require the desired
   procurement or maintainer approval before a release job can run.
4. Enable release immutability in the repository settings before publishing the
   first release. The workflow creates and fills a draft before publishing it,
   which is compatible with immutable release assets.
5. Enable private vulnerability reporting and review [Security](../SECURITY.md).
6. Protect tags matching `v*` with a repository ruleset.
7. After the first image is published, change the GHCR package visibility from
   private to public. GitHub creates new container packages as private by
   default; public GHCR images can be pulled without authentication. This
   visibility change cannot be reversed.

## Publish a version

Run the release checks from the exact commit you intend to publish:

```sh
make validate-source
git tag --sign v1.0.0
git push origin v1.0.0
```

The protected `release` environment supplies the human approval point. After
the workflow succeeds, verify the release assets, the anonymous GHCR pull, the
image attestation, and a test deployment by digest before submitting the
version for procurement approval.

## Procurement record

For each approved version, retain:

- the immutable GitHub Release URL and source tag;
- the project license and source revision;
- the third-party license bundle;
- the GHCR image name and digest;
- the OCI archive, its Sigstore bundle, and `SHA256SUMS`;
- the SPDX SBOMs, provenance files, release metadata, Helm chart, and
  digest-pinned values;
- the producer vulnerability report and successful GitHub and Cosign
  verification output;
- vulnerability-scan results produced under the receiving organization's own
  policy;
- the relevant [architecture](architecture.md), [security](security.md),
  [operations](operations.md), and [validation](validation.md) documents.

The supplied evidence establishes what was built and where it came from. The
receiving organization remains responsible for vulnerability policy, registry
admission rules, platform testing, and deciding which Lazarus versions it will
support.
