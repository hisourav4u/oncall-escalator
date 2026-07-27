# oncall-escalator

A small Slack bot: when developers tag your on-call usergroup (e.g.
`@devops-oncall`) in a channel and **nobody acknowledges within N minutes**, the
bot automatically tags an escalation handle in that same thread.

Standalone today; designed to fold into the [oncall](../oncall) service later
(shared users/schedules would let "who's on-call" drive both the responder set
and the escalation target).

## How it works

```
@devops-oncall mentioned  ──►  timer armed (default 30m)
        │
        ├─ someone authorized replies "ack"/"looking"/… OR reacts ✅/👀  ──►  timer cancelled
        │
        └─ timeout with no ack  ──►  bot replies in-thread: "⏰ escalating to @devops …"
```

- **Trigger**: a mention of a watched Slack usergroup (matched by subteam ID, so
  renaming the handle doesn't break it). Optional literal keyword triggers too.
- **Acknowledgement** (either cancels the timer), and only from an **authorized
  responder**:
  - a **thread reply** matching an ack keyword — `ack`, `acked`, `looking`,
    `on it`, `taking`, … (single words match whole-word, so "slack"/"feedback"
    don't false-trigger), or
  - an **ack reaction** — `✅` `white_check_mark`, `👀` `eyes`, `+1`, …
- **Authorized responders**: the union of `RESPONDER_USER_IDS`,
  `RESPONDER_EMAILS`, and members of `RESPONDER_USERGROUP_IDS` (refreshed every
  10 min). Leave all blank to accept anyone.
- **Escalation**: single tier — tags `ESCALATION_TARGETS` (a usergroup, users,
  or emails) in the thread. Escalation is only ever posted once; a failed Slack
  post is retried on the next tick.

State lives in SQLite, so pending timers survive a restart.

## Slack app setup

1. Create an app at api.slack.com/apps.
2. **OAuth & Permissions → Bot Token Scopes**: `chat:write`, `channels:history`,
   `groups:history`, `reactions:read`, `usergroups:read`, `users:read`,
   `users:read.email`. Install → copy the **Bot User OAuth Token** (`xoxb-…`).
3. **Event Subscriptions**: turn on, set the Request URL to
   `https://<host>/slack/events` (the app answers Slack's verification
   handshake). Subscribe to bot events: `message.channels`, `message.groups`,
   `reaction_added`.
4. Copy the **Signing Secret** → `SLACK_SIGNING_SECRET`.
5. `/invite` the bot into the channels where `@devops-oncall` gets tagged.

Find usergroup IDs:
```bash
curl -s -H "Authorization: Bearer $SLACK_BOT_TOKEN" \
  https://slack.com/api/usergroups.list | jq '.usergroups[]|{id,handle}'
```

## Run

```bash
cp .env.example .env    # fill in token, watch group, escalation target
docker compose up --build -d
curl localhost:8090/healthz
```

Or locally: `go build -o bin/bot ./cmd/bot && ./bin/bot` with the env vars set.

## Configuration (env)

| Var | Default | Meaning |
|-----|---------|---------|
| `SLACK_BOT_TOKEN` | — (required) | Bot token |
| `SLACK_SIGNING_SECRET` | — | Verifies events (recommended) |
| `WATCH_USERGROUP_IDS` | — | Subteam IDs that trigger a timer (comma-sep) |
| `WATCH_KEYWORDS` | — | Optional literal text triggers |
| `ACK_TIMEOUT` | `30m` | Wait before escalating |
| `ESCALATION_TARGETS` | — (required) | Who to tag: `S…`/`U…`/email, comma-sep |
| `RESPONDER_USERGROUP_IDS` | — | Usergroups whose members may ack |
| `RESPONDER_EMAILS` | — | Emails that may ack |
| `RESPONDER_USER_IDS` | — | User IDs that may ack |
| `ACK_KEYWORDS` | ack,looking,on it,… | Reply phrases that ack |
| `ACK_EMOJIS` | white_check_mark,eyes,… | Reactions that ack |
| `ACK_ANY_REPLY_USER_IDS` | *(empty)* | Users whose any thread reply acks, whatever it says |
| `TICK_INTERVAL` | `15s` | How often the escalation timer scans |

## Tests

```bash
go test ./...
```

Covers trigger detection, whole-word ack matching, authorized-only acks,
reaction acks, the double-escalation guard, and retry-on-failed-post.
