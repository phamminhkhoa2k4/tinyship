package store

import (
	"context"
	"errors"
	"testing"
)

func TestUpsertCredentialCreatesAndRotates(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	c, err := s.UpsertCredential(ctx, "ghcr-linh", "ghcr.io", "linh", []byte("sealed-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Name != "ghcr-linh" || string(c.SecretSealed) != "sealed-1" {
		t.Fatalf("credential = %+v, want ghcr-linh sealed-1", c)
	}

	rotated, err := s.UpsertCredential(ctx, "ghcr-linh", "ghcr.io", "linh", []byte("sealed-2"))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if string(rotated.SecretSealed) != "sealed-2" {
		t.Errorf("rotated sealed = %q, want sealed-2", rotated.SecretSealed)
	}
	if !rotated.CreatedAt.Equal(c.CreatedAt) {
		t.Errorf("created_at changed on rotate: %v → %v", c.CreatedAt, rotated.CreatedAt)
	}

	list, err := s.ListCredentials(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v, want exactly 1", list, err)
	}
	if list[0].Name != "ghcr-linh" {
		t.Errorf("list name = %q", list[0].Name)
	}
}

func TestGetCredentialNotFound(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	_, err := s.GetCredential(ctx, "missing")
	if !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("get missing = %v, want ErrCredentialNotFound", err)
	}
	if err := s.DeleteCredential(ctx, "missing"); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("delete missing = %v, want ErrCredentialNotFound", err)
	}
}

func TestUpsertCredentialRequiresSealedToken(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.UpsertCredential(ctx, "x", "r", "u", nil); err == nil {
		t.Error("empty sealed token accepted, want error")
	}
}

func TestDeleteCredentialRefusedWhileReferenced(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	appID := createApp(t, s, "web")

	cred, err := s.UpsertCredential(ctx, "ghcr", "ghcr.io", "linh", []byte("sealed"))
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE apps SET registry_credential_id = ? WHERE id = ?`, cred.ID, appID); err != nil {
		t.Fatalf("link app: %v", err)
	}

	if err := s.DeleteCredential(ctx, "ghcr"); !errors.Is(err, ErrCredentialInUse) {
		t.Errorf("delete in use = %v, want ErrCredentialInUse", err)
	}

	if _, err := s.db.Exec(
		`UPDATE apps SET registry_credential_id = NULL WHERE id = ?`, appID); err != nil {
		t.Fatalf("unlink app: %v", err)
	}
	if err := s.DeleteCredential(ctx, "ghcr"); err != nil {
		t.Errorf("delete after unlink: %v", err)
	}
	if _, err := s.GetCredential(ctx, "ghcr"); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("get deleted = %v, want ErrCredentialNotFound", err)
	}
}
