package main

import (
	"context"
	"cpa-codex-candy-guard/internal/guard"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// This server uses synthetic accounts only and cannot contact CPA or disable real accounts.
type demoClient struct {
	mu          sync.Mutex
	credentials []guard.Credential
	records     map[string]guard.Record
}

func (c *demoClient) Snapshot(context.Context) (guard.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]guard.Credential{}, c.credentials...)
	for i := range out {
		out[i].Records = append([]guard.Record{}, out[i].Records...)
	}
	return guard.Snapshot{Credentials: out}, nil
}
func (c *demoClient) Models(context.Context, guard.Credential) ([]string, error) {
	return []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-terra"}, nil
}
func (c *demoClient) Start(_ context.Context, cred guard.Credential, s guard.Settings) (guard.StartAck, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := guard.Record{ID: fmt.Sprint(time.Now().UnixNano()), Time: time.Now().UTC(), Model: s.Model, Status: "completed", Attribution: &guard.Attribution{Prediction: "gpt-6-astra"}}
	c.records[r.ID] = r
	for i := range c.credentials {
		if c.credentials[i].ID == cred.ID {
			c.credentials[i].Records = append(c.credentials[i].Records, r)
		}
	}
	return guard.StartAck{Started: 1}, nil
}
func (c *demoClient) Detail(_ context.Context, _, id string) (guard.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records[id], nil
}
func (c *demoClient) Disable(_ context.Context, cred guard.Credential) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.credentials {
		if c.credentials[i].ID == cred.ID {
			c.credentials[i].Disabled = true
		}
	}
	return nil
}
func main() {
	c := &demoClient{records: map[string]guard.Record{}}
	for i := range 12 {
		c.credentials = append(c.credentials, guard.Credential{ID: fmt.Sprint(i), Name: fmt.Sprintf("codex-demo-account-%02d.json", i+1), AuthIndex: fmt.Sprint(i)})
	}
	dir, err := os.MkdirTemp("", "candy-guard-demo-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	e, err := guard.NewEngine(c, filepath.Join(dir, "state.json"), guard.Options{})
	if err != nil {
		panic(err)
	}
	defer e.Close()
	server := &http.Server{Addr: "127.0.0.1:28931", Handler: guard.Handler(e), ReadHeaderTimeout: 5 * time.Second}
	log.Print("Synthetic preview: http://127.0.0.1:28931" + guard.UIPath)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Print(err)
	}
}
