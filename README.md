# Argon Echo Bot

A voice bot for [Argon](https://argon.gl) that answers calls, records the caller's speech, and plays it back — like a voice echo. After the echo it loops background music until the caller hangs up.

Built on the Argon Bot API and AudioWS (WebSocket audio gateway).

## How it works

Each audio variant (`girl_echo_*.opus`) is a pre-recorded script with timestamp markers (`.json`):

| Phase | Description |
|---|---|
| **Greeting** | Bot speaks an intro line |
| **Recording** | Bot listens while the caller talks |
| **Transition** | Short bridging line |
| **Playback** | Bot plays the caller's recorded audio back |
| **Outro** | Wrapping up |
| **Background** | Loops `background.opus` until hangup |

The bot uses a single duplex WebSocket to both publish and subscribe to audio.

## Quick start

```bash
cp config.yaml config.yaml   # edit token, api_base, ingress_url
export BOT_TOKEN="your-bot-token"
go run .
```

## Configuration

Edit `config.yaml` or use environment variables:

| Config key | Env var | Default | Description |
|---|---|---|---|
| `token` | `BOT_TOKEN` | — | Bot API token (required) |
| `api_base` | `API_BASE` | `https://gateway.argon.zone` | Argon API base URL |
| `ingress_url` | `INGRESS_URL` | `ws://localhost:12880` | AudioWS ingress URL |
| `log_level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `variants` | — | see config | List of audio variant names |
| `background_file` | — | `background.opus` | Background music file |

Environment variables take priority over config file values.

## Docker

```bash
docker build -t argon-echo-bot .
docker run -e BOT_TOKEN="your-token" argon-echo-bot
```

## Audio files

- `girl_echo_*.opus` — pre-recorded voice variants (OGG/Opus)
- `girl_echo_*.json` — timestamp markers for each variant
- `background.opus` — background music loop

You can replace these with your own recordings — just match the JSON marker format and update `config.yaml`.

## License

Source code is licensed under the [MIT License](LICENSE).

Voice recordings (`girl_echo_*`) are proprietary assets owned by Argon Inc. LLC and included for demo purposes only. `background.opus` is "Classical Music Uplifting Symphony Loop" by Sonican (royalty-free). See [LICENSE](LICENSE) for details.
