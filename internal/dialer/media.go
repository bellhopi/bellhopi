package dialer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/enetx/g"
)

type mediaEvent struct {
	Event    g.String `json:"event"`
	StreamID g.String `json:"stream_id"`
	Start    struct {
		ControlID   g.String `json:"call_control_id"`
		ClientState g.String `json:"client_state"`
		From        g.String `json:"from"`
		To          g.String `json:"to"`
		Format      struct {
			Encoding g.String `json:"encoding"`
			Rate     g.Int    `json:"sample_rate"`
			Channels g.Int    `json:"channels"`
		} `json:"media_format"`
	} `json:"start"`
	Media struct {
		Track   g.String `json:"track"`
		Chunk   g.String `json:"chunk"`
		Payload g.String `json:"payload"`
	} `json:"media"`
}

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	id := g.String(r.PathValue("id"))
	if !idPattern.MatchString(id.Std()) {
		http.NotFound(w, r)
		return
	}
	streamURL := s.cfg.PublicURL.TrimEndSet("/").Replace("https://", "wss://", 1) + g.String(r.URL.RequestURI())
	if !verifySignature(s.cfg.TelnyxPublicKey, r.Header, streamURL.Bytes(), time.Now()) {
		failHTTP(w, 401, errors.New("invalid Telnyx media signature"))
		return
	}
	found := s.lookup(id)
	if found.IsErr() {
		http.NotFound(w, r)
		return
	}
	c := found.Unwrap()
	c.mu.Lock()
	reject := c.stopRequested || terminal(c.r.Snapshot.Status) || c.mediaConn != nil
	c.mu.Unlock()
	if reject {
		failHTTP(w, 409, errors.New("call is not accepting a media connection"))
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(65536)
	c.mu.Lock()
	if c.mediaConn != nil || c.stopRequested {
		c.mu.Unlock()
		conn.CloseNow()
		return
	}
	c.mediaConn = conn
	c.mu.Unlock()
	defer func() {
		conn.CloseNow()
		c.mu.Lock()
		c.r.Snapshot.MediaConnected = false
		done := c.stopRequested
		_ = c.saveLocked()
		c.mu.Unlock()
		if !done {
			_ = c.stop("Telnyx media disconnected")
		}
	}()
	incoming := make(chan audioPacket, 64)
	go c.readAudio(incoming)
	stream := g.String("")
	started := false
	for {
		_, raw, err := conn.Read(c.ctx)
		if err != nil {
			return
		}
		var e mediaEvent
		if json.Unmarshal(raw, &e) != nil {
			return
		}
		switch e.Event {
		case "connected":
			continue
		case "start":
			c.mu.Lock()
			valid := !started && !e.StreamID.IsEmpty() && !e.Start.ControlID.IsEmpty() && e.Start.Format.Encoding == "PCMU" && e.Start.Format.Rate == 8000 && e.Start.Format.Channels == 1 && e.Start.To == c.r.Snapshot.To && e.Start.From == s.cfg.From && (c.r.ControlID == e.Start.ControlID || (c.r.ControlID.IsEmpty() && e.Start.ClientState == g.String(base64.StdEncoding.EncodeToString(id.Bytes()))))
			if valid {
				c.r.ControlID = e.Start.ControlID
				c.r.Snapshot.MediaConnected = true
				c.eventLocked("media", "Telnyx PCMU stream connected")
				err = c.saveLocked()
			}
			c.mu.Unlock()
			if !valid || err != nil {
				return
			}
			started = true
			stream = e.StreamID
			c.mediaOnce.Do(func() { close(c.mediaReady) })
		case "media":
			if !started || e.StreamID != stream {
				return
			}
			if e.Media.Track != "inbound" {
				continue
			}
			chunk, err := strconv.ParseUint(e.Media.Chunk.Std(), 10, 64)
			if err != nil || chunk == 0 {
				return
			}
			b, err := base64.StdEncoding.DecodeString(e.Media.Payload.Std())
			if err != nil || len(b) == 0 || len(b) > 8000 {
				return
			}
			select {
			case incoming <- audioPacket{Sequence: chunk, Payload: e.Media.Payload}:
			case <-c.ctx.Done():
				return
			default:
				return
			}
		case "stop", "error":
			return
		}
	}
}

type audioPacket struct {
	Sequence uint64
	Payload  g.String
}

// shortcut: a 120 ms reorder window tolerates short gaps; use a jitter-buffer library for lossy long-haul audio.
func (c *call) readAudio(input <-chan audioPacket) {
	pending := g.NewMap[uint64, g.String]()
	next := uint64(1)
	last := time.Now()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case p := <-input:
			if p.Sequence >= next {
				pending[p.Sequence] = p.Payload
			}
		case <-tick.C:
			if len(pending) > 0 && time.Since(last) > 120*time.Millisecond {
				smallest := ^uint64(0)
				pending.Iter().ForEach(func(n uint64, _ g.String) {
					if n < smallest {
						smallest = n
					}
				})
				next = smallest
			}
		}
		if len(pending) > 64 {
			c.liveFailure("inbound audio reorder window overflow")
			return
		}
		for {
			payload, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			last = time.Now()
			if err := c.sendLive(g.Map[g.String, any]{"type": "session.input_audio.append", "audio": payload}); err != nil {
				c.liveFailure("cannot send call audio to GPT-Live")
				return
			}
		}
	}
}
