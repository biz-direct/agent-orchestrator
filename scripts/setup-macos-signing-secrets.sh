#!/usr/bin/env bash
#
# Prompts for the macOS signing files and values, then runs `gh secret set` for
# the six secrets documented in docs/macos-signing.md. Secret values are read
# from files or hidden prompts and piped to gh on stdin; they are never echoed
# and never appear on a command line.
#
# usage: scripts/setup-macos-signing-secrets.sh [owner/repo]

set -euo pipefail

repo="${1:-biz-direct/agent-orchestrator}"

command -v gh >/dev/null || { echo "gh CLI is required" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "run 'gh auth login' first" >&2; exit 1; }

read_file() { # prompt -> path (must be a readable file)
	local path
	read -r -p "$1: " path
	path="${path/#\~/$HOME}"
	[ -f "$path" ] && [ -r "$path" ] || { echo "not a readable file: $path" >&2; exit 1; }
	printf '%s' "$path"
}

read_secret() { # prompt -> value on stdout, input hidden
	local value
	read -r -s -p "$1: " value
	echo >&2
	[ -n "$value" ] || { echo "value must not be empty" >&2; exit 1; }
	printf '%s' "$value"
}

set_from_stdin() { # name; value on stdin
	gh secret set "$1" --repo "$repo" >/dev/null
	echo "set $1 on $repo"
}

p12="$(read_file 'Path to Developer ID Application .p12')"
p8="$(read_file 'Path to App Store Connect AuthKey_XXXX.p8')"

base64 < "$p12" | tr -d '\n' | set_from_stdin CSC_LINK
base64 < "$p8" | tr -d '\n' | set_from_stdin APPLE_API_KEY_BASE64
read_secret '.p12 export password' | set_from_stdin CSC_KEY_PASSWORD
read_secret 'Signing identity (Developer ID Application: Name (TEAMID))' | set_from_stdin APPLE_SIGNING_IDENTITY
read_secret 'App Store Connect Key ID' | set_from_stdin APPLE_API_KEY_ID
read_secret 'App Store Connect Issuer ID' | set_from_stdin APPLE_API_ISSUER

echo "done. Next release built by fork-release.yml will be signed and notarized."
