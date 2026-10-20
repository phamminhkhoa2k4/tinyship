package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tinyship/tinyship/internal/policy"
	"github.com/tinyship/tinyship/internal/store"
)

// addRelease records one successful release with a distinct digest and
// finishes its job so the app has no active work left.
func addRelease(t *testing.T, s *store.Store, app *store.App, cfgJSON, imageRef, digest string) *store.Deployment {
	t.Helper()
	ctx := context.Background()
	depID := store.NewID()
	dep, job, err := s.CreateDeploymentJob(ctx, store.CreateDeploymentInput{
		ID: depID, AppID: app.ID, Kind: "deploy", ImageRef: imageRef,
		ConfigSnapshotJSON: cfgJSON, ConfigVersion: 1,
	}, store.CreateJobInput{
		AppID: &app.ID, Type: DeployJobType,
		PayloadJSON: fmt.Sprintf(`{"deployment_id":%q}`, depID), CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if err := s.SetDeploymentImageDigest(ctx, dep.ID, digest); err != nil {
		t.Fatalf("set digest: %v", err)
	}
	if err := s.SucceedDeployment(ctx, app.ID, dep.ID); err != nil {
		t.Fatalf("succeed: %v", err)
	}
	claimed, err := s.Claim(ctx, "test", time.Now().UTC(), time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim job: got %v, err %v", claimed, err)
	}
	if err := s.FinishJob(ctx, claimed.ID, "test", store.JobSucceeded, "", time.Now().UTC()); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	return dep
}

func gcDigest(n int) string { return fmt.Sprintf("sha256:%064x", n) }

func TestGCKeepsNewestReleases(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const cfgJSON = `{"name":"web","image":"traefik/whoami","tag":"v1","port":8080,"keep_releases":2}`
	app, err := s.CreateApp(ctx, store.CreateAppInput{Name: "web", ConfigJSON: cfgJSON, Actor: "cli"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	digests := []string{gcDigest(1), gcDigest(2), gcDigest(3), gcDigest(4)}
	for _, d := range digests {
		addRelease(t, s, app, cfgJSON, "traefik/whoami:v1", d)
	}

	fk := newFakeDocker()
	for i, d := range digests {
		fk.images["traefik/whoami@"+d] = int64(100 * (i + 1))
	}
	g := NewGCRunner(s, fk, "test-worker", slog.New(slog.DiscardHandler))

	dry, err := g.Run(ctx, GCOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dry.Removed) != 2 || dry.FreedBytes != 300 {
		t.Fatalf("dry run removed = %+v, freed = %d, want two images and 300 bytes",
			dry.Removed, dry.FreedBytes)
	}
	if len(fk.removedIm) != 0 {
		t.Fatalf("dry run removed images from docker: %v", fk.removedIm)
	}
	for _, img := range dry.Removed {
		if !strings.HasSuffix(img.Digest, fmt.Sprintf("%064x", 1)) &&
			!strings.HasSuffix(img.Digest, fmt.Sprintf("%064x", 2)) {
			t.Errorf("dry run touched a kept release: %+v", img)
		}
	}

	real, err := g.Run(ctx, GCOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(real.Removed) != 2 || len(fk.removedIm) != 2 {
		t.Fatalf("real run removed = %+v / docker = %v", real.Removed, fk.removedIm)
	}
	for i, d := range digests {
		present := fk.images["traefik/whoami@"+d] > 0
		wantPresent := i >= 2
		if present != wantPresent {
			t.Errorf("digest %d present = %v, want %v", i+1, present, wantPresent)
		}
	}

	again, err := g.Run(ctx, GCOptions{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(again.Removed) != 0 {
		t.Errorf("second run removed %+v, want nothing", again.Removed)
	}
}

func TestGCKeepsImagesUsedByContainers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const cfgJSON = `{"name":"web","image":"traefik/whoami","tag":"v1","port":8080,"keep_releases":1}`
	app, err := s.CreateApp(ctx, store.CreateAppInput{Name: "web", ConfigJSON: cfgJSON, Actor: "cli"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	old, current := gcDigest(11), gcDigest(12)
	addRelease(t, s, app, cfgJSON, "traefik/whoami:v1", old)
	addRelease(t, s, app, cfgJSON, "traefik/whoami:v1", current)

	fk := newFakeDocker()
	fk.images["traefik/whoami@"+old] = 10
	fk.images["traefik/whoami@"+current] = 10
	// A stopped container still pins the old image on the daemon.
	fk.addContainer("cid-old", "ts-web-old", "exited", map[string]string{
		policy.LabelManaged: policy.ManagedValue,
		policy.LabelApp:     app.ID,
	})
	fk.containers["cid-old"].image = "traefik/whoami@" + old

	g := NewGCRunner(s, fk, "test-worker", slog.New(slog.DiscardHandler))
	res, err := g.Run(ctx, GCOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("removed %+v, want the container's image kept", res.Removed)
	}
}

func TestGCSkipsAppWithActiveJob(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const cfgJSON = `{"name":"web","image":"traefik/whoami","tag":"v1","port":8080,"keep_releases":1}`
	app, err := s.CreateApp(ctx, store.CreateAppInput{Name: "web", ConfigJSON: cfgJSON, Actor: "cli"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	old, current := gcDigest(21), gcDigest(22)
	addRelease(t, s, app, cfgJSON, "traefik/whoami:v1", old)
	addRelease(t, s, app, cfgJSON, "traefik/whoami:v1", current)

	fk := newFakeDocker()
	fk.images["traefik/whoami@"+old] = 10
	fk.images["traefik/whoami@"+current] = 10

	if _, err := s.CreateJob(ctx, store.CreateJobInput{
		AppID: &app.ID, Type: DeployJobType, PayloadJSON: `{}`, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("create active job: %v", err)
	}

	g := NewGCRunner(s, fk, "test-worker", slog.New(slog.DiscardHandler))
	res, err := g.Run(ctx, GCOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Removed) != 0 || len(res.Skipped) != 1 ||
		res.Skipped[0].Reason != "deployment in flight" {
		t.Errorf("result = %+v, want the app skipped", res)
	}
}

func TestGCUnknownApp(t *testing.T) {
	s := testStore(t)
	g := NewGCRunner(s, newFakeDocker(), "test-worker", slog.New(slog.DiscardHandler))
	if _, err := g.Run(context.Background(), GCOptions{App: "ghost"}); err == nil {
		t.Fatal("unknown app accepted")
	}
}

func TestGCFilteredRunStillKeepsOtherAppsImages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	cfgA := `{"name":"alpha","image":"traefik/whoami","tag":"v1","port":8080,"keep_releases":1}`
	cfgB := `{"name":"beta","image":"traefik/whoami","tag":"v1","port":8080,"keep_releases":1}`
	appA, err := s.CreateApp(ctx, store.CreateAppInput{Name: "alpha", ConfigJSON: cfgA, Actor: "cli"})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	appB, err := s.CreateApp(ctx, store.CreateAppInput{Name: "beta", ConfigJSON: cfgB, Actor: "cli"})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}

	shared, newer := gcDigest(31), gcDigest(32)
	addRelease(t, s, appA, cfgA, "traefik/whoami:v1", shared)
	addRelease(t, s, appA, cfgA, "traefik/whoami:v1", newer)
	addRelease(t, s, appB, cfgB, "traefik/whoami:v1", shared)

	fk := newFakeDocker()
	fk.images["traefik/whoami@"+shared] = 10
	fk.images["traefik/whoami@"+newer] = 10

	g := NewGCRunner(s, fk, "test-worker", slog.New(slog.DiscardHandler))
	res, err := g.Run(ctx, GCOptions{App: "alpha"})
	if err != nil {
		t.Fatalf("filtered run: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("removed %+v, want the shared digest kept for beta", res.Removed)
	}
	if len(fk.removedIm) != 0 {
		t.Errorf("docker removed %v, want nothing", fk.removedIm)
	}
}
