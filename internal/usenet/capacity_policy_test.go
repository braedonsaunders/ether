package usenet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/usenet/nntptest"
)

// Provider reply vocabulary is independent of the status code: the same
// capacity failure is delivered at greeting, AUTHINFO USER, or AUTHINFO PASS.
func TestProviderCapacityPolicy(t *testing.T) {
	t.Run("reply vocabulary", func(t *testing.T) {
		for _, tc := range []struct {
			provider, msg string
			limit         bool
		}{
			{"Giganews", "(remote) gn;gn1296806: max number of simultaneous connections", true},
			{"Newshosting", "Too many connections", true},
			{"Eweka", "Maximum number of connections exceeded", true},
			{"UsenetServer", "Connection limit reached", true},
			{"Frugal", "Too many simultaneous connections", true},
			{"session", "Account ALREADY LOGGED IN", true},
			{"session", "Account in use", true},
			{"capacity", "Maximum sessions exceeded", true},
			{"capacity", "Simultaneous sessions exceeded", true},
			{"password", "Authentication failed", false},
			{"password", "Invalid username or password", false},
			{"sequence", "Authentication commands issued out of sequence", false},
		} {
			for _, code := range []int{481, 482, 502} {
				err := &ProtocolError{Cmd: "AUTHINFO PASS", Code: code, Msg: tc.msg}
				if errors.Is(err, ErrConnectionLimit) != tc.limit {
					t.Errorf("%s %v: wrong capacity classification", tc.provider, err)
				}
				wantAuth := !tc.limit && code != 502
				if errors.Is(err, ErrAuthFailed) != wantAuth {
					t.Errorf("%s %v: wrong credential classification", tc.provider, err)
				}
			}
		}
	})
	t.Run("shrink and recovery", func(t *testing.T) {
		s := nntptest.Start(t, nntptest.Options{Username: "user", Password: "pass"})
		cfg := s.Provider("p", 4)
		f := newFetcher(t, []config.UsenetProvider{cfg}, Options{MinIdle: -1, limits: limitTiming{widen: time.Hour}})
		p := poolFor(t, f, "p")
		ctx := context.Background()
		c, err := p.get(ctx, core.PriorityLive)
		if err != nil {
			t.Fatal(err)
		}
		s.SetFault(func(cmd, arg string) nntptest.Fault {
			if cmd == "AUTHINFO" && arg == "PASS pass" {
				return nntptest.Fault{Code: 481, Text: "(remote) gn;gn1296806: max number of simultaneous connections"}
			}
			return nntptest.Fault{}
		})
		if err := p.sem.Acquire(ctx, core.PriorityLive); err != nil {
			t.Fatal(err)
		}
		_, err = p.dialNew(ctx, false)
		p.sem.Release(core.PriorityLive)
		if !errors.Is(err, errAtLimit) || errors.Is(err, ErrAuthFailed) {
			t.Fatalf("capacity dial: %v", err)
		}
		if st := p.stats(); st.State != StateOK || st.MaxConnections != 1 || !st.LimitLearned {
			t.Fatalf("pool did not keep its working connection: %+v", st)
		}
		p.put(c, core.PriorityLive)
		s.SetFault(nil)
		p.mu.Lock()
		p.widenLocked(time.Now().Add(2 * time.Hour))
		p.mu.Unlock()
		p.applySize()
		c, err = p.get(ctx, core.PriorityLive)
		if err != nil {
			t.Fatal(err)
		}
		second, err := p.get(ctx, core.PriorityLive)
		if err != nil {
			t.Fatal(err)
		}
		p.put(second, core.PriorityLive)
		p.put(c, core.PriorityLive)
		if st := p.stats(); st.State != StateOK || st.MaxConnections != 2 || st.LiveConnections != 2 {
			t.Fatalf("capacity did not recover: %+v", st)
		}
	})
	t.Run("manual test unlocks saved credentials only", func(t *testing.T) {
		s := nntptest.Start(t, nntptest.Options{Username: "user", Password: "pass"})
		cfg := s.Provider("p", 2)
		f := newFetcher(t, []config.UsenetProvider{cfg}, Options{MinIdle: -1})
		p := poolFor(t, f, "p")
		p.brk.authFailure(&ProtocolError{Code: 481, Msg: "authentication failed"})
		// Valid draft credentials for another account must not unlock this one.
		s.SetCredentials("draft", "pass")
		draft := cfg
		draft.Username = "draft"
		if err := f.TestProvider(context.Background(), draft); err != nil {
			t.Fatal(err)
		}
		if p.brk.authDisabled() == nil {
			t.Fatal("testing draft credentials unlocked the saved account")
		}
		s.SetCredentials("user", "pass")
		if err := f.TestProvider(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if p.brk.authDisabled() != nil {
			t.Fatal("manual login did not immediately unlock saved credentials")
		}
	})
	// Preserve the existing adaptive-capacity invariants with jitter enabled.
	t.Run("unknown limit recovery", TestUnknownLimitWidensOnePerInterval)
	t.Run("repeated capacity refusals back off", TestRefusedWidenBacksOffExponentially)
	t.Run("credential retry cadence", TestBreakerAuthBackoffDoublesToCap)
}
