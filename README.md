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

## Voice selection

When a call comes in, the `CallIncoming` event carries the caller's language as
`fromLocale` (a BCP-47 tag like `ru`, `en`, `ja` — Bot API ≥ 1.10.0). The bot
uses it to pick a voice:

1. The locale is reduced to its primary language subtag (`en-US` → `en`).
2. If that language has a voice bucket, the bot picks from it. Otherwise it
   uses the `fallback_language` bucket (default `en`). If even that is empty, it
   falls back to any available voice so the call is always answered.
3. Within the chosen bucket, a voice is chosen by a **rarity-weighted** random
   pick, so rare presets surface only occasionally.

Configure voices per language under `voices`. Each preset can declare a
`rarity` tier — `common` (default), `uncommon`, `rare`, `legendary` — or an
explicit numeric `weight` that overrides the tier. Relative tier weights:
`common` 100, `uncommon` 30, `rare` 8, `legendary` 1 (higher = picked more
often).

Preset files live under `<voices_dir>/<language>/`, where `voices_dir` defaults
to `voices`. Each preset `name` resolves to two files in that language folder:
`<name>.opus` (the audio) and `<name>.json` (its phase markers). The same
`name` in two languages is two different recordings.

```
voices/
  ru/  girl_echo_00.opus  girl_echo_00.json  …
  en/  girl_echo_00.opus  girl_echo_00.json  …
```

```json
{
  "fallback_language": "en",
  "voices_dir": "voices",
  "voices": {
    "ru": [
      { "name": "girl_echo_00", "rarity": "rare" },
      { "name": "girl_echo_01" },
      { "name": "girl_echo_02", "rarity": "legendary" }
    ],
    "en": [
      { "name": "girl_echo_00" },
      { "name": "girl_echo_06", "rarity": "legendary" }
    ]
  }
}
```

## Quick start

```bash
cp config.json config.json   # edit token, api_base, ingress_url
export BOT_TOKEN="your-bot-token"
go run .
```

## Configuration

Edit `config.json` or use environment variables:

| Config key | Env var | Default | Description |
|---|---|---|---|
| `token` | `BOT_TOKEN` | — | Bot API token (required) |
| `api_base` | `API_BASE` | `https://gateway.argon.zone` | Argon API base URL |
| `ingress_url` | `INGRESS_URL` | `ws://localhost:12880` | AudioWS ingress URL |
| `log_level` | `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `fallback_language` | — | `en` | Language bucket used for unknown/missing caller locales |
| `voices_dir` | — | `voices` | Base directory holding per-language preset folders |
| `voices` | — | see config | Per-language voice presets (see [Voice selection](#voice-selection)) |
| `background_file` | — | `background.opus` | Background music file |

Environment variables take priority over config file values.

> **Legacy `variants`:** a flat `"variants": ["girl_echo_00", …]` list is still
> accepted. When `voices` is omitted, those names are mapped into the
> `fallback_language` bucket with common rarity.

## Docker

```bash
docker build -t argon-echo-bot .
docker run -e BOT_TOKEN="your-token" argon-echo-bot
```

## Audio files

- `voices/<lang>/girl_echo_*.opus` — pre-recorded voice presets (OGG/Opus, 48 kHz mono)
- `voices/<lang>/girl_echo_*.json` — timestamp markers for each preset
- `background.opus` — background music loop

You can replace these with your own recordings — just match the JSON marker format and update the `voices` map in `config.json`.

> The bot only ships `.opus`. If you keep lossless `.flac` masters in the
> `voices/` tree they are git-ignored; transcode them to 48 kHz mono Opus, e.g.
> `ffmpeg -i in.flac -c:a libopus -b:a 48k -ar 48000 -ac 1 -frame_duration 20 out.opus`.

## License

Source code is licensed under the [MIT License](LICENSE).

Voice recordings (`girl_echo_*`) are proprietary assets owned by Argon Inc. LLC and included for demo purposes only. `background.opus` is "Classical Music Uplifting Symphony Loop" by Sonican (royalty-free). See [LICENSE](LICENSE) for details.
