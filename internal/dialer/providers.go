package dialer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/enetx/g"
)

type providerError struct {
	Status  int
	Code    g.String
	Unknown bool
}

func (e *providerError) Error() string {
	return fmt.Sprintf("Telnyx HTTP %d code %s (outcome_unknown=%t)", e.Status, e.Code, e.Unknown)
}

func fingerprint(v any) g.String {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return g.String(hex.EncodeToString(h[:]))
}

func verifySignature(key g.String, headers http.Header, payload g.Bytes, now time.Time) bool {
	ts := headers.Get("telnyx-timestamp")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || n < now.Unix()-300 || n > now.Unix()+300 {
		return false
	}
	pub, err := base64.StdEncoding.DecodeString(key.Std())
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(headers.Get("telnyx-signature-ed25519"))
	if err != nil {
		return false
	}
	message := append(g.Bytes(ts+"|"), payload...)
	return ed25519.Verify(pub, message, sig)
}

func (s *Server) telnyx(ctx context.Context, method, path g.String, body any) g.Result[json.RawMessage] {
	var b g.Bytes
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return g.Err[json.RawMessage](err)
		}
		b = raw
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method.Std(), (s.telnyxURL + path).Std(), bytes.NewReader(b))
	if err != nil {
		return g.Err[json.RawMessage](err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.TelnyxKey.Std())
	req.Header.Set("Content-Type", "application/json")
	res, err := s.http.Do(req)
	if err != nil {
		return g.Err[json.RawMessage](&providerError{Unknown: true, Code: "transport_error"})
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1024*1024))
	if err != nil {
		return g.Err[json.RawMessage](&providerError{Status: res.StatusCode, Unknown: true, Code: "read_error"})
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e struct {
			Errors g.Slice[struct {
				Code g.String `json:"code"`
			}] `json:"errors"`
		}
		_ = json.Unmarshal(raw, &e)
		code := g.String("unknown")
		if len(e.Errors) > 0 {
			code = e.Errors[0].Code.Truncate(80)
		}
		return g.Err[json.RawMessage](&providerError{Status: res.StatusCode, Code: code, Unknown: res.StatusCode >= 500 || res.StatusCode == 408})
	}
	return g.Ok(json.RawMessage(raw))
}

func actionPath(control, action g.String) g.String {
	return "/calls/" + g.String(url.PathEscape(control.Std())) + "/actions/" + action
}

func unknown(err error) bool { var p *providerError; return !errors.As(err, &p) || p.Unknown }
func alreadyEnded(err error) bool {
	var p *providerError
	return errors.As(err, &p) && p.Code == "90018"
}
