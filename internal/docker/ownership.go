package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/tinyship/tinyship/internal/policy"
)

// ErrNotManaged marks an ownership check failure (E_NOT_MANAGED): the
// container either is not TinyShip's or does not belong
// to the requesting app's deployment history. Callers must audit-log these.
var ErrNotManaged = errors.New("E_NOT_MANAGED: container fails TinyShip ownership checks")

// OwnsDeployment reports whether containerID appears on a deployment record
// of appID in the database. The worker passes the
// store's implementation; tests pass a fake.
type OwnsDeployment func(ctx context.Context, appID, containerID string) (bool, error)

// Inspector returns a container's state and labels; the wrapper implements
// it, tests inject a fake.
type Inspector func(ctx context.Context, containerID string) (*ContainerInfo, error)

// AssertOwned performs the three mandatory ownership checks before any
// stop/remove/exec/log operation:
//
// 1. the container carries tinyship.managed=true;
// 2. its tinyship.app label equals the job's app id;
// 3. the container id belongs to a deployment of that app (DB lookup).
//
// Any failure returns ErrNotManaged wrapped with the reason; the caller
// refuses the operation and audits it.
func AssertOwned(ctx context.Context, inspect Inspector, appID, containerID string, owns OwnsDeployment) error {
	if appID == "" {
		return fmt.Errorf("%w: no app id in job payload (refusing to touch %s)", ErrNotManaged, containerID)
	}
	info, err := inspect(ctx, containerID)
	if err != nil {
		// Missing container: treat as not-managed to keep the audit trail
		// uniform; callers decide whether "already gone" is success.
		return fmt.Errorf("%w: inspect %s: %v", ErrNotManaged, containerID, err)
	}
	if info.Labels[policy.LabelManaged] != policy.ManagedValue {
		return fmt.Errorf("%w: %s lacks %s=%s (found %q)",
			ErrNotManaged, containerID, policy.LabelManaged, policy.ManagedValue,
			info.Labels[policy.LabelManaged])
	}
	if got := info.Labels[policy.LabelApp]; got != appID {
		return fmt.Errorf("%w: %s belongs to app %q, job targets %q",
			ErrNotManaged, containerID, got, appID)
	}
	if owns != nil {
		ok, err := owns(ctx, appID, containerID)
		if err != nil {
			return fmt.Errorf("%w: deployment lookup for %s: %v", ErrNotManaged, containerID, err)
		}
		if !ok {
			return fmt.Errorf("%w: no deployment of app %s references container %s",
				ErrNotManaged, appID, containerID)
		}
	}
	return nil
}
