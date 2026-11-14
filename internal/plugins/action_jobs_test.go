package plugins

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/config"
	"strings"
	"testing"
	"time"
)

func TestHostedJobInvariants(t *testing.T) {
	t.Run("resource grants", func(t *testing.T) {
		m := Manifest{Ether: &EtherBlock{Runtime: RuntimeDecl{Kind: "hosted"}}}
		defaults := RuntimeLimitsFor(m)
		if defaults.MemoryBytes != 32<<20 || defaults.Deadline != 12*time.Hour || defaults.UnitBudget != 1<<60 {
			t.Fatalf("defaults: %+v", defaults)
		}
		m.Ether.Runtime.Limits = &RuntimeLimits{MemoryBytes: 1 << 40, Deadline: "100h", UnitBudget: 1 << 50}
		limits := HotLimits(m)
		if limits.MemoryBytes != 2<<30 || limits.Deadline != 12*time.Hour || limits.UnitBudget != 1<<50 {
			t.Fatalf("ceilings: %+v", limits)
		}
		m.Ether.Runtime.Limits = &RuntimeLimits{MemoryBytes: 16 << 20, Deadline: "120s", UnitBudget: 12000}
		limits = HotLimits(m)
		granted := GrantedRuntimeLimits(m)
		if limits.MemoryBytes != 16<<20 || limits.Deadline != 120*time.Second || limits.UnitBudget != 12000 || granted.Deadline != "2m0s" {
			t.Fatalf("grant: %+v / %+v", limits, granted)
		}
		if budgets := EffectiveBudgets(limits); budgets.PerCandidate != 2*time.Millisecond || budgets.PerBatch != 50*time.Millisecond {
			t.Fatal(budgets)
		}
	})
	t.Run("owner ceilings and per plugin override", func(t *testing.T) {
		cfg := config.Plugins{RuntimeLimits: config.PluginRuntimeLimits{MemoryBytes: 512 << 20, Deadline: "24h", UnitBudget: 1000000}, RuntimeOverrides: map[string]config.PluginRuntimeLimits{"test.plugin": {MemoryBytes: 4 << 30, Deadline: "48h", UnitBudget: 1 << 60}}}
		m := &Manager{deps: Deps{ReadRuntimeConfig: func() config.Plugins { return cfg }}}
		if c := m.runtimeCeilings("other"); c.memory != 512<<20 || c.deadline != 24*time.Hour || c.units != 1000000 {
			t.Fatal(c)
		}
		if c := m.runtimeCeilings("test.plugin"); c.memory != 4<<30 || c.deadline != 48*time.Hour || c.units != 1<<60 {
			t.Fatal(c)
		}
	})

	t.Run("data grants and archive ceilings", func(t *testing.T) {
		cfg := config.Plugins{RuntimeLimits: config.PluginRuntimeLimits{FetchBytes: 64 << 30, ArchiveMemberBytes: 16 << 30, ArchiveExpandedBytes: 128 << 30, ArchiveMembers: 100000}}
		mgr := &Manager{deps: Deps{ReadRuntimeConfig: func() config.Plugins { return cfg }}}
		man := Manifest{ID: "test.plugin", Ether: &EtherBlock{Runtime: RuntimeDecl{Kind: "hosted", Limits: &RuntimeLimits{FetchBytes: 128 << 30, ArchiveMemberBytes: 32 << 30, ArchiveExpandedBytes: 256 << 30, ArchiveMembers: 200000}}}}
		grant := mgr.grantedRuntimeLimits(man)
		if grant.FetchBytes != 64<<30 || grant.ArchiveMemberBytes != 16<<30 || grant.ArchiveExpandedBytes != 128<<30 || grant.ArchiveMembers != 100000 {
			t.Fatalf("data clamp: %+v", grant)
		}
		data := dataZIP(t, "safe", zip.Store, []byte("1234"))
		grant.ArchiveMemberBytes = 3
		if _, err := openDataZIP(context.Background(), bytes.NewReader(data), *grant); err == nil {
			t.Fatal("member ceiling ignored")
		}
		grant.ArchiveMemberBytes = 10
		grant.ArchiveExpandedBytes = 3
		if _, err := openDataZIP(context.Background(), bytes.NewReader(data), *grant); err == nil {
			t.Fatal("total ceiling ignored")
		}
	})
	t.Run("ZIP64 bounded directory", func(t *testing.T) {
		data := dataZIP(t, "safe", zip.Store, []byte("1234"))
		end := len(data) - 22
		e := append([]byte{}, data[end:]...)
		rec := make([]byte, 56)
		binary.LittleEndian.PutUint32(rec, 0x06064b50)
		binary.LittleEndian.PutUint64(rec[4:], 44)
		binary.LittleEndian.PutUint64(rec[24:], 1)
		binary.LittleEndian.PutUint64(rec[32:], 1)
		binary.LittleEndian.PutUint64(rec[40:], uint64(binary.LittleEndian.Uint32(e[12:])))
		binary.LittleEndian.PutUint64(rec[48:], uint64(binary.LittleEndian.Uint32(e[16:])))
		locator := make([]byte, 20)
		binary.LittleEndian.PutUint32(locator, 0x07064b50)
		binary.LittleEndian.PutUint64(locator[8:], uint64(end))
		binary.LittleEndian.PutUint32(locator[16:], 1)
		binary.LittleEndian.PutUint16(e[8:], 65535)
		binary.LittleEndian.PutUint16(e[10:], 65535)
		binary.LittleEndian.PutUint32(e[16:], 0xffffffff)
		combined := append(append(append(append([]byte{}, data[:end]...), rec...), locator...), e...)
		if z, err := openDataZIP(context.Background(), bytes.NewReader(combined)); err != nil || len(z.File) != 1 {
			t.Fatalf("ZIP64: %v", err)
		}
		binary.LittleEndian.PutUint64(combined[end+32:], 1000001)
		if _, err := openDataZIP(context.Background(), bytes.NewReader(combined)); err == nil {
			t.Fatal("ZIP64 count ceiling ignored")
		}
	})

	t.Run("checkpoint progress completion", func(t *testing.T) {
		j := &actionJob{}
		calls := 0
		err := runActionJob(context.Background(), j, 3, func(ctx context.Context, token json.RawMessage) (Response, error) {
			calls++
			if calls == 1 {
				if token != nil {
					t.Fatal(string(token))
				}
				return Response{Status: 200, Body: json.RawMessage(`{"continueInMs":0,"continuation":{"next":41},"progress":"41 of 92"}`)}, nil
			}
			if string(token) != `{"next":41}` || j.snapshot().Progress != "41 of 92" {
				t.Fatalf("checkpoint: %s %+v", token, j.snapshot())
			}
			return Response{Status: 200, Body: json.RawMessage(`{"done":true,"continueInMs":0,"progress":"Ready"}`)}, nil
		})
		if err != nil || calls != 2 || j.snapshot().Progress != "Ready" {
			t.Fatalf("%v / %d / %+v", err, calls, j.snapshot())
		}
	})
	t.Run("bounded attempts", func(t *testing.T) {
		j := &actionJob{}
		err := runActionJob(context.Background(), j, 2, func(context.Context, json.RawMessage) (Response, error) {
			return Response{Status: 200, Body: json.RawMessage(`{"continueInMs":0}`)}, nil
		})
		if err == nil || !strings.Contains(err.Error(), "limit of 2 calls") || j.snapshot().Attempts != 2 {
			t.Fatalf("%v %+v", err, j.snapshot())
		}
	})
	t.Run("cancellable between calls", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		j := &actionJob{}
		err := runActionJob(ctx, j, 3, func(context.Context, json.RawMessage) (Response, error) {
			cancel()
			return Response{Status: 200, Body: json.RawMessage(`{"continueInMs":300000}`)}, nil
		})
		if !errors.Is(err, context.Canceled) || j.snapshot().Attempts != 1 {
			t.Fatalf("%v %+v", err, j.snapshot())
		}
	})
	t.Run("wall clock cap", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		j := &actionJob{}
		err := runActionJob(ctx, j, 3, func(context.Context, json.RawMessage) (Response, error) {
			return Response{Status: 200, Body: json.RawMessage(`{"continueInMs":300000}`)}, nil
		})
		if !errors.Is(err, context.DeadlineExceeded) || j.snapshot().Attempts != 1 {
			t.Fatalf("%v %+v", err, j.snapshot())
		}
	})
	for name, body := range map[string]string{"failed": `{"failed":"bad input","continueInMs":0}`, "invalid": `{"continueInMs":-1}`, "huge delay": `{"continueInMs":9223372036854775807}`, "invalid token": `{"continueInMs":0,"continuation":"` + strings.Repeat("x", 65536) + `"}`} {
		t.Run(name, func(t *testing.T) {
			j := &actionJob{}
			err := runActionJob(context.Background(), j, 3, func(context.Context, json.RawMessage) (Response, error) {
				return Response{Status: 200, Body: json.RawMessage(body)}, nil
			})
			if err == nil || j.snapshot().Attempts != 1 {
				t.Fatalf("%v %+v", err, j.snapshot())
			}
		})
	}
}
