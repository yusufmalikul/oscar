#!/bin/sh
# Posts to Slack with a user or bot token from $SLACK_TOKEN (needs chat:write).
# Input: {channel, thread, text}. Output: {"url": permalink}.
set -eu
: "${SLACK_TOKEN:?set SLACK_TOKEN}"
in=$(cat)
body=$(printf '%s' "$in" | jq '{channel, text} + (if (.thread // "") != "" then {thread_ts: .thread} else {} end)')
resp=$(curl -sS -X POST https://slack.com/api/chat.postMessage \
  -H "Authorization: Bearer $SLACK_TOKEN" -H 'Content-Type: application/json; charset=utf-8' --data "$body")
printf '%s' "$resp" | jq -e '.ok' >/dev/null || { printf '%s' "$resp" | jq -r '.error' >&2; exit 1; }
ch=$(printf '%s' "$resp" | jq -r .channel); ts=$(printf '%s' "$resp" | jq -r .ts)
link=$(curl -sS -G https://slack.com/api/chat.getPermalink -H "Authorization: Bearer $SLACK_TOKEN" \
  --data-urlencode "channel=$ch" --data-urlencode "message_ts=$ts" | jq -r '.permalink // ""')
jq -n --arg url "$link" '{url: $url}'
