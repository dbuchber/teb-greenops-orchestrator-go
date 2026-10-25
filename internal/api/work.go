package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Status ist die Antwort von GET {work}/status: maßgebliche Simulationszeit, Laufzustand und je Workload
// Fortschritt, Lieferung und zugeordnete VMs.
type Status struct {
	RunID         string           `json:"run_id"`
	VirtualTime   time.Time        `json:"virtual_time"`
	Speed         int              `json:"speed_multiplier"`
	State         string           `json:"state"`
	Qualification string           `json:"qualification"`
	Workloads     []WorkloadStatus `json:"workloads"`
}

type WorkloadStatus struct {
	ID            string       `json:"id"`
	State         string       `json:"state"`
	RequiredWork  string       `json:"required_work"`
	CompletedWork string       `json:"completed_work"`
	LostWork      string       `json:"lost_work"`
	Delivered     bool         `json:"final_output_delivered"`
	Assignments   []Assignment `json:"assignments"`
}

type Assignment struct {
	VMID  string `json:"vm_id"`
	Zone  string `json:"zone"`
	State string `json:"state"` // creating, running, stopped
}

func (w WorkloadStatus) Completed() float64 {
	v, _ := strconv.ParseFloat(w.CompletedWork, 64)
	return v
}

// Ended ist wahr, wenn der Lauf nicht mehr gesteuert werden kann.
func (s Status) Ended() bool {
	return s.State == "COMPLETED" || s.State == "INVALIDATED" || s.State == "UNAVAILABLE"
}

type Work struct {
	acc  Account
	url  string
	http *http.Client
}

// NewWork nimmt GREENOPS_WORK_ENDPOINT oder leitet ihn aus dem Cloud-Endpunkt ab (/runs/{id}/work/v1).
func NewWork(acc Account, workEndpoint string) (*Work, error) {
	if err := CheckEndpoint(acc.Endpoint, ""); err != nil {
		return nil, err
	}
	if workEndpoint == "" {
		workEndpoint = strings.TrimSuffix(acc.Endpoint, "/cloud/v2") + "/work/v1"
	}
	return &Work{acc: acc, url: strings.TrimSuffix(workEndpoint, "/") + "/status", http: newHTTP(10 * time.Second)}, nil
}

func (w *Work) Status(ctx context.Context) (Status, error) {
	var out Status
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.url, nil)
	if err != nil {
		return out, err
	}
	signEXO2(req, w.acc.Key, w.acc.Secret)
	resp, err := w.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return out, RateLimited{}
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("work status HTTP %d", resp.StatusCode)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
