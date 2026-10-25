package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckEndpoint(t *testing.T) {
	ok := "https://greenops.example/runs/abc/cloud/v2"
	if err := CheckEndpoint(ok, ""); err != nil {
		t.Fatal(err)
	}
	if err := CheckEndpoint("https://greenops.example/runs/abc/cloud/de-fra-1/v2", ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"https://api.exoscale.com/runs/abc/cloud/v2", "https://greenops.example/v2",
		"https://greenops.example/runs/other/cloud/de-fra-1/v2"} {
		if CheckEndpoint(bad, ok) == nil {
			t.Fatalf("%s hätte abgelehnt werden müssen", bad)
		}
	}
}

func TestWorkStatusSignedAndDecoded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runs/abc/work/v1/status" || !strings.HasPrefix(r.Header.Get("Authorization"), "EXO2-HMAC-SHA256 credential=k,") {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"run_id":"abc","virtual_time":"2025-09-30T05:30:00Z","speed_multiplier":720,"state":"RUNNING",
			"qualification":"QUALIFIED","workloads":[{"id":"w-01","state":"running","required_work":"1000",
			"completed_work":"12.500000","work_unit":"vcpu_hour","lost_work":"0.000000","final_output_delivered":false,
			"assignments":[{"vm_id":"v1","zone":"at-vie-1","state":"running"}]}]}`))
	}))
	defer srv.Close()
	w, err := NewWork(Account{Key: "k", Secret: "s", Endpoint: srv.URL + "/runs/abc/cloud/v2"}, "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Speed != 720 || st.Workloads[0].Completed() != 12.5 || st.Workloads[0].Assignments[0].VMID != "v1" {
		t.Fatalf("%+v", st)
	}
}

func TestGridForecast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("auth-token") != "g" || r.URL.Query().Get("zone") != "AT" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"zone":"AT","forecast":[{"datetime":"2025-09-30T06:00:00Z","carbonIntensity":123.4}],"updatedAt":"2025-09-30T06:00:00Z"}`))
	}))
	defer srv.Close()
	g, err := NewGrid(srv.URL+"/runs/abc/grid/v4", "g")
	if err != nil {
		t.Fatal(err)
	}
	fc, errs := g.Forecasts(context.Background(), []string{"AT"})
	if len(errs) > 0 || len(fc["AT"]) != 1 || fc["AT"][0].GCO2kW != 123.4 {
		t.Fatalf("%v %v", fc, errs)
	}
}
