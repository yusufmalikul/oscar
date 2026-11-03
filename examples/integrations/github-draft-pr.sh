#!/bin/sh
# Opens a draft PR with the GitHub CLI. Input: JSON on stdin. Output: {"url": "..."}.
set -eu
in=$(cat)
get() { printf '%s' "$in" | jq -r "$1"; }
cd "$(get .worktree)"
url=$(gh pr create --draft --base "$(get .base)" --head "$(get .branch)" \
  --title "$(get .title)" --body "$(get .description)")
jq -n --arg url "$url" '{url: $url}'
