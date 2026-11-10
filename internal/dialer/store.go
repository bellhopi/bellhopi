package dialer

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/bellhopi/bellhopi/internal/privatefs"
	"github.com/enetx/g"
)

type receipt struct {
	Snapshot    Snapshot                       `json:"snapshot"`
	RequestHash g.String                       `json:"request_hash"`
	ControlID   g.String                       `json:"call_control_id"`
	DialSent    bool                           `json:"dial_sent"`
	HangupSent  bool                           `json:"hangup_sent"`
	Commands    g.Map[g.String, commandRecord] `json:"commands"`
}
type commandRecord struct {
	Hash   g.String `json:"hash"`
	Result g.String `json:"result"`
}

func atomicWrite(path g.String, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path.Std()), ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = privatefs.Check(f); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return privatefs.Replace(g.String(f.Name()), path)
}

func (s *Server) receiptPath(id g.String) g.String {
	return g.String(filepath.Join(s.cfg.StateDir.Std(), id.Std()+".json"))
}
func (s *Server) activePath() g.String {
	return g.String(filepath.Join(s.cfg.StateDir.Std(), "active"))
}
func (s *Server) loadReceipt(id g.String) g.Result[receipt] {
	if !idPattern.MatchString(id.Std()) {
		return g.Err[receipt](errors.New("invalid call id"))
	}
	b, err := os.ReadFile(s.receiptPath(id).Std())
	if err != nil {
		return g.Err[receipt](err)
	}
	var r receipt
	if err = json.Unmarshal(b, &r); err != nil {
		return g.Err[receipt](errors.New("invalid call receipt; preserve it for recovery"))
	}
	if r.Snapshot.ID != id || r.RequestHash.IsEmpty() {
		return g.Err[receipt](errors.New("receipt identity mismatch"))
	}
	if r.Commands == nil {
		r.Commands = g.NewMap[g.String, commandRecord]()
	}
	return g.Ok(r)
}

func (s *Server) reserve(id g.String) error {
	f, err := privatefs.Create(s.activePath())
	if err != nil {
		return errors.New("an active call receipt exists; reconcile or hang up before another call")
	}
	_, err = f.Write(id.Bytes())
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
