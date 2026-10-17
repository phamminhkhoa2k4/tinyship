package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Host budget settings keys. An absent or zero budget_mb means "auto":
// the admission step derives it from MemTotal on every check, so a RAM
// upgrade takes effect without reconfiguring.
const (
	BudgetMBKey      = "budget_mb"
	MinFreeDiskMBKey = "min_free_disk_mb"
	CPUOvercommitKey = "cpu_overcommit"
)

// Defaults match the documented host budget: apps may use at most
// min_free_disk_mb of free disk before pulls are refused, and the CPU
// sum may exceed physical capacity by cpu_overcommit before a warning
// is recorded.
const (
	DefaultMinFreeDiskMB = 3072
	DefaultCPUOvercommit = 2.0
)

// BudgetSettings is the host-level guardrail configuration.
type BudgetSettings struct {
	// BudgetMB is the app RAM budget; 0 means auto (derived from
	// MemTotal at each admission check).
	BudgetMB int
	// MinFreeDiskMB is the free-disk floor for pulling images.
	MinFreeDiskMB int
	// CPUOvercommit is the allowed cpu_millis/vCPU ratio before
	// W_CPU_OVERCOMMIT; it never rejects a deploy on its own.
	CPUOvercommit float64
}

// GetBudgetSettings reads the guardrail settings, applying defaults for
// absent values and rejecting unparsable ones (a garbage limit must
// fail the check loudly, not silently disable it).
func (s *Store) GetBudgetSettings(ctx context.Context) (*BudgetSettings, error) {
	out := &BudgetSettings{MinFreeDiskMB: DefaultMinFreeDiskMB, CPUOvercommit: DefaultCPUOvercommit}
	rawBudget, err := s.GetSetting(ctx, BudgetMBKey)
	if err != nil {
		return nil, err
	}
	if rawBudget != "" {
		n, err := strconv.Atoi(rawBudget)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("store: setting %s=%q is not a non-negative integer",
				BudgetMBKey, rawBudget)
		}
		out.BudgetMB = n
	}
	rawDisk, err := s.GetSetting(ctx, MinFreeDiskMBKey)
	if err != nil {
		return nil, err
	}
	if rawDisk != "" {
		n, err := strconv.Atoi(rawDisk)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("store: setting %s=%q is not a non-negative integer",
				MinFreeDiskMBKey, rawDisk)
		}
		out.MinFreeDiskMB = n
	}
	rawCPU, err := s.GetSetting(ctx, CPUOvercommitKey)
	if err != nil {
		return nil, err
	}
	if rawCPU != "" {
		f, err := strconv.ParseFloat(rawCPU, 64)
		if err != nil || f <= 0 {
			return nil, fmt.Errorf("store: setting %s=%q is not a positive number",
				CPUOvercommitKey, rawCPU)
		}
		out.CPUOvercommit = f
	}
	return out, nil
}

// SetBudgetMB overrides the auto RAM budget; 0 restores the auto
// derivation from MemTotal.
func (s *Store) SetBudgetMB(ctx context.Context, mb int) error {
	if mb < 0 {
		return errors.New("store: budget must be non-negative")
	}
	return s.PutSetting(ctx, BudgetMBKey, strconv.Itoa(mb))
}

// SetMinFreeDiskMB overrides the free-disk floor.
func (s *Store) SetMinFreeDiskMB(ctx context.Context, mb int) error {
	if mb < 0 {
		return errors.New("store: min free disk must be non-negative")
	}
	return s.PutSetting(ctx, MinFreeDiskMBKey, strconv.Itoa(mb))
}

// ListRunningApps returns apps the budget must account for: everything
// with desired_state=running that is not being deleted. Each app's
// granted memory lives in its config JSON; callers parse ConfigJSON
// with appconfig.
func (s *Store) ListRunningApps(ctx context.Context) ([]*App, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, config_json, config_version, desired_state,
		       current_deployment_id, registry_credential_id, deleting,
		       created_at, updated_at
		FROM apps
		WHERE desired_state = 'running' AND deleting = 0
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list running apps: %w", err)
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

// FindRollbackSource returns the most recent deployment that is known
// good: succeeded or superseded by a later good release (both carry a
// pinned digest), excluding the given deployment. It answers "roll back
// to the previous good release"; ErrNotFound means no such release
// exists.
func (s *Store) FindRollbackSource(ctx context.Context, appID, excludeDeploymentID string) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx, deploymentSelect+`
		WHERE app_id = ? AND state IN ('succeeded','superseded')
		  AND image_digest IS NOT NULL AND id != ?
		ORDER BY seq DESC LIMIT 1`, appID, excludeDeploymentID)
	d, err := scanDeployment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: find rollback source: %w", err)
	}
	return d, nil
}

// FindDeploymentBySeq returns one deployment of an app by seq number.
func (s *Store) FindDeploymentBySeq(ctx context.Context, appID string, seq int) (*Deployment, error) {
	row := s.db.QueryRowContext(ctx, deploymentSelect+` WHERE app_id = ? AND seq = ?`, appID, seq)
	d, err := scanDeployment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: find deployment by seq: %w", err)
	}
	return d, nil
}
