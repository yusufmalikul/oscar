# Oscar

Oscar lets one person work at the top level while Claude Code does the hands-on work
across many repositories.

You chat with the **Captain**. The Captain turns your request (a typed task, a pasted Slack
thread, a PR link) into tasks. Each task gets its own **worker**, a Claude Code session in
its own git worktree. When the worker is done, a fresh **reviewer** that never saw the
worker's process checks the result and gives a PASS or FAIL with evidence. A failed review
goes back to the worker, up to a limit, and after that it comes to you.

Everything that leaves your machine or changes something important waits for you in the
web UI: posting to Slack, PR comments, commands that write to a server or database, deploys.
These gates live in Oscar's code. Claude cannot switch them off.

```
You ── chat ──► Captain ──► Worker (Item-1, own worktree) ──► Reviewer (fresh)
                       ├──► Worker (Item-2, two repos)     ──► Reviewer
                       └──► answers quick questions itself
```

## Status

Early. It works end to end on one machine and can update itself. Not done yet: scheduled and
long-running monitor tasks, approval chains, a remote-access setup guide, and web push.

## How it works

- Oscar starts the `claude` CLI as child processes and talks to them over the stream-json
  protocol (the protocol the official Agent SDKs use). It uses your existing Claude Code
  login and settings.
- Every tool call goes through a `PreToolUse` hook that Oscar answers in code
  (`internal/policy`). The hook can allow the call, deny it with a reason, or send it to
  you for approval. Calls it does not decide go to the CLI permission mode (`auto` by
  default). Whenever the CLI would prompt, the prompt comes to you in the UI.
- Your own Claude Code permission rules count too. Oscar reads `permissions.allow`, `ask` and
  `deny` from `~/.claude/settings*.json` and from `.claude/settings*.json` in the start dir and
  in the task's repos (following `setting_sources`). What you allow in your terminal sessions is
  allowed for workers; Oscar's own deny rules and the main-checkout gate still come first.
- Workers report through Oscar's own tools (`report`, `ask`, `submit_for_review`,
  `open_draft_pr`, `outbox`, `finish`). The server checks every step. For example, a draft
  PR needs a PASS from the reviewer, and an ops command runs only after you approve the plan.
- Workers may push their task branch when you ask. Force push, deleting a remote branch and
  pushing to main/master wait for your approval. When the review passes, Oscar pushes the
  branch and opens a draft PR through an integration script.
- The Captain picks each worker's model and effort (`create_task`), within the defaults you set.
- When a worker needs you (a question, an approval, a message to post), the Captain tells you in
  the chat and passes your reply back (`answer_worker`). Approvals need your own reply in the
  chat, written after the request appeared; the Captain cannot give them alone. The item also
  stays in the task panel, so you can still answer it there.
- State is kept in `~/.oscar` as one folder per task. After a restart, Oscar resumes the
  Claude sessions where they stopped.
- Secrets are masked in logs, chat and task text.

## Task types

Built-in types: `bugfix`, `feature`, `debug` (a report with no code change), `ops` (plan,
then your approval, then run), `review` (review someone's PR; you approve comments before
they are posted), and `slack` (draft a thread reply that you edit and approve).

Each type is defined in a YAML file in `internal/engine/types`. The Captain can propose a
new type. You approve it once, it is saved to `~/.oscar/types`, and it is reused after that.

## Run

Requirements: Go 1.24+, Claude Code (`claude`) logged in, and `git`.

```sh
go build -o oscar ./cmd/oscar
mkdir -p ~/.oscar && cp examples/config.yaml ~/.oscar/config.yaml   # then edit it
./oscar                     # http://127.0.0.1:7777
```

Settings that matter:

- `claude_cwd`: sessions start in this folder, so they load its `CLAUDE.md`, memory and skills.
- `repos_dir`: the folder that holds your git repos.
- `rules_file`: team rules added to every prompt, such as the reply language and conventions.
- `integrations`: small scripts for draft PRs, PR comments, Slack posts and notifications.
  Examples for GitHub and Slack are in `examples/integrations`.

`./oscar` runs a small supervisor in front of the server. The supervisor restarts the server
after a self-update, and if the new version does not start, it rolls back to the previous one.
To start Oscar at login on macOS and keep it running, install the LaunchAgent:

```sh
examples/launchd/install.sh            # uses ./oscar; logs to ~/.oscar/server.log
```

Oscar listens on localhost only. To use it from your phone, put it behind a private network
such as Tailscale. Do not expose it to the internet.

## Test

```sh
go test ./...
OSCAR_LIVE=1 go test ./internal/claude/ -run Live   # talks to the real claude CLI
```

## License

See [LICENSE](LICENSE).
