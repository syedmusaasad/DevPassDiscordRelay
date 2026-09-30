# DevPassDiscordRelay

DevPassDiscordRelay connects local devpass-code tmux sessions to Discord threads for remote monitoring and interaction.

The local tmux session serves as the sole source of truth. Discord acts as a remote mirror and an external input channel. All agent execution, state changes, and file edits occur on your host machine inside tmux. If the relay service crashes or disconnects, your terminal session continues uninterrupted. When you return to the terminal, you retain full interactive control of the running session.

```
+------------------------------------------------------------------------+
|                              Host Machine                              |
|                                                                        |
|  +--------------------+         read-only          +----------------+  |
|  |   devpass-code     | -------------------------> |  opencode.db   |  |
|  |   (inside tmux)    |                            |  (SQLite WAL)  |  |
|  +--------------------+                            +----------------+  |
|         ^          |                                       |           |
| send-   |          | capture-pane                          | tail      |
| keys    |          v (readiness/refusal)                   v rows      |
|  +------------------------------------------------------------------+  |
|  |                    DevPassDiscordRelay Daemon                    |  |
|  |                                                                  |  |
|  |   - Tails transcript parts from opencode.db                      |  |
|  |   - Inspects pane status with capture-pane                       |  |
|  |   - Filters Discord events by DISCORD_USER_ID                    |  |
|  +------------------------------------------------------------------+  |
|                                |                                       |
|             outbound HTTPS REST and Gateway WebSocket                  |
+--------------------------------|---------------------------------------+
                                 v
+------------------------------------------------------------------------+
|                      Discord Cloud (Private Guild)                     |
|                                                                        |
|  Channel: DISCORD_CHANNEL_ID                                           |
|    |                                                                   |
|    +-- Thread: ses_<session-id> (session mirror + user replies)        |
+------------------------------------------------------------------------+
```

## Transcript vs pane

The relay divides session observability into content and state. The daemon tails transcript content from SQLite and reads operational state from tmux.

devpass-code version 1.18.11 persists conversation records exclusively in an SQLite database. The engine stores conversation records at ~/.local/share/devpass-code/opencode.db in WAL mode. It generates no per-session JSONL files on disk. The relay opens opencode.db with URI `file:<path>?mode=ro` to tail message and part rows. Content extraction reads user prompts, assistant turns, tool invocations, and tool results directly from this database.

Terminal state relies strictly on tmux capture-pane. The relay never parses conversational prose from the terminal pane. Instead, the daemon invokes capture-pane to detect readiness markers, session refusals, and interactive approval prompts. Specifically, capture-pane confirms when the pane displays the "Ask anything..." prompt before keystroke injection. It also identifies "Error: Session not found" during failed session resumes, and detects "Permission required" prompts. State detection belongs to capture-pane, while message content belongs to opencode.db. The architecture strictly maintains this boundary.

## Discord structural difference and privacy

Discord bots cannot create message threads inside Direct Message channels. Discord restricts thread primitives to guild channels. For this reason, DevPassDiscordRelay requires a dedicated private server owned by you.

This structural constraint introduces a direct privacy boundary. If you invite the bot into a shared or multi-user server, other guild members could view your mirrored prompts, assistant outputs, and tool activity. You must run the bot inside a private server where you remain the sole human member. Limiting server membership prevents third parties from reading your terminal transcripts.

## Security posture

DevPassDiscordRelay makes the following security commitments and disclosures:

- Runs on your machine under your account: The daemon executes locally as an ordinary process of the account that installed it (a systemd user unit). It does not drop privileges itself, so anything it runs has your account's access.
- Per-user token in .env never committed and backed up across reinstalls: Secrets live in ~/.config/devpass-discord-relay/.env outside source control. The install.sh script preserves this file during updates.
- Single-user gating at the event layer: The relay refuses to start (non-zero exit, with a log line naming DISCORD_USER_ID) when DISCORD_USER_ID is unset or empty; there is no accept-everyone mode. Once running, the handler discards messages from every other user and from bots before processing.
- Outbound only: The daemon establishes outbound TLS connections to Discord API endpoints. It opens no listening network ports on your machine.
- Content goes only to your own private guild: The daemon forwards transcripts solely to the configured private server channel. No external metrics service receives conversation logs.
- Telemetry is off unless you set TELEMETRY_URL: When TELEMETRY_URL is set, the relay POSTs once per UTC day (while running, not only at startup) a body containing only sha256(username)[:12] and the relay version. Nothing else is sent. Leave TELEMETRY_URL unset and nothing is sent.

## Usage

Start a session with dpmux to initiate mirroring:

```bash
dpmux
```

List active sessions managed by the relay:

```bash
dpmux ls
```
