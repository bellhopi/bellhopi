package dialer

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/enetx/g"
)

var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,80}$`)
var digitsPattern = regexp.MustCompile(`^[0-9A-D*#wW]{1,32}$`)

type Request struct {
	ID          g.String `json:"id"`
	To          g.String `json:"to"`
	Task        g.String `json:"task"`
	Information g.String `json:"information"`
}

func (r Request) Validate() g.Result[bool] {
	if !idPattern.MatchString(r.ID.Std()) {
		return g.Err[bool](errors.New("id must contain 8-80 letters, digits, underscores or hyphens; reuse it on retries"))
	}
	if !phonePattern.MatchString(r.To.Std()) {
		return g.Err[bool](errors.New("to must be a single E.164 phone number"))
	}
	if r.Task.Trim().Len() < 5 || r.Task.Len() > 8000 || r.Information.Len() > 16000 {
		return g.Err[bool](errors.New("task must be 5-8000 bytes; information at most 16000 bytes"))
	}
	return g.Ok(true)
}

type Question struct {
	ID   g.String `json:"id"`
	Text g.String `json:"text"`
}
type Outcome struct {
	Outcome      g.String `json:"outcome"`
	Summary      g.String `json:"summary"`
	Confirmation g.String `json:"confirmation"`
}
type Event struct {
	At   time.Time `json:"at"`
	Kind g.String  `json:"kind"`
	Text g.String  `json:"text"`
}
type Transcript struct {
	Speaker g.String `json:"speaker"`
	Text    g.String `json:"text"`
	StartMS g.Int    `json:"start_ms"`
	EndMS   g.Int    `json:"end_ms"`
}
type Snapshot struct {
	ID             g.String            `json:"id"`
	To             g.String            `json:"to"`
	Status         g.String            `json:"status"`
	StartedAt      time.Time           `json:"started_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
	Question       *Question           `json:"question"`
	Result         *Outcome            `json:"result"`
	Events         g.Slice[Event]      `json:"events"`
	Transcript     g.Slice[Transcript] `json:"transcript"`
	LastError      g.String            `json:"last_error"`
	MediaConnected bool                `json:"media_connected"`
	LiveConnected  bool                `json:"live_connected"`
	VoiceUsage     json.RawMessage     `json:"voice_usage,omitempty"`
}

func terminal(status g.String) bool { return status == "ended" || status == "failed" }

func decode(r io.Reader, target any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return errors.New("invalid JSON or unsupported field")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}
