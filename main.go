package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token          string   `json:"token"`
	APIBase        string   `json:"api_base"`
	IngressURL     string   `json:"ingress_url"`
	Variants       []string `json:"variants"`
	BackgroundFile string   `json:"background_file"`
	LogLevel       string   `json:"log_level"`
}

type EchoVariant struct {
	Name    string
	Packets []OpusPacket
	Markers ParsedMarkers
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
		APIBase:        "https://gateway.argon.zone",
		IngressURL:     "ws://localhost:12880",
		Variants:       []string{"girl_echo_00", "girl_echo_01", "girl_echo_02", "girl_echo_03"},
		BackgroundFile: "background.opus",
		LogLevel:       "info",
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func main() {
	cfg := loadConfig(env("CONFIG_FILE", "config.yaml"))

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

	var variants []EchoVariant
	for _, name := range cfg.Variants {
		markers, err := loadMarkers(name + ".json")
		if err != nil {
			slog.Warn("skipping variant", "name", name, "error", err)
			continue
		}
		packets, err := LoadOpusPackets(name + ".opus")
		if err != nil {
			slog.Warn("skipping variant", "name", name, "error", err)
			continue
		}
		var dur time.Duration
		for _, p := range packets {
			dur += p.Duration
		}
		slog.Info("variant loaded", "name", name, "packets", len(packets), "duration", dur.Round(time.Millisecond))
		variants = append(variants, EchoVariant{Name: name, Packets: packets, Markers: markers})
	}
	if len(variants) == 0 {
		slog.Error("no echo variants loaded")
		os.Exit(1)
	}

	bgPackets, err := LoadOpusPackets(cfg.BackgroundFile)
	if err != nil {
		slog.Error("failed to load background audio", "error", err)
		os.Exit(1)
	}
	slog.Info("audio loaded", "variants", len(variants), "bgPackets", len(bgPackets))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	bot := NewBot(cfg)

	me, err := bot.GetMe(ctx)
	if err != nil {
		slog.Error("auth failed", "error", err)
		os.Exit(1)
	}
	slog.Info("authenticated", "botId", me.BotID, "username", me.Username)

	bot.Run(ctx, variants, bgPackets)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
