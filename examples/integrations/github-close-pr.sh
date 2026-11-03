#!/bin/sh
# Closes a GitHub PR with a comment (after its branch was renamed). Input: {repo_path, url, comment}.
set -eu
in=$(cat)
cd "$(printf '%s' "$in" | jq -r .repo_path)"
url=$(printf '%s' "$in" | jq -r .url)
gh pr close "$url" --comment "$(printf '%s' "$in" | jq -r .comment)"
jq -n --arg url "$url" '{url: $url}'
