package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// OpusPacket is a single Opus audio frame with its duration.
type OpusPacket struct {
	Data     []byte
	Duration time.Duration
}

// RecordBuffer is a thread-safe buffer for recorded audio frames.
type RecordBuffer struct {
	mu             sync.Mutex
	frames         [][]byte
	recordStartIdx int // index where actual recording phase begins
}

func NewRecordBuffer() *RecordBuffer {
	return &RecordBuffer{}
}

func (rb *RecordBuffer) Append(frame []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	cp := make([]byte, len(frame))
	copy(cp, frame)
	rb.frames = append(rb.frames, cp)
}

// MarkRecordingStart records the current buffer length as the start of the
// actual recording phase. Frames before this index are discarded on Snapshot.
func (rb *RecordBuffer) MarkRecordingStart() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.recordStartIdx = len(rb.frames)
}

// Snapshot returns only the frames captured since MarkRecordingStart.
func (rb *RecordBuffer) Snapshot() [][]byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	start := rb.recordStartIdx
	if start > len(rb.frames) {
		start = len(rb.frames)
	}
	out := make([][]byte, len(rb.frames)-start)
	copy(out, rb.frames[start:])
	return out
}

// Len returns the total number of frames in the buffer.
func (rb *RecordBuffer) Len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return len(rb.frames)
}

// RecordedLen returns the number of frames since MarkRecordingStart.
func (rb *RecordBuffer) RecordedLen() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	start := rb.recordStartIdx
	if start > len(rb.frames) {
		return 0
	}
	return len(rb.frames) - start
}

// LoadOpusPackets reads an OGG/Opus file and extracts raw Opus packets.
func LoadOpusPackets(path string) ([]OpusPacket, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}
	if string(magic) != "OggS" {
		return nil, fmt.Errorf("not an OGG file (magic: %x)", magic)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	reader := &oggReader{r: f}
	return reader.readAllPackets()
}

type oggReader struct {
	r       io.Reader
	partial []byte
}

func (o *oggReader) readAllPackets() ([]OpusPacket, error) {
	var packets []OpusPacket
	pageIdx := 0

	for {
		segTable, data, err := o.readPage()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", pageIdx, err)
		}

		if pageIdx < 2 {
			pageIdx++
			continue
		}

		offset := 0
		for _, segLen := range segTable {
			sl := int(segLen)
			o.partial = append(o.partial, data[offset:offset+sl]...)
			offset += sl

			if segLen < 255 {
				if len(o.partial) > 0 {
					pkt := make([]byte, len(o.partial))
					copy(pkt, o.partial)
					if !isValidOpusPacket(pkt) {
						slog.Warn("skipping invalid opus packet from OGG",
							"page", pageIdx,
							"size", len(pkt))
						o.partial = o.partial[:0]
						continue
					}
					dur := opusPacketDuration(pkt)
					packets = append(packets, OpusPacket{Data: pkt, Duration: dur})
				}
				o.partial = o.partial[:0]
			}
		}
		pageIdx++
	}
	return packets, nil
}

func (o *oggReader) readPage() (segTable []byte, data []byte, err error) {
	var magic [4]byte
	if _, err = io.ReadFull(o.r, magic[:]); err != nil {
		return nil, nil, err
	}
	if string(magic[:]) != "OggS" {
		return nil, nil, fmt.Errorf("bad sync (got %x)", magic)
	}

	var hdr [23]byte
	if _, err = io.ReadFull(o.r, hdr[:]); err != nil {
		return nil, nil, err
	}
	_ = binary.LittleEndian

	numSegments := int(hdr[22])
	segTable = make([]byte, numSegments)
	if _, err = io.ReadFull(o.r, segTable); err != nil {
		return nil, nil, err
	}

	totalSize := 0
	for _, s := range segTable {
		totalSize += int(s)
	}

	data = make([]byte, totalSize)
	if _, err = io.ReadFull(o.r, data); err != nil {
		return nil, nil, err
	}
	return segTable, data, nil
}

// opusPacketDuration computes the duration of an Opus packet from its TOC byte (RFC 6716).
func opusPacketDuration(pkt []byte) time.Duration {
	if len(pkt) == 0 {
		return 20 * time.Millisecond
	}

	toc := pkt[0]
	config := toc >> 3
	frameCode := toc & 0x03

	var frameSizeUs int
	switch {
	case config <= 11:
		frameSizeUs = [4]int{10000, 20000, 40000, 60000}[config%4]
	case config <= 15:
		frameSizeUs = [2]int{10000, 20000}[config%2]
	default:
		frameSizeUs = [4]int{2500, 5000, 10000, 20000}[config%4]
	}

	numFrames := 1
	switch frameCode {
	case 0:
		numFrames = 1
	case 1, 2:
		numFrames = 2
	case 3:
		if len(pkt) >= 2 {
			numFrames = int(pkt[1]) & 0x3F
		}
	}

	return time.Duration(frameSizeUs*numFrames) * time.Microsecond
}

