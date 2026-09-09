"""Repository rules for pinned distribution artifacts.

Phase 1 ships only the @openssh_dist placeholder: nothing depends on it yet,
so Bazel never fetches it and the placeholder sha256 is never checked. Phase 2
(tools/dist) replaces this placeholder with the real pinned OpenSSH 10.5p1
tarball (S3 primary + GitHub mirror_urls) and adds the other dist products
(git, fish, containerd, ca-certificates).
"""

load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

def _dist_impl(_mctx):
    # Placeholder only. The all-zero sha256 is deliberate: if any target
    # reaches for @openssh_dist before Phase 2 wires the real artifact, the
    # fetch fails loudly instead of silently building against a stub.
    http_archive(
        name = "openssh_dist",
        urls = [
            "https://github.com/ChronicCmposer/gitd-dist/releases/download/openssh-10.5p1/openssh-10.5p1.linux-arm64.tar.gz",
        ],
        sha256 = "0000000000000000000000000000000000000000000000000000000000000000",
    )

dist = module_extension(implementation = _dist_impl)