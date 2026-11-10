package dialer

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/enetx/g"
)

func toolDefinitions() g.Slice[g.Map[g.String, any]] {
	text := g.Map[g.String, any]{"type": "string"}
	tool := func(name, description g.String, properties g.Map[g.String, any], required g.Slice[g.String]) g.Map[g.String, any] {
		return g.Map[g.String, any]{"type": "function", "name": name, "description": description, "strict": true, "parameters": g.Map[g.String, any]{"type": "object", "properties": properties, "required": required, "additionalProperties": false}}
	}
	return g.Slice[g.Map[g.String, any]]{
		tool("send_dtmf", "Send real keypad tones for the menu just heard, then listen for the next prompt.", g.Map[g.String, any]{"digits": text, "duration_millis": g.Map[g.String, any]{"type": "integer", "minimum": 100, "maximum": 500}, "reason": text}, g.Slice[g.String]{"digits", "duration_millis", "reason"}),
		tool("wait", "Wait quietly while an IVR prompt or hold music plays.", g.Map[g.String, any]{"seconds": g.Map[g.String, any]{"type": "integer", "minimum": 1, "maximum": 10}}, g.Slice[g.String]{"seconds"}),
		tool("ask_user", "Ask the owner for missing information or a decision; wait up to 90 seconds for their answer.", g.Map[g.String, any]{"question": text}, g.Slice[g.String]{"question"}),
		tool("finish_task", "Record the evidenced outcome and end the phone call. Say goodbye before using this.", g.Map[g.String, any]{"outcome": g.Map[g.String, any]{"type": "string", "enum": g.Slice[g.String]{"completed", "partial", "blocked"}}, "summary": text, "confirmation": text}, g.Slice[g.String]{"outcome", "summary", "confirmation"}),
	}
}

func (c *call) runBatch(calls g.Slice[functionCall]) {
	for f := range calls.Iter() {
		if c.ctx.Err() != nil {
			return
		}
		result := c.executeTool(f)
		value := result.UnwrapOr(nil)
		if result.IsErr() {
			value = g.Map[g.String, any]{"error": result.Err().Error()}
		}
		b, _ := json.Marshal(value)
		if err := c.sendLive(g.Map[g.String, any]{"type": "response.item.create", "item": g.Map[g.String, any]{"type": "function_call_output", "call_id": f.CallID, "output": g.String(b)}}); err != nil {
			return
		}
	}
	if err := c.sendLive(g.Map[g.String, any]{"type": "response.create"}); err != nil {
		c.liveFailure("could not continue delegated work")
	}
}

func (c *call) executeTool(f functionCall) g.Result[g.Map[g.String, any]] {
	c.mu.Lock()
	stopped := c.stopRequested
	c.mu.Unlock()
	if stopped {
		return g.Err[g.Map[g.String, any]](errors.New("call is ending"))
	}
	if f.Arguments.Len() > 12000 {
		return g.Err[g.Map[g.String, any]](errors.New("tool arguments too large"))
	}
	switch f.Name {
	case "send_dtmf":
		var a struct {
			Digits   g.String `json:"digits"`
			Duration g.Int    `json:"duration_millis"`
			Reason   g.String `json:"reason"`
		}
		if err := decode(f.Arguments.Reader(), &a); err != nil {
			return g.Err[g.Map[g.String, any]](err)
		}
		return c.dtmf("tool_"+f.CallID, a.Digits, a.Duration)
	case "wait":
		var a struct {
			Seconds g.Int `json:"seconds"`
		}
		if err := decode(f.Arguments.Reader(), &a); err != nil {
			return g.Err[g.Map[g.String, any]](err)
		}
		if a.Seconds < 1 || a.Seconds > 10 {
			return g.Err[g.Map[g.String, any]](errors.New("wait must be 1-10 seconds"))
		}
		timer := time.NewTimer(time.Duration(a.Seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
			return g.Ok(g.Map[g.String, any]{"status": "waited"})
		case <-c.ctx.Done():
			return g.Err[g.Map[g.String, any]](errors.New("call ended"))
		}
	case "ask_user":
		var a struct {
			Question g.String `json:"question"`
		}
		if err := decode(f.Arguments.Reader(), &a); err != nil {
			return g.Err[g.Map[g.String, any]](err)
		}
		return c.askUser(f.CallID, a.Question)
	case "finish_task":
		var outcome Outcome
		if err := decode(f.Arguments.Reader(), &outcome); err != nil {
			return g.Err[g.Map[g.String, any]](err)
		}
		if !(g.Slice[g.String]{"completed", "partial", "blocked"}).Contains(outcome.Outcome) || outcome.Summary.Trim().IsEmpty() || outcome.Summary.Len() > 4000 || outcome.Confirmation.Len() > 1000 {
			return g.Err[g.Map[g.String, any]](errors.New("invalid task outcome"))
		}
		c.mu.Lock()
		if c.stopRequested || terminal(c.r.Snapshot.Status) {
			c.mu.Unlock()
			return g.Err[g.Map[g.String, any]](errors.New("call is ending"))
		}
		c.r.Snapshot.Result = &outcome
		c.eventLocked("result", "Task outcome reported by the assistant: "+outcome.Outcome)
		err := c.saveLocked()
		c.mu.Unlock()
		if err != nil {
			return g.Err[g.Map[g.String, any]](err)
		}
		go func() {
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
				_ = c.stop("task outcome recorded")
			case <-c.ctx.Done():
			}
		}()
		return g.Ok(g.Map[g.String, any]{"status": "recorded", "outcome": outcome.Outcome})
	default:
		return g.Err[g.Map[g.String, any]](errors.New("unknown tool"))
	}
}