// Opus silence frame (valid Opus packet that decodes to silence).
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// isValidOpusPacket validates an Opus frame per RFC 6716.
func isValidOpusPacket(pkt []byte) bool {
	if len(pkt) < 1 {
		return false
	}
	toc := pkt[0]
	frameCode := toc & 0x03
	payload := pkt[1:] // bytes after TOC

	switch frameCode {
	case 0:
		return len(payload) >= 1

	case 1:
		// Two equal-size CBR frames
		return len(payload) >= 2 && len(payload)%2 == 0

	case 2:
		// Two VBR frames
		if len(payload) < 1 {
			return false
		}
		// Frame 1 size (1 or 2 byte encoding, RFC 6716 §3.2.1)
		size1 := int(payload[0])
		headerLen := 1
		if size1 >= 252 {
			if len(payload) < 2 {
				return false
			}
			size1 = int(payload[0]) + int(payload[1])*4
			headerLen = 2
		}
		remaining := len(payload) - headerLen
		return remaining >= size1

	case 3:
		// Arbitrary N frames
		if len(payload) < 1 {
			return false
		}
		countByte := payload[0]
		M := int(countByte) & 0x3F
		vbr := countByte&0x80 != 0
		hasPadding := countByte&0x40 != 0

		if M == 0 {
			return false // 0 frames declared = invalid
		}

		offset := 1 // past count_byte

		// Skip padding length bytes
		if hasPadding {
			for offset < len(payload) {
				b := payload[offset]
				offset++
				if b < 255 {
					break
				}
			}
		}

		// VBR: M-1 frame size fields
		if vbr {
			for i := 0; i < M-1; i++ {
				if offset >= len(payload) {
					return false
				}
				if payload[offset] >= 252 {
					offset += 2 // two-byte size
				} else {
					offset++
				}
			}
		}

		return offset <= len(payload)
	}
	return false
}

// StreamDuplex opens a duplex WebSocket that publishes audio and records
// incoming frames. Recorded audio is echoed back during the playback phase.
func StreamDuplex(
	ctx context.Context,
	audioBaseURL, token, callerIdentity string,
	packets []OpusPacket,
	markers ParsedMarkers,
	bgPackets []OpusPacket,
	log *slog.Logger,
) error {
	u, err := url.Parse(audioBaseURL)
	if err != nil {
		return fmt.Errorf("bad audio base URL: %w", err)
	}

	u.Path = "/audio/duplex"
	q := u.Query()
	q.Set("token", token)
	q.Set("target_identity", callerIdentity)
	q.Set("target_track_source", "microphone")
	q.Set("stereo", "false")
	q.Set("frame_duration_ms", "20")
	u.RawQuery = q.Encode()

	log.Info("connecting to audio duplex", "url", u.String())

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("duplex ws connect: %w", err)
	}
	defer conn.Close()

	recorded := NewRecordBuffer()

	type wsStatus struct {
		Status              string `json:"status"`
		SessionID           string `json:"session_id,omitempty"`
		TrackSID            string `json:"track_sid,omitempty"`
		ParticipantIdentity string `json:"participant_identity,omitempty"`
		Message             string `json:"message,omitempty"`
	}
	statusCh := make(chan wsStatus, 8)
	callerLeft := make(chan struct{}, 1)

	wsClosed := make(chan struct{})
	var rxFrameCount int64
	go func() {
		defer close(wsClosed)
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				log.Debug("ws read error", "error", err)
				return
			}
			switch msgType {
			case websocket.BinaryMessage:
				if len(data) > 0 {
					rxFrameCount++
					if rxFrameCount <= 3 || rxFrameCount%500 == 0 {
						log.Debug("rx audio frame",
							"seq", rxFrameCount,
							"size", len(data),
							"toc", fmt.Sprintf("0x%02x", data[0]),
							"opusDur", opusPacketDuration(data).String())
					}
					// Skip DTX/comfort noise frames
					if len(data) <= 2 {
						continue
					}
					recorded.Append(data)
				}
			case websocket.TextMessage:
				var msg wsStatus
				if err := json.Unmarshal(data, &msg); err != nil || msg.Status == "" {
					log.Debug("duplex text message", "raw", string(data))
					continue
				}
				log.Info("duplex status",
					"status", msg.Status,
					"sessionId", msg.SessionID,
					"trackSid", msg.TrackSID,
					"participant", msg.ParticipantIdentity,
					"message", msg.Message)
				if msg.Status == "target_left" {
					select {
					case callerLeft <- struct{}{}:
					default:
					}
				}
				select {
				case statusCh <- msg:
				default:
				}
			}
		}
	}()

	// Wait for "subscribed" before starting playback
	log.Info("waiting for duplex subscribed...")
	gotReady := false
	for {
		select {
		case st := <-statusCh:
			switch st.Status {
			case "ready":
				gotReady = true
				log.Info("duplex ready (publish ok)", "sessionId", st.SessionID)
			case "subscribed":
				log.Info("duplex subscribed (can hear caller)",
					"sessionId", st.SessionID,
					"trackSid", st.TrackSID,
					"participant", st.ParticipantIdentity)
				goto startStream
			case "error":
				return fmt.Errorf("duplex error: %s", st.Message)
			case "target_left":
				return fmt.Errorf("caller left before stream started")
			default:
				log.Info("duplex status (waiting)", "status", st.Status)
			}
		case <-wsClosed:
			return fmt.Errorf("duplex closed before subscribed")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
			if gotReady {
				log.Warn("duplex subscribed timeout — proceeding with ready only")
			} else {
				log.Warn("duplex timeout — no ready/subscribed received, proceeding anyway")
			}
			goto startStream
		}
	}
