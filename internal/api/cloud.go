package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	exo "github.com/exoscale/egoscale/v3"
	"github.com/exoscale/egoscale/v3/credentials"
)

// guarded only lets requests to our own run through (same host, same /runs/{id}/ prefix).
type guarded struct{ root string }

func (g guarded) RoundTrip(r *http.Request) (*http.Response, error) {
	root, _ := url.Parse(g.root)
	prefix := strings.TrimSuffix(root.Path, "/v2") + "/"
	prefix = prefix[:strings.Index(prefix, "/cloud/")+1]
	if r.URL.Scheme != root.Scheme || r.URL.Host != root.Host || !strings.HasPrefix(r.URL.Path, prefix) {
		return nil, fmt.Errorf("request leaves the run: %s", r.URL)
	}
	return Transport.RoundTrip(r)
}

func client(acc Account, target string) (*exo.Client, error) {
	if err := CheckEndpoint(target, acc.Endpoint); err != nil {
		return nil, err
	}
	return exo.NewClient(credentials.NewStaticCredentials(acc.Key, acc.Secret),
		exo.ClientOptWithEndpoint(exo.Endpoint(target)),
		exo.ClientOptWithHTTPClient(&http.Client{Timeout: 20 * time.Second, Transport: guarded{acc.Endpoint},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
}

// VM is a VM of the run as listed by the cloud API.
type VM struct {
	ID, Name, Zone, State string
}

// Cloud holds one client per zone, the instance types (family.size) and a template. Discover loads everything
// once before the first Create: while the run is READY the clock is not ticking yet, so the time is free.
type Cloud struct {
	acc       Account
	zones     map[string]*exo.Client
	types     map[string]map[string]*exo.InstanceType
	templates map[string]*exo.Template
}

func NewCloud(acc Account) *Cloud {
	return &Cloud{acc: acc, zones: map[string]*exo.Client{}, types: map[string]map[string]*exo.InstanceType{},
		templates: map[string]*exo.Template{}}
}

func (c *Cloud) Discover(ctx context.Context) error {
	d, err := client(c.acc, c.acc.Endpoint)
	if err != nil {
		return err
	}
	zs, err := d.ListZones(ctx)
	if err != nil {
		return err
	}
	for _, z := range zs.Zones {
		cl, err := client(c.acc, string(z.APIEndpoint))
		if err != nil {
			return err
		}
		c.zones[string(z.Name)] = cl
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for name, cl := range c.zones {
		wg.Add(1)
		go func(name string, cl *exo.Client) {
			defer wg.Done()
			types, err := cl.ListInstanceTypes(ctx)
			var tpl *exo.ListTemplatesResponse
			if err == nil {
				tpl, err = cl.ListTemplates(ctx, exo.ListTemplatesWithVisibility(exo.ListTemplatesVisibilityPublic))
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", name, err)
				}
				return
			}
			m := map[string]*exo.InstanceType{}
			for i := range types.InstanceTypes {
				t := types.InstanceTypes[i]
				m[string(t.Family)+"."+string(t.Size)] = &t
			}
			c.types[name] = m
			if len(tpl.Templates) > 0 {
				c.templates[name] = &tpl.Templates[0]
			}
		}(name, cl)
	}
	wg.Wait()
	return firstErr
}

// List lists the VMs of all zones in parallel.
func (c *Cloud) List(ctx context.Context) ([]VM, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []VM
	var firstErr error
	for name, cl := range c.zones {
		wg.Add(1)
		go func(name string, cl *exo.Client) {
			defer wg.Done()
			r, err := cl.ListInstances(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, v := range r.Instances {
				out = append(out, VM{ID: string(v.ID), Name: v.Name, Zone: name, State: string(v.State)})
			}
		}(name, cl)
	}
	wg.Wait()
	return out, firstErr
}

// Create creates a VM and binds it to a workload via user-data. The caller checks legality right before
// (spec.Legal); nothing is decided here. Without workloadID the VM has no binding (idle infrastructure),
// e.g. to start the clock of a READY run.
func (c *Cloud) Create(ctx context.Context, zone, family, size, name, workloadID string) (string, error) {
	cl := c.zones[zone]
	t := c.types[zone][family+"."+size]
	tpl := c.templates[zone]
	if cl == nil || t == nil || tpl == nil {
		return "", fmt.Errorf("zone %s: profile %s.%s or template missing", zone, family, size)
	}
	req := exo.CreateInstanceRequest{Name: name, InstanceType: t, Template: tpl, DiskSize: 10}
	if workloadID != "" {
		binding, _ := json.Marshal(map[string]any{"greenops": map[string]string{"schema_version": "1.0", "workload_id": workloadID}})
		req.UserData = base64.StdEncoding.EncodeToString(binding)
	}
	op, err := cl.CreateInstance(ctx, req)
	if err != nil {
		return "", err
	}
	if op.Reference != nil {
		return string(op.Reference.ID), nil
	}
	return "", nil // ID unknown: the controller finds the VM by name on the next List
}

func (c *Cloud) Start(ctx context.Context, zone, id string) error {
	_, err := c.zones[zone].StartInstance(ctx, exo.UUID(id), exo.StartInstanceRequest{})
	return err
}

func (c *Cloud) Stop(ctx context.Context, zone, id string) error {
	_, err := c.zones[zone].StopInstance(ctx, exo.UUID(id))
	return err
}

func (c *Cloud) Delete(ctx context.Context, zone, id string) error {
	_, err := c.zones[zone].DeleteInstance(ctx, exo.UUID(id))
	return err
}
