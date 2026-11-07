package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestReady(t *testing.T) {
	tests := []struct {
		name      string
		checks    []Result
		wantCode  int
		wantState State
	}{
		{"all ok", []Result{{Name: "a", State: OK}}, 200, OK},
		{"warn is still ready", []Result{{Name: "a", State: OK}, {Name: "b", State: Warn}}, 200, Warn},
		{"fail is not ready", []Result{{Name: "a", State: Warn}, {Name: "b", State: Fail}}, 503, Fail},
		{"skipped ranks below ok", []Result{{Name: "a", State: Skipped}, {Name: "b", State: OK}}, 200, Skipped},
		{"no checks", nil, 200, Skipped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := Handler("0.1.0", time.Now(), func() []Result { return tc.checks }, prometheus.NewRegistry())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/ready", nil))
			var rep report
			if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.wantCode || rep.State != tc.wantState || rep.Ready != (tc.wantCode == 200) {
				t.Fatalf("got %d %+v", rec.Code, rep)
			}
		})
	}
}
