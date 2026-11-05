# Development prereleases

Publish an existing development branch as `X.Y.Z-dev.YYYYMMDD.N` with GitHub's
prerelease flag and `make_latest: false`. Do not move an existing version tag.
Publishing the release triggers `release.yml` from that tag; no merge is needed.

The release workflow accepts stable numeric tags and this explicit dev format.
It reads the actual release's prerelease flag for both release events and manual
dispatches, rejects dev tags marked stable, and never promotes a prerelease to
Docker `latest`. Dev images use the full version tag only. The pinned Web and
protocol revisions remain the build inputs unless an explicit Web ref is given.

Release notes should identify the source commit and any migration requirements.
For plugin package admission, configure an independent native TOTP or verified
native OTP factor before upgrading: historical permission approvals require
explicit package reapproval and an unapproved Passkey plugin cannot authorize
its own first activation.
