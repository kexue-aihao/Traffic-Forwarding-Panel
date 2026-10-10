package contract

import (
	"encoding/json"
	"testing"
)

func TestGroupAdvancedDefaultsZeroAndAlias(t *testing.T) {
	for _, tc := range []struct {
		json      string
		max, cool int
	}{{`{}`, 3, 30}, {`{"policy_version":2,"max_fail":0,"fail_timout_sec":0}`, 0, 0}, {`{"fail_timeout_sec":8}`, 3, 8}} {
		var v GroupAdvanced
		if e := json.Unmarshal([]byte(tc.json), &v); e != nil || v.MaxFail != tc.max || v.FailTimeoutSec != tc.cool {
			t.Fatalf("%s: %+v %v", tc.json, v, e)
		}
	}
	for _, bad := range []string{`{"fail_timeout_sec":1,"fail_timout_sec":2}`, `{"max_fail":"3"}`, `{"unknown":true}`} {
		var v GroupAdvanced
		if json.Unmarshal([]byte(bad), &v) == nil {
			t.Fatal("accepted invalid advanced config")
		}
	}
}
