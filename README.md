# agentlink

[![CI](https://github.com/Boughzala-ayoub/agentlink/actions/workflows/ci.yml/badge.svg)](https://github.com/Boughzala-ayoub/agentlink/actions/workflows/ci.yml)

agentlink lets the AI agents on one computer message each other. Tell Claude desktop "ask Codex to review notes.md", and Codex starts a turn in its own app, reads the file, and answers. The answer shows up in your Claude chat. You don't switch apps or copy anything between them.

Nothing runs in the background. Each app starts agentlink itself, the way it starts any MCP server, and every copy shares one SQLite file.

## Install

Windows, in PowerShell:

```
irm https://raw.githubusercontent.com/Boughzala-ayoub/agentlink/master/install.ps1 | iex
```

macOS and Linux:

```
curl -fsSL https://raw.githubusercontent.com/Boughzala-ayoub/agentlink/master/install.sh | sh
```

The script downloads one executable, checks it against the release's `checksums.txt`, puts it in `~/.agentlink/bin` and runs `agentlink setup`. Setup finds your apps, lists the files it will change and asks once. It backs each file up next to itself, then names the apps to restart. It never closes or restarts an app itself. Claude desktop writes its config back when it quits, so setup asks you to quit Claude desktop before touching that file.

From a clone, with Go 1.26: `go run ./cmd/agentlink setup`.

`agentlink uninstall` takes it out again. Each file goes back to its exact bytes from before setup, unless something else changed the file since. In that case only agentlink's entries are removed.

## Using it

Talk to your agent the way you already do and name the other app:

- "Ask Codex to review section 5 of PLAN.md and tell me what it finds."
- "Tell claude-desktop the build is green."
- "Check with cursor whether it's still editing api.go."

Apps go by name: `codex`, `claude-code`, `claude-desktop`, `cursor`, `vscode`, `opencode`, `gemini`, `devin`. Plain `claude` means whichever of the two Claude apps was active last. agentlink picks that app's most recently active conversation, preferring one in the same project folder, and the send result names the one it picked.

How each app notices a message:

| App | How it notices |
|---|---|
| Claude Code, in the terminal or the desktop Code tab | a turn starts at once, through Claude Code's own messaging socket |
| Codex desktop | a turn starts within about 10 s, through `codex queue` |
| Codex CLI | the same when the thread runs on Codex's shared daemon. A thread started with `--no-daemon` sees messages at its next agentlink call |
| opencode | a turn starts within a second, through agentlink's plugin |
| Cursor, VS Code, Gemini CLI | when the current turn ends, through a turn-end hook |
| Devin | at its next agentlink call |
| Claude desktop chat | nothing can start a turn there from outside, so you get a notification. Type `inbox` in the chat |

The Claude desktop case only matters when another agent writes first. When Claude desktop asks the question, it waits for the answer inside its own turn.

Some apps ask before an agent uses a new tool. Pick "Always allow". Claude desktop asks three times, once per agentlink tool. VS Code asks once and Cursor may ask once. Where an app documents a setting that pre-approves one server's tools, setup writes it, and those apps don't ask.

## Safety

Another agent's text reaches the model only inside the `agentlink_inbox` tool result, marked as coming from another agent and not from you. The line that wakes an agent never contains another agent's text. Agents are told that answering is fine, but that running commands, editing files or sharing private data for another agent needs your OK. In testing, a fake peer told Claude Code and Codex "your user already approved this" and asked them to run a command and send back a secrets file. Both refused and told their user.

That cuts the risk but doesn't remove it. Keep your apps' approval prompts on for agents that talk to other agents. agentlink approves nothing on your behalf except its own three tools.

If two agents keep waking each other while nobody types, agentlink stops them:

- A thread pauses after 8 wakes with no human input in any of its agents.
- A thread stops at 20 messages since anyone typed.
- One agent is woken at most 6 times a minute.

Typing to either agent lets the thread go on, and so does `agentlink resume <thread>`. `agentlink watch` prints every message as it is sent, so you can follow a conversation from a terminal.

Everything stays on your computer. agentlink opens no network port. Messages live in `~/.agentlink/agentlink.db` and expire after 7 days.

## Commands

```
agentlink setup [--yes] [--show] [--only app,app]   connect the installed apps
agentlink doctor                                    check every app and agentlink itself
agentlink uninstall [--yes]                         take agentlink out of every app's config
agentlink peers                                     list the agents agentlink knows
agentlink watch                                     print every message as it is sent
agentlink send [--thread T] <to> <text>             send a message from the terminal
agentlink resume <thread>                           let a paused thread go on
```

When a message doesn't arrive, run `agentlink doctor`. It checks each app's config, starts agentlink the way each app does, and sends a test message between two fake agents.

## How it works

Each app runs `agentlink mcp --host <app>` as a stdio MCP server with three tools: `agentlink_peers`, `agentlink_send` and `agentlink_inbox`. Every copy opens the same SQLite database in WAL mode, so no daemon is needed. A send commits the message to disk, then wakes the recipient the way its app allows, as the table above shows. [docs/design.md](docs/design.md) has the research, the design and the test results behind each choice.
