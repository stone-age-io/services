package watchdog

import (
	"strings"
	"testing"
	"time"
)

func TestParseCheck(t *testing.T) {
	const eval = 10 * time.Second
	tests := []struct {
		name    string
		check   string
		json    string
		wantErr string // substring; "" means valid
	}{
		{"wildcard", "gateways", `{"subject":"site.*.heartbeat","target_token":1,"timeout":"5m","expected":["gw-01"]}`, ""},
		{"no wildcard", "hub", `{"subject":"hub.heartbeat","timeout":"1m"}`, ""},
		{"tail wildcard ok with star", "x", `{"subject":"a.*.>","target_token":1,"timeout":"1m"}`, ""},
		{"bad name", "a.b", `{"subject":"hub.heartbeat","timeout":"1m"}`, "check name"},
		{"unknown field", "x", `{"subject":"hub.heartbeat","timeout":"1m","timout":"2m"}`, "unknown field"},
		{"missing subject", "x", `{"timeout":"1m"}`, "subject is required"},
		{"missing target_token", "x", `{"subject":"site.*.hb","timeout":"1m"}`, "target_token is required"},
		{"target_token without wildcard", "x", `{"subject":"site.hb","target_token":0,"timeout":"1m"}`, "must be omitted"},
		{"target_token not star", "x", `{"subject":"site.*.hb","target_token":0,"timeout":"1m"}`, "must point at a '*'"},
		{"target_token out of range", "x", `{"subject":"site.*.hb","target_token":7,"timeout":"1m"}`, "must point at a '*'"},
		{"only tail wildcard", "x", `{"subject":"site.>","target_token":1,"timeout":"1m"}`, "must point at a '*'"},
		{"gt not last", "x", `{"subject":"a.>.b","target_token":1,"timeout":"1m"}`, "must be the last"},
		{"partial wildcard", "x", `{"subject":"a.b*.c","timeout":"1m"}`, "whole token"},
		{"empty token", "x", `{"subject":"a..c","timeout":"1m"}`, "empty token"},
		{"missing timeout", "x", `{"subject":"hub.hb"}`, "timeout is required"},
		{"short timeout", "x", `{"subject":"hub.hb","timeout":"20s"}`, "at least 3x"},
		{"bad max_targets", "x", `{"subject":"a.*","target_token":1,"timeout":"1m","max_targets":0}`, "max_targets must be"},
		{"too many expected", "x", `{"subject":"a.*","target_token":1,"timeout":"1m","max_targets":1,"expected":["a","b"]}`, "more than max_targets"},
		{"bad expected", "x", `{"subject":"a.*","target_token":1,"timeout":"1m","expected":["a b"]}`, "KV key"},
		{"expected without wildcard", "x", `{"subject":"hub.hb","timeout":"1m","expected":["a"]}`, "expected must be omitted"},
		{"not json", "x", `nope`, "invalid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCheck(tc.check, []byte(tc.json), eval)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseCheckReportsEveryProblem(t *testing.T) {
	_, err := ParseCheck("a.b", []byte(`{"subject":"a.*"}`), time.Second)
	if err == nil {
		t.Fatal("want error")
	}
	if n := len(strings.Split(err.Error(), "\n")); n != 3 {
		t.Fatalf("want 3 problems (name, target_token, timeout), got %d: %v", n, err)
	}
}

func TestCheckTarget(t *testing.T) {
	c, err := ParseCheck("gateways", []byte(`{"subject":"site.*.heartbeat","target_token":1,"timeout":"1m"}`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Target("site.gw-01.heartbeat"); got != "gw-01" {
		t.Fatalf("got %q", got)
	}
	single, _ := ParseCheck("hub", []byte(`{"subject":"hub.heartbeat","timeout":"1m"}`), time.Second)
	if got := single.Target("hub.heartbeat"); got != "hub" {
		t.Fatalf("got %q", got)
	}
}