func (c *call) dtmf(command, digits g.String, duration g.Int) g.Result[g.Map[g.String, any]] {
	if duration == 0 {
		duration = 250
	}
	if !digitsPattern.MatchString(digits.Std()) || !digits.ContainsAnyChars("0123456789ABCD*#") || duration < 100 || duration > 500 {
		return g.Err[g.Map[g.String, any]](errors.New("DTMF requires 1-32 digits 0-9, A-D, *, # or w/W pauses; tone duration 100-500 ms"))
	}
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	hash := fingerprint(g.Map[g.String, any]{"digits": digits, "duration": duration})
	c.mu.Lock()
	if previous := c.r.Commands.Get(command); previous.IsSome() {
		p := previous.Unwrap()
		c.mu.Unlock()
		if p.Hash != hash {
			return g.Err[g.Map[g.String, any]](errors.New("command id already used for different DTMF"))
		}
		return g.Ok(g.Map[g.String, any]{"status": p.Result, "duplicate": true})
	}
	if c.stopRequested || !c.answered || c.r.ControlID.IsEmpty() {
		c.mu.Unlock()
		return g.Err[g.Map[g.String, any]](errors.New("call is not answered or is ending"))
	}
	if len(c.r.Commands) >= 128 {
		c.mu.Unlock()
		return g.Err[g.Map[g.String, any]](errors.New("call command limit reached"))
	}
	c.r.Commands[command] = commandRecord{Hash: hash, Result: "outcome_unknown"}
	if err := c.saveLocked(); err != nil {
		c.mu.Unlock()
		return g.Err[g.Map[g.String, any]](err)
	}
	id := c.r.ControlID
	c.mu.Unlock()
	response := c.s.telnyx(context.Background(), "POST", actionPath(id, "send_dtmf"), g.Map[g.String, any]{"digits": digits, "duration_millis": duration, "command_id": fingerprint(command)})
	status := g.String("accepted")
	if response.IsErr() {
		status = "rejected"
		if unknown(response.Err()) {
			status = "outcome_unknown"
		}
	}
	c.mu.Lock()
	uncertain := response.IsErr() && unknown(response.Err())
	if uncertain {
		c.stopRequested = true
		c.r.Snapshot.LastError = g.String(response.Err().Error())
	}
	c.r.Commands[command] = commandRecord{Hash: hash, Result: status}
	c.eventLocked("dtmf", "DTMF command "+status+" ("+digits.Len().String()+" characters)")
	err := c.saveLocked()
	c.mu.Unlock()
	if uncertain {
		go func() { _ = c.stop("DTMF outcome unknown; stopped rather than repeating selection") }()
	}
	if err != nil {
		return g.Err[g.Map[g.String, any]](err)
	}
	if response.IsErr() {
		return g.Err[g.Map[g.String, any]](response.Err())
	}
	return g.Ok(g.Map[g.String, any]{"status": status, "ivr_recognition": "unconfirmed_until_next_prompt"})
}

func (c *call) askUser(id, question g.String) g.Result[g.Map[g.String, any]] {
	if question.Trim().IsEmpty() || question.Len() > 2000 {
		return g.Err[g.Map[g.String, any]](errors.New("question must contain 1-2000 bytes"))
	}
	c.mu.Lock()
	if c.stopRequested || terminal(c.r.Snapshot.Status) {
		c.mu.Unlock()
		return g.Err[g.Map[g.String, any]](errors.New("call is ending"))
	}
	if c.r.Snapshot.Question != nil {
		c.mu.Unlock()
		return g.Err[g.Map[g.String, any]](errors.New("another question is pending"))
	}
	ch := make(chan g.String, 1)
	c.questionReply = ch
	c.r.Snapshot.Question = &Question{ID: id, Text: question}
	c.r.Snapshot.Status = "needs_input"
	c.eventLocked("question", "Assistant is waiting for the owner")
	err := c.saveLocked()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.questionReply == ch {
			c.questionReply = nil
			c.r.Snapshot.Question = nil
			if c.r.Snapshot.Status == "needs_input" {
				c.r.Snapshot.Status = "active"
			}
			_ = c.saveLocked()
		}
		c.mu.Unlock()
	}()
	if err != nil {
		return g.Err[g.Map[g.String, any]](err)
	}
	timer := time.NewTimer(90 * time.Second)
	defer timer.Stop()
	select {
	case answer := <-ch:
		return g.Ok(g.Map[g.String, any]{"answer": answer})
	case <-timer.C:
		return g.Ok(g.Map[g.String, any]{"status": "user_unavailable", "instruction": "Do not assume permission or facts; report partial/blocked if you cannot proceed."})
	case <-c.ctx.Done():
		return g.Err[g.Map[g.String, any]](errors.New("call ended"))
	}
}

func (c *call) reply(id, answer g.String) g.Result[bool] {
	if answer.Trim().IsEmpty() || answer.Len() > 8000 {
		return g.Err[bool](errors.New("answer must contain 1-8000 bytes"))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopRequested || c.r.Snapshot.Question == nil || c.r.Snapshot.Question.ID != id || c.questionReply == nil {
		return g.Err[bool](errors.New("question is no longer pending"))
	}
	select {
	case c.questionReply <- answer:
		c.eventLocked("reply", "Owner supplied an answer")
		return g.ResultOf(true, c.saveLocked())
	default:
		return g.Err[bool](errors.New("question already answered"))
	}
}
