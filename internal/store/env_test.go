package store

import (
	"context"
	"errors"
	"testing"
)

func TestSetAppEnvPlainAndSecret(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	if err := s.SetAppEnv(ctx, appID, "GREETING", EnvPlain, "hi", nil); err != nil {
		t.Fatalf("set plain: %v", err)
	}
	if err := s.SetAppEnv(ctx, appID, "EMPTY", EnvPlain, "", nil); err != nil {
		t.Fatalf("set empty plain: %v", err)
	}
	if err := s.SetAppEnv(ctx, appID, "TOKEN", EnvSecret, "", []byte("sealed-blob")); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	vars, err := s.ListAppEnv(ctx, appID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(vars) != 3 {
		t.Fatalf("list len = %d, want 3", len(vars))
	}
	byKey := map[string]EnvVar{}
	for _, v := range vars {
		byKey[v.Key] = v
	}
	if got := byKey["GREETING"]; got.Kind != EnvPlain || got.ValuePlain != "hi" || got.ValueSealed != nil {
		t.Errorf("GREETING = %+v, want plain 'hi'", got)
	}
	// An empty plain value must survive the round trip: the schema
	// CHECK wants value_plain NOT NULL for kind=plain.
	if got := byKey["EMPTY"]; got.Kind != EnvPlain || got.ValuePlain != "" {
		t.Errorf("EMPTY = %+v, want plain empty", got)
	}
	if got := byKey["TOKEN"]; got.Kind != EnvSecret || string(got.ValueSealed) != "sealed-blob" || got.ValuePlain != "" {
		t.Errorf("TOKEN = %+v, want secret sealed-blob", got)
	}
}

func TestSetAppEnvUpsertAndUnset(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	if err := s.SetAppEnv(ctx, appID, "PORT", EnvPlain, "8080", nil); err != nil {
		t.Fatalf("first set: %v", err)
	}
	if err := s.SetAppEnv(ctx, appID, "PORT", EnvPlain, "9090", nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	vars, err := s.ListAppEnv(ctx, appID)
	if err != nil || len(vars) != 1 || vars[0].ValuePlain != "9090" {
		t.Fatalf("after upsert vars = %+v err=%v, want one row 9090", vars, err)
	}

	// Switching kind on upsert must replace the whole row, not leave a
	// stale value in the other column.
	if err := s.SetAppEnv(ctx, appID, "PORT", EnvSecret, "", []byte("sealed")); err != nil {
		t.Fatalf("kind switch: %v", err)
	}
	vars, _ = s.ListAppEnv(ctx, appID)
	if vars[0].Kind != EnvSecret || string(vars[0].ValueSealed) != "sealed" {
		t.Fatalf("after kind switch = %+v, want secret sealed", vars[0])
	}

	if err := s.UnsetAppEnv(ctx, appID, "PORT"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if err := s.UnsetAppEnv(ctx, appID, "PORT"); !errors.Is(err, ErrEnvNotFound) {
		t.Errorf("second unset = %v, want ErrEnvNotFound", err)
	}
}

func TestSetAppEnvRejectsMismatchedPayload(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	if err := s.SetAppEnv(ctx, appID, "K", EnvPlain, "v", []byte("sealed")); !errors.Is(err, ErrEnvInvalid) {
		t.Errorf("plain with sealed = %v, want ErrEnvInvalid", err)
	}
	if err := s.SetAppEnv(ctx, appID, "K", EnvSecret, "clear", []byte("sealed")); !errors.Is(err, ErrEnvInvalid) {
		t.Errorf("secret with clear value = %v, want ErrEnvInvalid", err)
	}
	if err := s.SetAppEnv(ctx, appID, "K", EnvSecret, "", nil); !errors.Is(err, ErrEnvInvalid) {
		t.Errorf("secret without sealed = %v, want ErrEnvInvalid", err)
	}
	if err := s.SetAppEnv(ctx, appID, "K", "bogus", "v", nil); !errors.Is(err, ErrEnvInvalid) {
		t.Errorf("unknown kind = %v, want ErrEnvInvalid", err)
	}
}

func TestCountAppEnv(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	n, err := s.CountAppEnv(ctx, appID)
	if err != nil || n != 0 {
		t.Fatalf("count empty = %d err=%v, want 0", n, err)
	}
	if err := s.SetAppEnv(ctx, appID, "A", EnvPlain, "1", nil); err != nil {
		t.Fatalf("set: %v", err)
	}
	if n, _ := s.CountAppEnv(ctx, appID); n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}
