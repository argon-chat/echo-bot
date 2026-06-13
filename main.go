package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token          string `json:"token"`
	APIBase        string `json:"api_base"`
	IngressURL     string `json:"ingress_url"`
	BackgroundFile string `json:"background_file"`
	LogLevel       string `json:"log_level"`

	// FallbackLanguage is used when a caller's locale has no matching voice
	// bucket (and for unknown/missing locales). Defaults to "en".
	FallbackLanguage string `json:"fallback_language"`

	// VoicesDir is the base directory holding per-language preset folders.
	// Preset files are resolved as <VoicesDir>/<language>/<name>.{opus,json}.
	// Defaults to "voices".
	VoicesDir string `json:"voices_dir"`

	// Voices maps a language tag (e.g. "ru", "en") to its voice presets.
	// Each preset carries an optional rarity tier / weight controlling how
	// often it is picked. This is the preferred configuration format.
	Voices map[string][]VoicePreset `json:"voices"`

	// Variants is the legacy flat list of preset names. Kept for backward
	// compatibility: when Voices is empty, these are mapped to the fallback
	// language bucket with common rarity.
	Variants []string `json:"variants"`
}

type AudioMarkersJSON struct {
	Name    string `json:"name"`
	Codec   string `json:"codec"`
	Markers struct {
		Recording struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"recording"`
		Playback struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"playback"`
		Background struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"background"`
	} `json:"markers"`
}

type ParsedMarkers struct {
	RecordingStart  time.Duration
	RecordingEnd    time.Duration
	PlaybackStart   time.Duration
	PlaybackEnd     time.Duration
	BackgroundStart time.Duration
}

type Phase int

const (
	PhaseGreeting Phase = iota
	PhaseRecording
	PhaseTransition
	PhasePlayback
	PhaseOutro
	PhaseBackground
)

func (p Phase) String() string {
	return [...]string{"GREETING", "RECORDING", "TRANSITION", "PLAYBACK", "OUTRO", "BACKGROUND"}[p]
}

func (m ParsedMarkers) PhaseAt(elapsed time.Duration) Phase {
	switch {
	case elapsed >= m.BackgroundStart:
		return PhaseBackground
	case elapsed >= m.PlaybackEnd:
		return PhaseOutro
	case elapsed >= m.PlaybackStart:
		return PhasePlayback
	case elapsed >= m.RecordingEnd:
		return PhaseTransition
	case elapsed >= m.RecordingStart:
		return PhaseRecording
	default:
		return PhaseGreeting
	}
}

func parseTimestamp(ts string) (time.Duration, error) {
	if ts == "auto" || ts == "" {
		return 0, fmt.Errorf("auto/empty")
	}
	parts := strings.SplitN(ts, ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("bad format: %q", ts)
	}
	min, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	secParts := strings.SplitN(parts[1], ".", 2)
	sec, err := strconv.Atoi(secParts[0])
	if err != nil {
		return 0, err
	}
	ms := 0
	if len(secParts) == 2 {
		ms, _ = strconv.Atoi(secParts[1])
	}
	return time.Duration(min)*time.Minute + time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond, nil
}

func loadMarkers(path string) (ParsedMarkers, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ParsedMarkers{}, err
	}
	var raw AudioMarkersJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return ParsedMarkers{}, err
	}

	m := ParsedMarkers{}
	if m.RecordingStart, err = parseTimestamp(raw.Markers.Recording.Start); err != nil {
		return m, fmt.Errorf("recording.start: %w", err)
	}
	if m.RecordingEnd, err = parseTimestamp(raw.Markers.Recording.End); err != nil {
		return m, fmt.Errorf("recording.end: %w", err)
	}
	if m.PlaybackStart, err = parseTimestamp(raw.Markers.Playback.Start); err != nil {
		return m, fmt.Errorf("playback.start: %w", err)
	}
	if m.PlaybackEnd, err = parseTimestamp(raw.Markers.Playback.End); err != nil {
		return m, fmt.Errorf("playback.end: %w", err)
	}
	if bg, err := parseTimestamp(raw.Markers.Background.Start); err == nil {
		m.BackgroundStart = bg
	} else {
		m.BackgroundStart = m.PlaybackEnd
	}
	return m, nil
}

func loadConfig(path string) Config {
	cfg := Config{
		APIBase:          "https://gateway.argon.zone",
		IngressURL:       "ws://localhost:12880",
		Variants:         []string{"girl_echo_00", "girl_echo_01", "girl_echo_02", "girl_echo_03"},
		BackgroundFile:   "background.opus",
		LogLevel:         "info",
		FallbackLanguage: "en",
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func main() {
	cfg := loadConfig(env("CONFIG_FILE", "config.json"))

	// env vars take priority
	if v := os.Getenv("BOT_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("API_BASE"); v != "" {
		cfg.APIBase = v
	}
	if v := os.Getenv("INGRESS_URL"); v != "" {
		cfg.IngressURL = v
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}

	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if cfg.Token == "" {
		slog.Error("token is required (set BOT_TOKEN env or 'token' in config)")
		os.Exit(1)
	}

	voices := cfg.Voices
	var pathFor func(lang, name string) string
	if len(voices) > 0 {
		// New layout: <voices_dir>/<language>/<name>.{opus,json}
		voicesDir := cfg.VoicesDir
		if voicesDir == "" {
			voicesDir = "voices"
		}
		pathFor = func(lang, name string) string { return filepath.Join(voicesDir, lang, name) }
	} else {
		// Back-compat: a flat variants list of files at the repo root, mapped
		// to the fallback language bucket with common rarity.
		lang := cfg.FallbackLanguage
		if lang == "" {
			lang = "en"
		}
		presets := make([]VoicePreset, 0, len(cfg.Variants))
		for _, name := range cfg.Variants {
			presets = append(presets, VoicePreset{Name: name})
		}
		voices = map[string][]VoicePreset{lang: presets}
		pathFor = func(_, name string) string { return name }
		slog.Warn("config has no 'voices'; mapping legacy 'variants' to fallback language",
			"language", lang, "count", len(presets))
	}

	pool, err := LoadVoicePool(voices, cfg.FallbackLanguage, pathFor)
	if err != nil {
		slog.Error("failed to load voice pool", "error", err)
		os.Exit(1)
	}

	bgPackets, err := LoadOpusPackets(cfg.BackgroundFile)
	if err != nil {
		slog.Error("failed to load background audio", "error", err)
		os.Exit(1)
	}
	slog.Info("audio loaded", "languages", pool.Languages(), "bgPackets", len(bgPackets))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	bot := NewBot(cfg)

	me, err := bot.GetMe(ctx)
	if err != nil {
		slog.Error("auth failed", "error", err)
		os.Exit(1)
	}
	slog.Info("authenticated", "botId", me.BotID, "username", me.Username)

	bot.Run(ctx, pool, bgPackets)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
