package dialer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/enetx/g"
)

type call struct {
	s             *Server
	mu            sync.Mutex
	r             receipt
	request       Request
	ctx           context.Context
	cancel        context.CancelFunc
	live          *websocket.Conn
	mediaConn     *websocket.Conn
	ready         chan error
	closed        chan struct{}
	closeOnce     sync.Once
	finalizeOnce  sync.Once
	stopRequested bool
	answered      bool
	seen          g.Map[g.String, bool]
	questionReply chan g.String
	toolMu        sync.Mutex
	outbound      chan g.Bytes
	mediaReady    chan struct{}
	mediaOnce     sync.Once
}

func newCall(s *Server, request Request, r receipt) *call {
	ctx, cancel := context.WithCancel(context.Background())
	return &call{s: s, r: r, request: request, ctx: ctx, cancel: cancel, ready: make(chan error, 1), closed: make(chan struct{}), seen: g.NewMap[g.String, bool](), outbound: make(chan g.Bytes, 64), mediaReady: make(chan struct{})}
}

func (c *call) snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.r.Snapshot
	s.Events = s.Events.Clone()
	s.Transcript = s.Transcript.Clone()
	if s.Question != nil {
		q := *s.Question
		s.Question = &q
	}
	if s.Result != nil {
		v := *s.Result
		s.Result = &v
	}
	return s
}
func (c *call) saveLocked() error {
	c.r.Snapshot.UpdatedAt = time.Now().UTC()
	r := c.r
	r.Snapshot.Transcript = nil
	return atomicWrite(c.s.receiptPath(r.Snapshot.ID), r)
}
func (c *call) eventLocked(kind, text g.String) {
	c.r.Snapshot.Events = append(c.r.Snapshot.Events, Event{At: time.Now().UTC(), Kind: kind, Text: text})
	if len(c.r.Snapshot.Events) > 200 {
		c.r.Snapshot.Events = c.r.Snapshot.Events[len(c.r.Snapshot.Events)-200:]
	}
}
func (c *call) streamURL() g.String {
	return c.s.cfg.PublicURL.TrimEndSet("/").Replace("https://", "wss://", 1) + "/media/" + c.r.Snapshot.ID
}

func (c *call) run() {
	if err := c.connectLive(); err != nil {
		c.finish("failed", g.String(err.Error()))
		return
	}
	c.mu.Lock()
	if c.stopRequested || terminal(c.r.Snapshot.Status) {
		c.mu.Unlock()
		c.finish("ended", "")
		return
	}
	c.r.DialSent = true
	c.eventLocked("dial", "Outbound call requested")
	if err := c.saveLocked(); err != nil {
		c.mu.Unlock()
		c.finish("failed", "cannot persist call before dialing")
		return
	}
	c.mu.Unlock()
	body := g.Map[g.String, any]{"connection_id": c.s.cfg.ConnectionID, "from": c.s.cfg.From, "to": c.request.To, "command_id": c.r.Snapshot.ID, "client_state": base64.StdEncoding.EncodeToString(c.r.Snapshot.ID.Bytes()), "webhook_url": c.s.cfg.PublicURL.TrimEndSet("/") + "/webhooks/telnyx", "webhook_url_method": "POST", "timeout_secs": 30, "retry_on_timeout": false, "time_limit_secs": c.s.cfg.MaxCallSeconds, "stream_url": c.streamURL(), "stream_track": "inbound_track", "stream_codec": "PCMU", "stream_bidirectional_mode": "rtp", "stream_bidirectional_codec": "PCMU", "stream_bidirectional_sampling_rate": 8000}
	result := c.s.telnyx(context.Background(), "POST", "/calls", body)
	if result.IsErr() {
		if unknown(result.Err()) {
			c.uncertainDial(g.String(result.Err().Error()))
		} else {
			c.finish("failed", g.String(result.Err().Error()))
		}
		return
	}
	var response struct {
		Data struct {
			ID g.String `json:"call_control_id"`
		} `json:"data"`
	}
	if json.Unmarshal(result.Unwrap(), &response) != nil || response.Data.ID.IsEmpty() {
		c.uncertainDial("Dial accepted without a call identifier; reconcile in Telnyx")
		return
	}
	c.mu.Lock()
	if !c.r.ControlID.IsEmpty() && c.r.ControlID != response.Data.ID {
		c.mu.Unlock()
		c.uncertainDial("Provider call identifier mismatch")
		return
	}
	c.r.ControlID = response.Data.ID
	if c.r.Snapshot.Status == "starting" {
		c.r.Snapshot.Status = "ringing"
	}
	err := c.saveLocked()
	stop := c.stopRequested
	done := terminal(c.r.Snapshot.Status)
	c.mu.Unlock()
	if done {
		return
	}
	if err != nil || stop {
		_ = c.stop("call cancelled during dialing")
		return
	}
	timer := time.NewTimer(time.Duration(c.s.cfg.MaxCallSeconds+45) * time.Second)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
	case <-timer.C:
		_ = c.stop("maximum call duration reached")
	}
}

func (c *call) uncertainDial(message g.String) {
	c.mu.Lock()
	c.stopRequested = true
	if !terminal(c.r.Snapshot.Status) {
		c.r.Snapshot.Status = "uncertain"
		c.r.Snapshot.LastError = message
		c.eventLocked("error", "Dial outcome unknown; never redial this request")
	}
	_ = c.saveLocked()
	hasID := !c.r.ControlID.IsEmpty()
	c.mu.Unlock()
	c.finalize()
	if hasID {
		_ = c.stop("dial response was uncertain")
	}
}

