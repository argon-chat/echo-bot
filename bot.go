package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// replaceHost swaps the scheme+host of srcURL with those from baseURL, keeping path and query.
func replaceHost(srcURL, baseURL string) string {
	s, err := url.Parse(srcURL)
	if err != nil {
		return srcURL
	}
	b, err := url.Parse(baseURL)
	if err != nil {
		return srcURL
	}
	s.Scheme = b.Scheme
	s.Host = b.Host
	return s.String()
}

type BotSelfResponse struct {
	BotID       string `json:"botId"`
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
}

type AcceptCallResponse struct {
	Token        string `json:"token"`
	RoomName     string `json:"roomName"`
	CallerID     string `json:"callerId"`
	AudioBaseUrl string `json:"audioBaseUrl"`
}

type CallIncomingEvent struct {
	CallID     string `json:"callId"`
	FromUserID string `json:"fromUserId"`
}

type CallEndedEvent struct {
	CallID string `json:"callId"`
}

type Bot struct {
	cfg         Config
	client      *http.Client
	variants    []EchoVariant
	bgPackets   []OpusPacket
	activeCalls sync.Map     // callID → context.CancelFunc
	callCount   atomic.Int64 // number of active call handlers
}

func NewBot(cfg Config) *Bot {
	return &Bot{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (b *Bot) apiURL(iface string, version int, method string) string {
	return fmt.Sprintf("%s/%s/v%d/%s", b.cfg.APIBase, iface, version, method)
}

func (b *Bot) doGet(ctx context.Context, url string, out any) error {
	slog.Debug("HTTP GET", "url", url)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+b.cfg.Token)

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	slog.Debug("HTTP GET response", "url", url, "status", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *Bot) doPost(ctx context.Context, url string, payload any, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	slog.Debug("HTTP POST", "url", url, "body", string(data))
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+b.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	slog.Debug("HTTP POST response", "url", url, "status", resp.StatusCode)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// GetMe verifies bot authentication.
func (b *Bot) GetMe(ctx context.Context) (*BotSelfResponse, error) {
	var me BotSelfResponse
	err := b.doGet(ctx, b.apiURL("IBotSelf", 1, "GetMe"), &me)
	return &me, err
}

// AcceptCall accepts an incoming call and returns a stream token.
func (b *Bot) AcceptCall(ctx context.Context, callID string) (*AcceptCallResponse, error) {
	var resp AcceptCallResponse
	err := b.doPost(ctx, b.apiURL("ICalls", 20260401, "Accept"),
		map[string]string{"callId": callID}, &resp)
	return &resp, err
}

// Run connects to the SSE event stream and handles incoming calls.
func (b *Bot) Run(ctx context.Context, variants []EchoVariant, bgPackets []OpusPacket) {
	b.variants = variants
	b.bgPackets = bgPackets
	const allIntents = 16383

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		default:
		}

		slog.Info("connecting to event stream...")
		err := b.streamEvents(ctx, allIntents)
		if err != nil && ctx.Err() == nil {
			slog.Error("event stream disconnected, reconnecting in 3s", "error", err)
			time.Sleep(3 * time.Second)
		}
	}
}

func (b *Bot) streamEvents(ctx context.Context, intents int) error {
	sseURL := fmt.Sprintf("%s?intents=%d", b.apiURL("IEvents", 1, "Stream"), intents)

	slog.Info("SSE connecting", "url", sseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+b.cfg.Token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	sseClient := &http.Client{Timeout: 0}
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	slog.Info("SSE response", "status", resp.StatusCode, "contentType", resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("SSE HTTP %d: %s", resp.StatusCode, body)
	}
	slog.Info("event stream connected")

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType, eventData string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if eventType != "" && eventData != "" {
				b.handleEvent(ctx, eventType, eventData)
			}
			eventType = ""
			eventData = ""
			continue
		}

		if strings.HasPrefix(line, "event: ") {
			eventType = line[7:]
		} else if strings.HasPrefix(line, "data: ") {
			eventData = line[6:]
		}
	}

	return scanner.Err()
}

func (b *Bot) handleEvent(ctx context.Context, eventType, data string) {
	switch eventType {
	case "ready":
		slog.Info("SSE ready", "data", data)

	case "heartbeat":
		slog.Debug("heartbeat")

	case "callIncoming":
		var ev CallIncomingEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			slog.Error("failed to parse callIncoming", "error", err)
			return
		}
		slog.Info("incoming call", "callId", ev.CallID, "from", ev.FromUserID)

		callCtx, callCancel := context.WithCancel(ctx)
		b.activeCalls.Store(ev.CallID, callCancel)
		go func() {
			defer callCancel()
			defer b.activeCalls.Delete(ev.CallID)
			b.handleCall(callCtx, ev)
		}()

	case "callEnded":
		var ev CallEndedEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			slog.Error("failed to parse callEnded", "error", err)
			return
		}
		if cancel, ok := b.activeCalls.LoadAndDelete(ev.CallID); ok {
			cancel.(context.CancelFunc)()
		}
		slog.Info("call ended", "callId", ev.CallID)

	default:
		slog.Debug("unhandled event", "type", eventType)
	}
}

func (b *Bot) handleCall(ctx context.Context, ev CallIncomingEvent) {
	n := b.callCount.Add(1)
	defer b.callCount.Add(-1)

	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in handleCall", "callId", ev.CallID, "panic", r)
		}
	}()

	callLog := slog.With("callId", ev.CallID, "from", ev.FromUserID, "activeCalls", n)

	v := b.variants[rand.Intn(len(b.variants))]
	callLog.Info("selected variant", "variant", v.Name)

	resp, err := b.AcceptCall(ctx, ev.CallID)
	if err != nil {
		callLog.Error("failed to accept call", "error", err)
		return
	}
	callLog.Info("call accepted", "room", resp.RoomName, "caller", resp.CallerID)

	audioBase := replaceHost(resp.AudioBaseUrl, b.cfg.IngressURL)
	err = StreamDuplex(ctx, audioBase, resp.Token, resp.CallerID, v.Packets, v.Markers, b.bgPackets, callLog)
	if err != nil {
		callLog.Error("duplex stream ended with error", "error", err)
		return
	}
	callLog.Info("audio playback complete")
}
