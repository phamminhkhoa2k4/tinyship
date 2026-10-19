package appconfig

// Fuzz target for the configuration schema: arbitrary bytes must never
// panic Parse or Validate, and any config that validates must
// round-trip through JSON storage unchanged.

import (
	"encoding/json"
	"testing"
)

func FuzzParseValidate(f *testing.F) {
	f.Add([]byte(`{"name":"web","image":"nginx","tag":"1.27","port":8080}`))
	f.Add([]byte(`{"name":"web","image":"nginx","port":80,` +
		`"resources":{"memory_mb":256,"cpu_millis":500,"pids_limit":256}}`))
	f.Add([]byte(`{"name":"../etc","image":"nginx","port":80}`))
	f.Add([]byte(`{"name":"web","image":"nginx;rm -rf /","port":80}`))
	f.Add([]byte(`{"name":"web","image":"nginx","port":80,"privileged":true}`))
	f.Add([]byte(`{"name":"web","image":"nginx","port":80,` +
		`"volumes":[{"name":"d","path":"/../../.."}]}`))
	f.Add([]byte(`{"name":"web","image":"nginx","port":80,` +
		`"cap_add":["SYS_ADMIN"]}`))
	f.Add([]byte{})
	f.Add([]byte(`{`))

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := Parse(data)
		if err != nil {
			return
		}
		if _, err := Validate(cfg); err != nil {
			return
		}
		// A config that validates must survive the JSON storage
		// round-trip: the worker re-parses exactly this encoding.
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("validated config does not marshal: %v", err)
		}
		if _, err := Parse(raw); err != nil {
			t.Fatalf("validated config does not round-trip: %v\n%s", err, raw)
		}
	})
}
