## Tables

- **`session`**:
```sql
CREATE TABLE `session` (
  `id` text PRIMARY KEY,
  `project_id` text NOT NULL,
  `workspace_id` text,
  `parent_id` text,
  `slug` text NOT NULL,
  `directory` text NOT NULL,
  `path` text,
  `title` text NOT NULL,
  `version` text NOT NULL,
  `share_url` text,
  `summary_additions` integer,
  `summary_deletions` integer,
  `summary_files` integer,
  `summary_diffs` text,
  `metadata` text,
  `cost` real DEFAULT 0 NOT NULL,
  `tokens_input` integer DEFAULT 0 NOT NULL,
  `tokens_output` integer DEFAULT 0 NOT NULL,
  `tokens_reasoning` integer DEFAULT 0 NOT NULL,
  `tokens_cache_read` integer DEFAULT 0 NOT NULL,
  `tokens_cache_write` integer DEFAULT 0 NOT NULL,
  `revert` text,
  `permission` text,
  `agent` text,
  `model` text,
  `time_created` integer NOT NULL,
  `time_updated` integer NOT NULL,
  `time_compacting` integer,
  `time_archived` integer,
  CONSTRAINT `fk_session_project_id_project_id_fk` FOREIGN KEY (`project_id`) REFERENCES `project`(`id`) ON DELETE CASCADE
)
```
  - **Meanings**: `id` is the session ID (`ses_...`).
  - **Timestamps/Ordering**: `time_created`, `time_updated` (Unix epoch milliseconds).
  - **Parent/Subagent relation**: The `parent_id` column stores the parent session ID. Subagent sessions have this field non-null, allowing them to be filtered out by `WHERE parent_id IS NULL`.

- **`message`**:
```sql
CREATE TABLE `message` (
  `id` text PRIMARY KEY,
  `session_id` text NOT NULL,
  `time_created` integer NOT NULL,
  `time_updated` integer NOT NULL,
  `data` text NOT NULL,
  CONSTRAINT `fk_message_session_id_session_id_fk` FOREIGN KEY (`session_id`) REFERENCES `session`(`id`) ON DELETE CASCADE
)
```
  - **Meanings**: Represents a message turn (user or assistant). `data` contains JSON with role, finish reason, and tokens.
  - **Timestamps**: `time_created` and `time_updated` (Unix milliseconds).

- **`part`**:
```sql
CREATE TABLE `part` (
  `id` text PRIMARY KEY,
  `message_id` text NOT NULL,
  `session_id` text NOT NULL,
  `time_created` integer NOT NULL,
  `time_updated` integer NOT NULL,
  `data` text NOT NULL,
  CONSTRAINT `fk_part_message_id_message_id_fk` FOREIGN KEY (`message_id`) REFERENCES `message`(`id`) ON DELETE CASCADE
)
```
  - **Meanings**: Contains the actual content chunks (text, tool calls, tool results) for a message. `data` is JSON.
  - **Timestamps**: `time_created` (Unix ms). Ordered by `time_created`.

- **`session_message`, `session_input`, `todo`**: Exist in schema but are generally empty or unused for raw transcripts in headless usage. The real content streams via `message` and `part`.

## Event mapping

- **User message**:
  - `role`: "user" (in `message.data` JSON)
  - `part` type: "text" (in `part.data` JSON)
  - Harness/Agent injected preambles appear as separate `part` rows of `type: "text"` attached to the same `message_id` prior to or after the main user prompt part, ordered by `time_created`.
  - Example `message`: `{"role":"user","time":{"created":1788500968734},"agent":"pl-interactive","model":{"providerID":"llmgateway-devpass","modelID":"claude-fable-5-1","variant":"high"},"summary":{"diffs":[]}}`
  - Example `part`: `{"type":"text","text":"ping"}`

- **Assistant text**:
  - `role`: "assistant" (in `message.data` JSON)
  - `part` type: "text"
  - Example `part`: `{"type":"text","text":"Pong. Nothing in flight...","time":{"start":1788500971743,"end":1788500973199}}`

- **Tool call**:
  - `part` type: "tool-call" (in v1.18, some versions use `type:"tool"` with state status in `data`)
  - JSON fields: `type`, `tool` (e.g. "bash"), `callID`, `state.status`, `input`
  - Example: `{"type":"tool","tool":"bash","callID":"toolu_01LG9gcD92ECC5rNNVNPhfLK","state":{"status":"completed","input":{"command":"ls"}}, ...}`

- **Tool result**:
  - Bound to the same `part` as the tool call, updated when finished.
  - JSON fields: `state.status` transitions to "completed", `output` added, `metadata.output` added.
  - Example: (Same as tool call, but with `state.status="completed"` and populated `output` field).

- **Turn complete**:
  - Marked by `part` of type `step-finish`.
  - Example `part`: `{"reason":"stop","type":"step-finish","tokens":{"total":7605,"input":78,"output":38},"cost":0.00455225}`

## Turn boundaries

Assistant turn completion is marked by a `part` row appearing with `type: "step-finish"` and `reason: "stop"` (or a similar stop reason like `tool_calls`). The `message` data also gets updated with `finish: "stop"`.
Polling the DB every 250ms during a live ping/pong turn showed the following row counts for `[session_message, message, part]`:
- Turn starts: Msg count: 2, Part count: 1
- `step-start` added: Msg count: 2, Part count: 2
- Assistant text streams in: Msg count: 2, Part count: 3
- Turn finishes: Msg count: 2, Part count: 4 (the `step-finish` part appears)

## Approval prompt

Running devpass-code without `--auto` and with `bash: ask` permission in a disposable XDG-isolated tmux session. Prompt: "run `ls` using your shell tool".
- The approval prompt appears in the pane verbatim:
```
  ┃
  ┃  △ Permission required
  ┃    # Shell command
  ┃
  ┃  $ ls
  ┃
  ┃
  ┃   Allow once   Allow always   Reject  ctrl+f fullscreen  ⇆ select  enter con
```
- Keystrokes to approve: Left/Right arrows to select (`⇆ select`), then `Enter` to confirm.
- Confirming `--auto` suppresses it: When run with `--auto`, the tool executes immediately without prompting.
- DB representation: Pending approval does not generate a specific blocking "approval-pending" part in the DB; the tool call part is created with status "running" or similar, and the TUI intercepts execution to ask for approval.

## Question picker

Running isolated config with prompt: "ask me to choose between apple and pear using your question tool, then stop".
- The assistant replied: "I don't have a dedicated question tool available in this session, so I'll ask directly: Apple or pear — which do you choose?"
- No interactive option picker was presented.
- Evidence: In devpass-code 1.18.11, the assistant fell back to text. The DB representation is just an assistant text part. There is no TUI question picker in this version. Exact keystroke recipe to answer interactive questions via a tool is unverified.

## Isolation

Using `XDG_DATA_HOME=/tmp/devpass-code/discovery/xdg-data` successfully relocated `opencode.db` to `/tmp/devpass-code/discovery/xdg-data/devpass-code/opencode.db`. This path observation confirms that XDG_DATA_HOME cleanly isolates the persistence layer, which the test suite can rely on to never touch the real HOME.
