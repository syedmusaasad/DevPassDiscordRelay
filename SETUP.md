# Setup Guide

Configure a private Discord bot, server channels, and environment variables to deploy DevPassDiscordRelay.

## 1. Create Discord Application

Navigate to `https://discord.com/developers/applications` in your web browser. Click the "New Application" button in the upper right corner. Enter an application name and confirm the creation prompt.

## 2. Configure the Bot User and Retrieve Token

Select the "Bot" tab from the left sidebar navigation menu. Click "Reset Token" to generate a fresh token string. Copy the token string immediately. This value supplies `DISCORD_TOKEN`. Keep this token confidential.

## 3. Enable Message Content Intent

Scroll down the Bot page to the "Privileged Gateway Intents" heading. Enable the `MESSAGE CONTENT INTENT` toggle switch. Save your changes. The relay requires `MESSAGE CONTENT` access to read thread messages and route keystrokes to your terminal.

## 4. Create a Private Server

Open your Discord client and create a dedicated personal server. Retain exclusive membership so no other users join. Create a text channel for relay sessions, such as `#relay`.

## 5. Generate Invite URL and Authorize Bot

Return to the Discord Developer Portal and select "OAuth2", then open "URL Generator".

Under the "Scopes" checklist, select `bot`.

Under the "Bot Permissions" checklist, select the following permissions:
- Send Messages
- Create Public Threads
- Send Messages in Threads
- Manage Threads
- Read Message History
- Attach Files
- Embed Links

Copy the generated URL displayed at the bottom of the page. Paste the URL into your browser address bar. Choose your private server from the dropdown menu and confirm authorization.

## 6. Collect Channel ID and User ID

Open Discord User Settings, select "Advanced", and enable Developer Mode.

Right-click your designated relay text channel in the server sidebar. Select "Copy Channel ID". This numerical value supplies `DISCORD_CHANNEL_ID`.

Right-click your personal account username in the channel or server member list. Select "Copy User ID". This numerical value supplies `DISCORD_USER_ID`. The daemon drops events from every other user.

## 7. Write Environment Configuration

Create the configuration file at `~/.config/devpass-discord-relay/.env` or in the repository root as `.env`:

```env
# Required credentials
DISCORD_TOKEN=your_bot_token_here
DISCORD_CHANNEL_ID=123456789012345678
DISCORD_USER_ID=987654321098765432

# Optional runtime controls
APPROVAL_TIMEOUT_SECONDS=300
SESSION_TTL_DAYS=30
```

## 8. Install and Start the Service

Execute the installer script from the root of the repository:

```bash
./install.sh
```

The script builds the binaries, installs them to `~/.local/bin`, installs the systemd user service to `~/.config/systemd/user/devpass-discord-relay.service`, and starts the daemon.
