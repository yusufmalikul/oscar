#!/bin/sh
# Changes a GitHub PR description (and title). Input: {repo_path, url, description, title?}.
set -eu
in=$(cat)
cd "$(printf '%s' "$in" | jq -r .repo_path)"
url=$(printf '%s' "$in" | jq -r .url); title=$(printf '%s' "$in" | jq -r '.title // ""')
printf '%s' "$in" | jq -r .description | gh pr edit "$url" --body-file -
[ -z "$title" ] || gh pr edit "$url" --title "$title"
jq -n --arg url "$url" '{url: $url}'
