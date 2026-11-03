#!/bin/sh
# Merges a GitHub PR (used by "Merge & update" for Oscar's own repo). Input: {repo_path, url}.
set -eu
in=$(cat)
cd "$(printf '%s' "$in" | jq -r .repo_path)"
url=$(printf '%s' "$in" | jq -r .url)
gh pr ready "$url" >/dev/null 2>&1 || true   # draft PRs must be marked ready first
gh pr merge "$url" --squash --delete-branch
jq -n --arg url "$url" '{url: $url}'
