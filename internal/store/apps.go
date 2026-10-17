package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAppNameExists is returned when CreateApp collides with an existing app
// name.
var ErrAppNameExists = errors.New("E_VALIDATION: app name already exists")

// ErrNotFound is returned by lookups that expect a row (apps, deployments).
var ErrNotFound = errors.New("not found")

// App is a row of the apps table. ConfigJSON is the validated,
// strict-schema app config as of ConfigVersion.
type App struct {
	ID                  string
	Name                string
	ConfigJSON          string
	ConfigVersion       int
	DesiredState        string
	CurrentDeploymentID *string
	RegistryCredential  *string
	Deleting            bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// CreateAppInput is the write shape for CreateApp.
type CreateAppInput struct {
	Name       string
	ConfigJSON string
	Actor      string // audit trail
}

// CreateApp inserts a new app. The caller validates the config first
// (appconfig.Validate); the store only enforces schema constraints.
func (s *Store) CreateApp(ctx context.Context, in CreateAppInput) (*App, error) {
	now := time.Now().UTC()
	id := NewIDAt(now)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO apps (id, name, config_json, config_version, desired_state,
		                  created_at, updated_at)
		VALUES (?, ?, ?, 1, 'running', ?, ?)`,
		id, in.Name, in.ConfigJSON, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: apps.name") {
			return nil, fmt.Errorf("%w: %q", ErrAppNameExists, in.Name)
		}
		return nil, fmt.Errorf("store: create app: %w", err)
	}
	return s.GetAppByID(ctx, id)
}

// GetAppByID fetches one app by id (ErrNotFound when absent).
func (s *Store) GetAppByID(ctx context.Context, id string) (*App, error) {
	return s.scanApp(s.db.QueryRowContext(ctx, `
		SELECT id, name, config_json, config_version, desired_state,
		       current_deployment_id, registry_credential_id, deleting,
		       created_at, updated_at
		FROM apps WHERE id = ?`, id))
}

// GetAppByName fetches one app by name (ErrNotFound when absent).
func (s *Store) GetAppByName(ctx context.Context, name string) (*App, error) {
	return s.scanApp(s.db.QueryRowContext(ctx, `
		SELECT id, name, config_json, config_version, desired_state,
		       current_deployment_id, registry_credential_id, deleting,
		       created_at, updated_at
		FROM apps WHERE name = ?`, name))
}

// ListApps returns every app ordered by name (CLI `app list`).
func (s *Store) ListApps(ctx context.Context) ([]*App, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, config_json, config_version, desired_state,
		       current_deployment_id, registry_credential_id, deleting,
		       created_at, updated_at
		FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list apps: %w", err)
	}
	defer rows.Close()
	var out []*App
	for rows.Next() {
		app, err := scanAppRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

func (s *Store) scanApp(row *sql.Row) (*App, error) {
	app, err := scanAppRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return app, err
}

type scannable interface {
	Scan(dest ...any) error
}

func scanAppRow(row scannable) (*App, error) {
	var (
		a          App
		currentDDL sql.NullString
		credDDL    sql.NullString
		deleting   int
		createdMS  int64
		updatedMS  int64
	)
	err := row.Scan(&a.ID, &a.Name, &a.ConfigJSON, &a.ConfigVersion,
		&a.DesiredState, &currentDDL, &credDDL, &deleting,
		&createdMS, &updatedMS)
	if err != nil {
		return nil, err
	}
	a.CurrentDeploymentID = nullStr(currentDDL)
	a.RegistryCredential = nullStr(credDDL)
	a.Deleting = deleting != 0
	a.CreatedAt = time.UnixMilli(createdMS).UTC()
	a.UpdatedAt = time.UnixMilli(updatedMS).UTC()
	return &a, nil
}

func nullStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
