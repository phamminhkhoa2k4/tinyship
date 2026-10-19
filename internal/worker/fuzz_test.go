package worker

// Fuzz target for the deploy job payload decoder: arbitrary payload
// text must never panic, and anything that decodes must carry a
// deployment id.

import "testing"

func FuzzDecodeDeployPayload(f *testing.F) {
	f.Add(`{"deployment_id":"01ABC"}`)
	f.Add(`{"deployment_id":"01ABC","force":true}`)
	f.Add(`{}`)
	f.Add(`{"deployment_id":"01ABC","privileged":true}`)
	f.Add(`{"deployment_id":"01ABC","binds":["/var/run/docker.sock:/var/run/docker.sock"]}`)
	f.Add(``)
	f.Add(`{`)

	f.Fuzz(func(t *testing.T, raw string) {
		pl, err := decodeDeployPayload(raw)
		if err != nil {
			if pl != nil {
				t.Fatalf("decode error returned a payload: %+v", pl)
			}
			return
		}
		if pl.DeploymentID == "" {
			t.Fatalf("payload accepted without a deployment id: %q", raw)
		}
	})
}