startStream:

	log.Info("streaming audio (duplex)", "packets", len(packets))

	start := time.Now()
	var elapsed time.Duration
	prevPhase := Phase(-1)

	var playbackFrames [][]byte
	playbackIdx := 0

	for i, pkt := range packets {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wsClosed:
			log.Info("duplex closed by server")
			return nil
		default:
		}

		elapsed += pkt.Duration
		phase := markers.PhaseAt(elapsed)

		if phase != prevPhase {
			switch phase {
			case PhaseRecording:
				recorded.MarkRecordingStart()
				log.Info("phase: recording",
					"at", elapsed.Round(time.Millisecond),
					"preRecordFrames", recorded.Len())
			case PhaseTransition:
				log.Info("phase: transition",
					"at", elapsed.Round(time.Millisecond),
					"recorded", recorded.RecordedLen())
			case PhasePlayback:
				playbackFrames = recorded.Snapshot()
				playbackIdx = 0
				log.Info("phase: playback",
					"at", elapsed.Round(time.Millisecond),
					"frames", len(playbackFrames))
			case PhaseOutro:
				log.Info("phase: outro", "at", elapsed.Round(time.Millisecond))
			case PhaseBackground:
				log.Info("phase: background", "at", elapsed.Round(time.Millisecond))
			default:
				log.Info("phase: greeting", "at", elapsed.Round(time.Millisecond))
			}
			prevPhase = phase
		}

		// During playback, inject recorded frames
		frameToSend := pkt.Data
		if phase == PhasePlayback && playbackFrames != nil && playbackIdx < len(playbackFrames) {
			frameToSend = playbackFrames[playbackIdx]
			playbackIdx++
		}

		// Validate Opus structure before sending
		if !isValidOpusPacket(frameToSend) {
			tocInfo := "empty"
			if len(frameToSend) > 0 {
				tocInfo = fmt.Sprintf("toc=0x%02x code=%d", frameToSend[0], frameToSend[0]&0x03)
			}
			hexDump := fmt.Sprintf("%x", frameToSend)
			if len(hexDump) > 40 {
				hexDump = hexDump[:40] + "..."
			}
			log.Warn("replacing invalid opus frame with silence",
				"packet", i,
				"size", len(frameToSend),
				"phase", phase.String(),
				"toc", tocInfo,
				"hex", hexDump,
				"isPlayback", phase == PhasePlayback,
				"playbackIdx", playbackIdx-1)
			frameToSend = opusSilence
		}

		if err := conn.WriteMessage(websocket.BinaryMessage, frameToSend); err != nil {
			return fmt.Errorf("ws write packet %d: %w", i, err)
		}

		// Precise sleep
		target := start.Add(elapsed)
		if wait := time.Until(target); wait > 0 {
			time.Sleep(wait)
		}
	}

	log.Info("main audio complete, entering background loop",
		"duration", elapsed.Round(time.Millisecond),
		"playbackInjected", playbackIdx,
		"playbackAvailable", len(playbackFrames),
		"totalRxFrames", recorded.Len())

	// Background music loop — play until caller hangs up (target_left or WS close)
	if len(bgPackets) > 0 {
		loopCount := 0
		for {
			loopCount++
			log.Info("background loop", "iteration", loopCount)

			for _, pkt := range bgPackets {
				select {
				case <-ctx.Done():
					goto cleanup
				case <-wsClosed:
					log.Info("duplex closed during background")
					goto cleanup
				case <-callerLeft:
					log.Info("caller left during background")
					goto cleanup
				default:
				}

				frameToSend := pkt.Data
				if !isValidOpusPacket(frameToSend) {
					frameToSend = opusSilence
				}

				if err := conn.WriteMessage(websocket.BinaryMessage, frameToSend); err != nil {
					log.Debug("bg write error", "error", err)
					goto cleanup
				}

				elapsed += pkt.Duration
				target := start.Add(elapsed)
				if wait := time.Until(target); wait > 0 {
					time.Sleep(wait)
				}
			}
		}
	} else {
		log.Info("no background audio, waiting for caller to leave")
		select {
		case <-callerLeft:
			log.Info("caller left")
		case <-wsClosed:
			log.Info("duplex closed")
		case <-ctx.Done():
		}
	}

cleanup:
	log.Info("duplex streaming complete", "totalDuration", elapsed.Round(time.Millisecond))

	_ = conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	select {
	case <-wsClosed:
	case <-time.After(2 * time.Second):
	}

	return nil
}
