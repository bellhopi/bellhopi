package dialer

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"

	"github.com/bellhopi/bellhopi/internal/privatefs"
	"github.com/enetx/g"
)

type Config struct {
	OpenAIKey       g.String `json:"openai_api_key"`
	TelnyxKey       g.String `json:"telnyx_api_key"`
	TelnyxPublicKey g.String `json:"telnyx_public_key"`
	ConnectionID    g.String `json:"telnyx_connection_id"`
	From            g.String `json:"telnyx_from_number"`
	PublicURL       g.String `json:"public_base_url"`
	APIToken        g.String `json:"api_token"`
	Listen          g.String `json:"listen"`
	StateDir        g.String `json:"state_dir"`
	LiveModel       g.String `json:"live_model"`
	BackendModel    g.String `json:"backend_model"`
	Voice           g.String `json:"voice"`
	MaxCallSeconds  g.Int    `json:"max_call_seconds"`
}

func DefaultConfigPath() g.Result[g.String] {
	base := g.String(os.Getenv("XDG_CONFIG_HOME"))
	if base.IsEmpty() {
		home, err := os.UserHomeDir()
		if err != nil {
			return g.Err[g.String](err)
		}
		base = g.String(filepath.Join(home, ".config"))
	}
	return g.Ok(g.String(filepath.Join(base.Std(), "concierge-dialer", "config.json")))
}

func defaults(path g.String) Config {
	return Config{Listen: "127.0.0.1:8787", StateDir: g.String(filepath.Join(filepath.Dir(path.Std()), "state")), LiveModel: "gpt-live-1", BackendModel: "gpt-6-luna", Voice: "marin", MaxCallSeconds: 600}
}

func InitConfig(path g.String) g.Result[Config] {
	cfg := defaults(path)
	secret := make(g.Bytes, 32)
	if _, err := rand.Read(secret); err != nil {
		return g.Err[Config](err)
	}
	cfg.APIToken = g.String(base64.RawURLEncoding.EncodeToString(secret))
	if err := privatefs.MkdirAll(g.String(filepath.Dir(path.Std()))); err != nil {
		return g.Err[Config](err)
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return g.Err[Config](err)
	}
	f, err := privatefs.Create(path)
	if err != nil {
		return g.Err[Config](err)
	}
	_, err = f.Write(append(b, '\n'))
	closeErr := f.Close()
	if err != nil {
		return g.Err[Config](err)
	}
	if closeErr != nil {
		return g.Err[Config](closeErr)
	}
	return g.Ok(cfg)
}

func LoadConfig(path g.String) g.Result[Config] {
	cfg := defaults(path)
	f, err := os.Open(path.Std())
	if err == nil {
		defer f.Close()
		st, statErr := f.Stat()
		if statErr != nil {
			return g.Err[Config](statErr)
		}
		if err := privatefs.Check(f); err != nil {
			return g.Err[Config](err)
		}
		if st.Size() > 65536 {
			return g.Err[Config](errors.New("configuration exceeds 64 KiB"))
		}
		if err := decode(f, &cfg); err != nil {
			return g.Err[Config](err)
		}
	} else if !os.IsNotExist(err) {
		return g.Err[Config](err)
	}
	g.Map[g.String, *g.String]{
		"OPENAI_API_KEY": &cfg.OpenAIKey, "TELNYX_API_KEY": &cfg.TelnyxKey,
		"TELNYX_PUBLIC_KEY": &cfg.TelnyxPublicKey, "TELNYX_CONNECTION_ID": &cfg.ConnectionID,
		"TELNYX_FROM_NUMBER": &cfg.From, "PUBLIC_BASE_URL": &cfg.PublicURL,
		"CONCIERGE_API_TOKEN": &cfg.APIToken,
	}.Iter().ForEach(func(key g.String, dest *g.String) {
		if val := os.Getenv(key.Std()); val != "" {
			*dest = g.String(val)
		}
	})
	return g.Ok(cfg)
}

func (c Config) Missing() g.Slice[g.String] {
	missing := g.Slice[g.String]{}
	g.Map[g.String, g.String]{"openai_api_key": c.OpenAIKey, "telnyx_api_key": c.TelnyxKey, "telnyx_public_key": c.TelnyxPublicKey, "telnyx_connection_id": c.ConnectionID, "telnyx_from_number": c.From, "public_base_url": c.PublicURL}.Iter().ForEach(func(k, v g.String) {
		if v.Trim().IsEmpty() {
			missing = append(missing, k)
		}
	})
	if c.APIToken.Len() < 32 {
		missing = append(missing, "api_token (at least 32 characters)")
	}
	if !c.From.IsEmpty() && !phonePattern.MatchString(c.From.Std()) {
		missing = append(missing, "telnyx_from_number (E.164 required)")
	}
	if !c.TelnyxPublicKey.IsEmpty() {
		b, e := base64.StdEncoding.DecodeString(c.TelnyxPublicKey.Std())
		if e != nil || len(b) != 32 {
			missing = append(missing, "telnyx_public_key (base64 Ed25519 required)")
		}
	}
	u, err := url.Parse(c.PublicURL.Std())
	if !c.PublicURL.IsEmpty() && (err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "") {
		missing = append(missing, "public_base_url (HTTPS origin required)")
	}
	if _, _, err := net.SplitHostPort(c.Listen.Std()); err != nil {
		missing = append(missing, "listen (host:port required)")
	}
	if c.StateDir.IsEmpty() {
		missing = append(missing, "state_dir")
	}
	if c.MaxCallSeconds < 30 || c.MaxCallSeconds > 14400 {
		missing = append(missing, "max_call_seconds (30-14400)")
	}
	if c.LiveModel.IsEmpty() || c.BackendModel.IsEmpty() || c.Voice.IsEmpty() {
		missing = append(missing, "live_model, backend_model and voice must be set")
	}
	return missing
}

func (c Config) validate() error {
	if m := c.Missing(); len(m) > 0 {
		return fmt.Errorf("configure: %v", m)
	}
	return nil
}
