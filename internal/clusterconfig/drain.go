package clusterconfig

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const DrainRequestFile = "drain.request"
const DrainStatusFile = "drain.status"

// DrainControl is node-local. Boot and Operation must match the current process
// and request; an old PVC request/ack never controls a new process incarnation.
type DrainControl struct {
	Boot      string `json:"boot"`
	Operation string `json:"operation"`
	State     string `json:"state"`
	Pending   int    `json:"pending"`
	// Optional numeric barriers, not distinct playback counts. Missing is unknown.
	PendingRuntime    *int                    `json:"pendingRuntime,omitempty"`
	PendingSessions   *int                    `json:"pendingSessions,omitempty"`
	PendingCategories *DrainPendingCategories `json:"pendingCategories,omitempty"`
}

// DrainPendingCategories is a closed, identifier-free inventory of obligations.
// Each category may count a barrier for the same playback independently.
type DrainPendingCategories struct {
	SourcePreparations   int `json:"sourcePreparations"`
	PreparationLifetimes int `json:"preparationLifetimes"`
	PreparationReceipts  int `json:"preparationReceipts"`
	SourceLifetimes      int `json:"sourceLifetimes"`
	SourceCallbacks      int `json:"sourceCallbacks"`
	EncoderStarts        int `json:"encoderStarts"`
	EncoderTokens        int `json:"encoderTokens"`
	Inputs               int `json:"inputs"`
	RemotePreparing      int `json:"remotePreparing"`
	RemoteLive           int `json:"remoteLive"`
	RemoteRetiring       int `json:"remoteRetiring"`
	Executions           int `json:"executions"`
	Reopens              int `json:"reopens"`
	Placements           int `json:"placements"`
}

func ReadDrainControl(dir, name string) (DrainControl, error) {
	var d DrainControl
	if name != DrainRequestFile && name != DrainStatusFile {
		return d, errors.New("drain: invalid control name")
	}
	p := filepath.Join(dir, name)
	st, err := os.Lstat(p)
	if err != nil {
		return d, err
	}
	if !st.Mode().IsRegular() || !privateDrainFile(p, st) || st.Size() > 4096 {
		return d, errors.New("drain: unsafe control file")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return d, errors.New("drain: cannot read control file")
	}
	if json.Unmarshal(b, &d) != nil || d.Boot == "" || len(d.Boot) > 128 || len(d.Operation) > 128 {
		return DrainControl{}, errors.New("drain: invalid control document")
	}
	return d, nil
}
func WriteDrainControl(dir, name string, d DrainControl) error {
	if name != DrainRequestFile && name != DrainStatusFile {
		return errors.New("drain: invalid control name")
	}
	p := filepath.Join(dir, name)
	if st, err := os.Lstat(p); err == nil {
		if !st.Mode().IsRegular() || !privateDrainFile(p, st) {
			return errors.New("drain: unsafe control file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(b) > 4096 {
		return errors.New("drain: control document exceeds limit")
	}
	f, err := os.CreateTemp(dir, ".drain-*")
	if err != nil {
		return errors.New("drain: cannot write control file")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := protectDrainFile(tmp); err != nil {
		_ = f.Close()
		return errors.New("drain: cannot protect control file")
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("drain: cannot persist control file")
	}
	return os.Rename(tmp, p)
}

// RequestDrain blocks until exact-operation quiescence or ctx expiry. It never
// treats stale status, node disappearance or a deadline as retirement proof.
func RequestDrain(ctx context.Context, dir string) error {
	status, err := ReadDrainControl(dir, DrainStatusFile)
	if err != nil {
		return errors.New("drain: no safe active runtime status")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	req := DrainControl{Boot: status.Boot, Operation: hex.EncodeToString(random[:]), State: "requested"}
	if err = WriteDrainControl(dir, DrainRequestFile, req); err != nil {
		return err
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("drain: timed out; unresolved ownership remains retained")
		case <-tick.C:
			st, err := ReadDrainControl(dir, DrainStatusFile)
			if err != nil {
				continue
			}
			if st.Boot != req.Boot {
				return errors.New("drain: runtime incarnation changed; retirement is unproven")
			}
			if st.Operation == req.Operation && st.State == "drained" && st.Pending == 0 {
				return nil
			}
		}
	}
}
