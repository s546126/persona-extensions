# Telegram Bot

[Extension development](../../docs/development.md) · [Documentation](../../docs/extensions/telegram-bot.md)

A personal Telegram Bot API channel for Persona. Create your own bot with `@BotFather`, enter the bot token (and an optional HTTP/HTTPS proxy) in **Settings → Connectors → Telegram 机器人**, and send the 6-digit pairing code to your bot in a private Telegram chat.

- **Owner-only private DM**: Only the single bound Telegram user's private chat reaches Persona's main chat; groups are ignored and other senders receive a rate-limited notice.
- **Rich round-trip**: Text, quoted replies, photos (`<= 5 MiB`), files (`<= 10 MiB`), typing indicators, proactive pushes, and chunked plain-text replies (`4,000` characters).
- **Independent Go binary**: Uses the official Go extension SDK and standard library only (`api.telegram.org`), with credentials stored encrypted in the hub's connector secrets.

## Development

```sh
go -C extensions/telegram-bot test -race ./...
scripts/package.sh telegram-bot
```
