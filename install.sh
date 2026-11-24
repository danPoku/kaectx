#!/bin/sh
# Copyright 2026 Dan Gyinaye Poku
# SPDX-License-Identifier: Apache-2.0
#
# Installs a prebuilt kae on Linux or macOS:
#
#   curl -fsSL https://raw.githubusercontent.com/danPoku/kaectx/master/install.sh | sh
#
# It downloads the archive for this machine from the GitHub release, checks it
# against the release's SHA256SUMS, and copies `kae` into a folder on PATH.
# Nothing is installed if the checksum does not match.
#
# Settings (environment variables):
#   KAE_VERSION      release tag to install, e.g. v0.3.0 (default: the latest)
#   KAE_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   KAE_BASE_URL     where to download from instead of GitHub; used by CI to
#                    test this script against archives it has just built
#
# The whole script is inside main() and main is called on the last line, so a
# download cut off half way runs nothing instead of half a script.

set -eu

REPO="danPoku/kaectx"

say() { printf '%s\n' "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

fetch() {
    # fetch URL FILE
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$1" -o "$2"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$1" -O "$2"
    else
        fail "curl or wget is needed to download kae"
    fi
}

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        fail "sha256sum or shasum is needed to verify the download"
    fi
}

main() {
    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) fail "unsupported system $(uname -s). On Windows use install.ps1; elsewhere build from source (see the README)." ;;
    esac
    case "$(uname -m)" in
        x86_64 | amd64) arch=amd64 ;;
        arm64 | aarch64) arch=arm64 ;;
        *) fail "no prebuilt kae for $(uname -m). Build from source (see the README)." ;;
    esac

    asset="kae_${os}_${arch}.tar.gz"
    if [ -n "${KAE_BASE_URL:-}" ]; then
        base="${KAE_BASE_URL%/}"
    elif [ -n "${KAE_VERSION:-}" ]; then
        base="https://github.com/${REPO}/releases/download/${KAE_VERSION}"
    else
        base="https://github.com/${REPO}/releases/latest/download"
    fi
    dest="${KAE_INSTALL_DIR:-$HOME/.local/bin}"

    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT

    say "Downloading ${asset}"
    fetch "${base}/${asset}" "${tmp}/${asset}" || fail "could not download ${base}/${asset}"
    fetch "${base}/SHA256SUMS" "${tmp}/SHA256SUMS" || fail "could not download ${base}/SHA256SUMS"

    # The checksum file lists every archive of the release; take this one's
    # line. A release without a line for this archive is treated the same as
    # a wrong checksum: there is nothing to trust the download against.
    want="$(awk -v f="$asset" '{ name = $2; sub(/^\*/, "", name); if (name == f) print $1 }' "${tmp}/SHA256SUMS")"
    [ -n "$want" ] || fail "SHA256SUMS has no entry for ${asset}; not installing"
    got="$(sha256_of "${tmp}/${asset}")"
    if [ "$want" != "$got" ]; then
        fail "checksum mismatch for ${asset}: expected ${want}, got ${got}. Not installing."
    fi

    mkdir -p "${tmp}/pkg" "$dest"
    tar -xzf "${tmp}/${asset}" -C "${tmp}/pkg"
    [ -f "${tmp}/pkg/kae" ] || fail "the archive does not contain kae"

    # Copy to a temporary name in the destination and rename over the old
    # binary, so a kae that is running (a daemon, an agent's MCP server) is
    # replaced in one step and keeps running its own copy until restarted.
    install_bin() {
        cp "${tmp}/pkg/$1" "${dest}/.$1.new"
        chmod 755 "${dest}/.$1.new"
        mv -f "${dest}/.$1.new" "${dest}/$1"
    }
    install_bin kae
    # `ctx` is the command's old name, kept as a deprecated alias. It is
    # refreshed only where one is already installed, so a new machine does
    # not gain a second command it never asked for.
    if [ -f "${dest}/ctx" ] && [ -f "${tmp}/pkg/ctx" ]; then
        install_bin ctx
        say "Updated the deprecated ctx alias in ${dest}"
    fi

    say "Installed $("${dest}/kae" version) to ${dest}/kae"
    case ":${PATH}:" in
        *":${dest}:"*) ;;
        *) say "Add ${dest} to your PATH to run it as 'kae':  export PATH=\"${dest}:\$PATH\"" ;;
    esac
    say "Next: kae ingest, then kae search \"something you worked on\""
}

main "$@"
