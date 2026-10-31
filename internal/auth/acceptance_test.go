package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestAcceptReceiptFollowsPersonalTokenLifetime(t *testing.T) {
	s, v, c := setup(t)
	ctx := context.Background()
	u, acc, err := s.Accept(ctx, "home-user")
	if err != nil || u == nil || !acc.Valid() || fmt.Sprint(acc) != "auth.Acceptance" {
		t.Fatalf("accept home-user: %v %v %v", u, acc, err)
	}
	if err := s.ReceiptCurrent(ctx, acc); err != nil {
		t.Fatal(err)
	}
	c.add(s.o.CacheTTL - time.Second)
	if _, _, err := s.Accept(ctx, "home-user"); err != nil {
		t.Fatalf("inside CacheTTL: %v", err)
	}
	if err := s.ReceiptCurrent(ctx, acc); err != nil {
		t.Fatalf("receipt inside CacheTTL: %v", err)
	}
	c.add(2 * time.Second)
	v.setDown(true)
	u2, stale, err := s.Accept(ctx, "home-user")
	if err != nil || u2.ID != u.ID || !stale.Valid() {
		t.Fatalf("stale fallback: %v %v %v", u2, stale, err)
	}
	if err := s.ReceiptCurrent(ctx, acc); err == nil {
		t.Fatal("positive receipt survived past CacheTTL")
	}
	if err := s.ReceiptCurrent(ctx, stale); err != nil {
		t.Fatalf("stale receipt: %v", err)
	}
	c.add(s.o.StaleTTL)
	if _, _, err := s.Accept(ctx, "home-user"); err == nil {
		t.Fatal("token worked past StaleTTL")
	}
	if err := s.ReceiptCurrent(ctx, stale); err == nil {
		t.Fatal("stale receipt worked past StaleTTL")
	}
}

func TestInvalidateRetiresAcceptedReceipt(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	_, acc, err := s.Accept(ctx, "home-user")
	if err != nil {
		t.Fatal(err)
	}
	s.Invalidate("home-user")
	if err := s.ReceiptCurrent(ctx, acc); err == nil {
		t.Fatal("invalidated receipt still current")
	}
	if _, err := s.Authenticate(ctx, "home-user"); err != nil {
		t.Fatalf("Authenticate after invalidate: %v", err)
	}
}

func TestDelegationReceiptExpiresWithRow(t *testing.T) {
	s, _, c := setup(t)
	ctx := context.Background()
	owner, err := s.Authenticate(ctx, "server-token")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.IssueDelegation(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, acc, err := s.Accept(ctx, token)
	if err != nil || !u.Delegated || !acc.Valid() {
		t.Fatalf("delegation accept: %v %+v %v", u, acc, err)
	}
	if err := s.ReceiptCurrent(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_delegations WHERE token_hash=?`, HashToken(token)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Accept(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("deleted delegation: %v", err)
	}
	if err := s.ReceiptCurrent(ctx, acc); err == nil {
		t.Fatal("deleted delegation receipt still current")
	}
	c.add(time.Minute)
}

func TestDurableTokenReceiptSurvivesCacheTTL(t *testing.T) {
	s, _, c := setup(t)
	ctx := context.Background()
	_, acc, err := s.Accept(ctx, "alice-access")
	if err != nil || !acc.Valid() {
		t.Fatal(err)
	}
	c.add(s.o.CacheTTL + s.o.StaleTTL)
	if _, _, err := s.Accept(ctx, "alice-access"); err != nil {
		t.Fatalf("synced token after cache window: %v", err)
	}
	if err := s.ReceiptCurrent(ctx, acc); err != nil {
		t.Fatalf("durable receipt: %v", err)
	}
}