func (c *call) finish(status, message g.String) {
	c.mu.Lock()
	if terminal(c.r.Snapshot.Status) {
		c.mu.Unlock()
		return
	}
	c.r.Snapshot.Status = status
	c.stopRequested = true
	c.r.Snapshot.Question = nil
	if !message.IsEmpty() {
		c.r.Snapshot.LastError = message
		c.eventLocked("error", message)
	}
	c.eventLocked("call", "Call "+status)
	if c.r.Snapshot.Result == nil {
		c.r.Snapshot.Result = &Outcome{Outcome: "unconfirmed", Summary: "Call ended without a confirmed task result.", Confirmation: ""}
	}
	err := c.saveLocked()
	if err == nil {
		_ = os.Remove(c.s.activePath().Std())
	} else {
		c.r.Snapshot.LastError = "Unable to persist terminal state; active receipt retained"
	}
	c.mu.Unlock()
	c.finalize()
}

func (c *call) stop(reason g.String) error {
	c.mu.Lock()
	c.stopRequested = true
	if terminal(c.r.Snapshot.Status) {
		c.mu.Unlock()
		return nil
	}
	if !c.r.DialSent {
		c.mu.Unlock()
		c.finish("ended", "")
		return nil
	}
	if c.r.ControlID.IsEmpty() {
		c.r.Snapshot.Status = "uncertain"
		c.eventLocked("hangup", "Cancellation queued until a signed callback supplies the call identifier")
		err := c.saveLocked()
		c.mu.Unlock()
		c.finalize()
		return err
	}
	if c.r.HangupSent {
		c.mu.Unlock()
		return nil
	}
	c.r.HangupSent = true
	c.r.Snapshot.Status = "ending"
	c.eventLocked("hangup", reason)
	persistErr := c.saveLocked()
	if persistErr != nil {
		c.r.Snapshot.LastError = "Unable to persist hangup request; previous receipt retained"
	}
	id := c.r.ControlID
	command := fingerprint(c.r.Snapshot.ID + ":hangup")
	c.mu.Unlock()
	result := c.s.telnyx(context.Background(), "POST", actionPath(id, "hangup"), g.Map[g.String, any]{"command_id": command})
	if result.IsErr() {
		if alreadyEnded(result.Err()) {
			c.finish("ended", "")
			return persistErr
		}
		c.mu.Lock()
		if !terminal(c.r.Snapshot.Status) {
			c.r.Snapshot.Status = "uncertain"
			c.r.Snapshot.LastError = g.String(result.Err().Error())
			c.eventLocked("error", "Hangup outcome unconfirmed; use reconcile")
		}
		_ = c.saveLocked()
		c.mu.Unlock()
		c.finalize()
		return errors.Join(persistErr, result.Err())
	}
	c.mu.Lock()
	c.eventLocked("hangup", "Telnyx accepted hangup; awaiting ended callback or reconciliation")
	_ = c.saveLocked()
	c.mu.Unlock()
	c.finalize()
	return persistErr
}

func (c *call) reconcile() error {
	c.mu.Lock()
	id := c.r.ControlID
	done := terminal(c.r.Snapshot.Status)
	c.mu.Unlock()
	if done {
		return nil
	}
	if id.IsEmpty() {
		return errors.New("no provider call id; inspect Telnyx using this request id before any new call")
	}
	result := c.s.telnyx(context.Background(), "GET", "/calls/"+g.String(url.PathEscape(id.Std())), nil)
	if result.IsErr() {
		return result.Err()
	}
	var status struct {
		Data struct {
			Alive *bool `json:"is_alive"`
		} `json:"data"`
	}
	if json.Unmarshal(result.Unwrap(), &status) != nil || status.Data.Alive == nil {
		return errors.New("provider returned no call state")
	}
	if !*status.Data.Alive {
		c.finish("ended", "")
		return nil
	}
	c.mu.Lock()
	c.eventLocked("reconcile", "Telnyx reports the call is still alive; end it in Telnyx if hangup remains unconfirmed")
	err := c.saveLocked()
	c.mu.Unlock()
	return err
}

func (c *call) webhook(e webhookEvent) error {
	p := e.Data.Payload
	c.mu.Lock()
	if c.seen.Contains(e.Data.ID) || terminal(c.r.Snapshot.Status) {
		c.mu.Unlock()
		return nil
	}
	if p.ControlID.IsEmpty() || (!p.ConnectionID.IsEmpty() && p.ConnectionID != c.s.cfg.ConnectionID) || (!p.To.IsEmpty() && p.To != c.r.Snapshot.To) || (!p.From.IsEmpty() && p.From != c.s.cfg.From) || (!c.r.ControlID.IsEmpty() && p.ControlID != c.r.ControlID) {
		c.mu.Unlock()
		return errors.New("webhook call identity mismatch")
	}
	c.r.ControlID = p.ControlID
	shouldStop := c.stopRequested
	switch e.Data.Type {
	case "call.answered":
		c.answered = true
		if !c.stopRequested {
			c.r.Snapshot.Status = "active"
		}
	case "streaming.failed":
		c.r.Snapshot.LastError = "Telnyx media streaming failed"
		shouldStop = true
	case "streaming.stopped":
		shouldStop = true
	}
	c.eventLocked("telnyx", e.Data.Type)
	err := c.saveLocked()
	if err == nil {
		c.seen[e.Data.ID] = true
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if e.Data.Type == "call.hangup" {
		c.finish("ended", "")
	} else if shouldStop {
		go func() { _ = c.stop("call media stopped or cancellation requested") }()
	}
	return nil
}
