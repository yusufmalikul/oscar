#!/bin/sh
# Comments on a GitHub PR. Inline when path+line are given, otherwise a general comment.
set -eu
in=$(cat)
get() { printf '%s' "$in" | jq -r "$1"; }
cd "$(get .repo_path)"
pr=$(get .pr); path=$(get '.path // ""'); line=$(get '.line // 0'); text=$(get .text)
if [ -n "$path" ] && [ "$line" != "0" ]; then
  sha=$(gh pr view "$pr" --json headRefOid -q .headRefOid)
  gh api "repos/{owner}/{repo}/pulls/$pr/comments" -f body="$text" -f commit_id="$sha" -f path="$path" -F line="$line" -f side=RIGHT -q .html_url \
    | jq -R '{url: .}'
else
  gh pr comment "$pr" --body "$text" | jq -R '{url: .}'
fi
