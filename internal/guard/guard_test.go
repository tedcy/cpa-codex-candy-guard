package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestJudge(t *testing.T) {
	tests := []struct{ name, status, requested, predicted, decision, message, sample, want string }{
		{"consistent", "completed", "gpt-6-astra", "gpt-6-astra", "", "", "", ""},
		{"mismatch", "completed", "gpt-6-astra", "gpt-5.6-luna", "", "", "", "model_mismatch"},
		{"partial mismatch", "partial", "gpt-6-astra", "gpt-5.6-luna", "partial", "", "", "model_mismatch"},
		{"sol alias", "completed", "openai/GPT-6.1-SOL", "gpt-6-astra", "", "", "", ""},
		{"deepseek alias", "partial", "deepseek-flash", "deepseek-v4.1-flash-1017", "", "", "", ""},
		{"not a model", "completed", "gpt-6-astra", "gpt-5.6-luna", "not_model", "", "", ""},
		{"missing attribution", "partial", "gpt-6-astra", "", "", "", "", ""},
		{"request failed", "failed", "gpt-6-astra", "gpt-5.6-luna", "", "timeout", "", ""},
		{"cancelled overload", "cancelled", "gpt-6-astra", "", "", "Our servers are currently overloaded. Please try again later.", "", ""},
		{"skipped overload", "skipped", "gpt-6-astra", "", "", "Our servers are currently overloaded. Please try again later.", "", ""},
		{"ordinary 503", "failed", "gpt-6-astra", "", "", "HTTP 503 overloaded", "", ""},
		{"generic word", "failed", "gpt-6-astra", "", "", "overloaded", "", ""},
		{"quota", "failed", "gpt-6-astra", "", "", "usage_limit_reached", "", ""},
		{"final overload message", "failed", "gpt-6-astra", "", "", "Our servers are currently overloaded. Please try again later.)", "", "final_overload"},
		{"final structured code", "failed", "gpt-6-astra", "", "", `request: {"error":{"code":"server_is_overloaded"}}`, "", "final_overload"},
		{"error type", "failed", "gpt-6-astra", "", "", `{"error":{"type":"server_is_overloaded"}}`, "", "final_overload"},
		{"partial sample overload", "partial", "gpt-6-astra", "gpt-6-astra", "", "", `{"code":"server_is_overloaded"}`, "final_overload"},
		{"retry recovered", "completed", "gpt-6-astra", "gpt-6-astra", "", "", "", ""},
		{"running record", "running", "gpt-6-astra", "gpt-5.6-luna", "", "", "", ""},
		{"unrelated code string", "failed", "gpt-6-astra", "", "", `{"message":"server_is_overloaded"}`, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Record{Status: tt.status, Model: tt.requested, Error: tt.message, Attribution: &Attribution{Prediction: tt.predicted, Decision: tt.decision}, Samples: []Sample{{Error: tt.sample}}}
			v := Judge(r)
			if v.Reason != tt.want || v.Disable != (tt.want != "") {
				t.Fatalf("got %+v, want %s", v, tt.want)
			}
		})
	}
}

type fakeTimer struct {
	clock *fakeClock
	id    int
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	_, ok := t.clock.timers[t.id]
	delete(t.clock.timers, t.id)
	return ok
}

type timed struct {
	at time.Time
	f  func()
}
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	next   int
	timers map[int]timed
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), timers: map[int]timed{}}
}
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	c.timers[c.next] = timed{c.now.Add(d), f}
	return &fakeTimer{c, c.next}
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var callbacks []func()
	for id, item := range c.timers {
		if !item.at.After(c.now) {
			callbacks = append(callbacks, item.f)
			delete(c.timers, id)
		}
	}
	c.mu.Unlock()
	for _, f := range callbacks {
		f()
	}
}

type fakeClient struct {
	mu           sync.Mutex
	clock        *fakeClock
	credentials  []Credential
	records      map[string]Record
	started      []string
	disabled     []string
	per          []int
	active, peak int
	automatic    bool
	prediction   string
	startErr     bool
	snapshotErr  bool
	disableErr   bool
}

func newClient(clock *fakeClock, count int) *fakeClient {
	f := &fakeClient{clock: clock, records: map[string]Record{}, prediction: "gpt-5.6-luna"}
	for i := range count {
		id := fmt.Sprint(i)
		f.credentials = append(f.credentials, Credential{ID: id, Name: "test-" + id + ".json", AuthIndex: "auth-" + id, Models: []string{"gpt-6-astra", "gpt-6.1-sol"}})
	}
	return f
}
func (f *fakeClient) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshotErr {
		return Snapshot{}, &DependencyError{"mock incompatible"}
	}
	a := make([]Credential, len(f.credentials))
	copy(a, f.credentials)
	for i := range a {
		a[i].Records = slices.Clone(a[i].Records)
	}
	return Snapshot{a}, nil
}
func (f *fakeClient) Models(_ context.Context, c Credential) ([]string, error) { return c.Models, nil }
func (f *fakeClient) Start(_ context.Context, c Credential, s Settings) (StartAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr {
		return StartAck{}, errors.New("failed")
	}
	f.started = append(f.started, c.ID)
	f.per = append(f.per, s.PerCredential)
	f.active++
	f.peak = max(f.peak, f.active)
	for i := range f.credentials {
		if f.credentials[i].ID == c.ID {
			f.credentials[i].Running = &Progress{Test: "fingerprint", Model: s.Model}
		}
	}
	if f.automatic {
		f.completeLocked(c.ID, s.Model)
	}
	return StartAck{Started: 1}, nil
}
func (f *fakeClient) completeLocked(id, model string) {
	r := Record{ID: fmt.Sprint(f.clock.Now().UnixNano()) + "-" + id, Time: f.clock.Now(), Model: model, Status: "completed", Attribution: &Attribution{Prediction: f.prediction}}
	f.records[r.ID] = r
	for i := range f.credentials {
		if f.credentials[i].ID == id {
			f.credentials[i].Records = append(f.credentials[i].Records, r)
			f.credentials[i].Running = nil
		}
	}
	f.active--
}
func (f *fakeClient) Complete(id, model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeLocked(id, model)
}
func (f *fakeClient) Detail(_ context.Context, _, id string) (Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[id], nil
}
func (f *fakeClient) Disable(_ context.Context, c Credential) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disableErr {
		return errors.New("failed")
	}
	f.disabled = append(f.disabled, c.ID)
	return nil
}
func (f *fakeClient) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), len(f.disabled), f.peak
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true")
		default:
			runtime.Gosched()
		}
	}
}
func idle(e *Engine) bool { e.mu.Lock(); defer e.mu.Unlock(); return e.active == "" && !e.starting }
func makeEngine(t *testing.T, count int) (*Engine, *fakeClient, *fakeClock) {
	t.Helper()
	clock := newClock()
	client := newClient(clock, count)
	e, err := NewEngine(client, filepath.Join(t.TempDir(), "state.json"), Options{Clock: clock, PollInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	s := DefaultSettings()
	s.Model = "gpt-6-astra"
	if err = e.Settings(s); err != nil {
		t.Fatal(err)
	}
	return e, client, clock
}

func TestEngineDisablesOnlyOwnedResults(t *testing.T) {
	e, c, clock := makeEngine(t, 2)
	c.credentials[0].Records = []Record{{ID: "old", Time: clock.Now(), Model: "gpt-6-astra", Status: "completed", Attribution: &Attribution{Prediction: "gpt-5.6-luna"}}}
	c.automatic = true
	if _, err := e.Start("manual"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return idle(e) })
	a, d, _ := c.counts()
	if a != 2 || d != 2 {
		t.Fatalf("starts=%d disables=%d", a, d)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range c.per {
		if v != 3 {
			t.Fatalf("per credential = %d", v)
		}
	}
}

func TestEngineOldHistoryNeverDisables(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	c.credentials[0].Records = []Record{{ID: "old", Time: clock.Now().Add(-time.Hour), Model: "gpt-6-astra", Status: "completed", Attribution: &Attribution{Prediction: "other"}}}
	c.startErr = true
	_, _ = e.Start("manual")
	eventually(t, func() bool { return idle(e) })
	_, d, _ := c.counts()
	if d != 0 {
		t.Fatal("old record disabled an account")
	}
}

func TestEngineGlobalConcurrencyAndStop(t *testing.T) {
	e, c, clock := makeEngine(t, 4)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 2 })
	if _, err := e.Start("manual"); !errors.Is(err, ErrBusy) {
		t.Fatal("overlapping run accepted")
	}
	e.Stop()
	c.Complete("0", "gpt-6-astra")
	c.Complete("1", "gpt-6-astra")
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	a, d, peak := c.counts()
	if a != 2 || d != 0 || peak > 2 {
		t.Fatalf("starts=%d disables=%d peak=%d", a, d, peak)
	}
}

func TestEngineSkipsDisabledBusyCooldownUnsupported(t *testing.T) {
	e, c, _ := makeEngine(t, 5)
	c.automatic = true
	c.credentials[0].Disabled = true
	c.credentials[1].Running = &Progress{Test: "candy"}
	c.credentials[2].Unavailable = true
	c.credentials[3].Models = []string{"other"}
	_, _ = e.Start("manual")
	eventually(t, func() bool { return idle(e) })
	a, d, _ := c.counts()
	if a != 1 || d != 1 {
		t.Fatalf("starts=%d disables=%d", a, d)
	}
}

func TestEngineSelectedAndSnapshotSettings(t *testing.T) {
	e, c, clock := makeEngine(t, 3)
	s := DefaultSettings()
	s.Model = "gpt-6-astra"
	s.Scope = "selected"
	s.CredentialIDs = []string{"1"}
	s.Global = 1
	s.PerCredential = 2
	_ = e.Settings(s)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	s.Model = "gpt-6.1-sol"
	s.PerCredential = 1
	_ = e.Settings(s)
	c.Complete("1", "gpt-6-astra")
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.started, []string{"1"}) || c.per[0] != 2 {
		t.Fatal("in-flight settings changed")
	}
}

func TestScheduleWaitsAfterCompletionAndPersists(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	c.prediction = "gpt-6-astra"
	s := DefaultSettings()
	s.Model = "gpt-6-astra"
	s.Scheduled = true
	s.IntervalMinutes = 2
	if err := e.Settings(s); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	clock.Advance(10 * time.Minute)
	a, _, _ := c.counts()
	if a != 1 {
		t.Fatal("timer overlapped active round")
	}
	c.Complete("0", s.Model)
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	e.mu.Lock()
	next := *e.next
	e.mu.Unlock()
	if next.Sub(clock.Now()) != 2*time.Minute {
		t.Fatal("interval not measured from completion")
	}
	clock.Advance(119 * time.Second)
	a, _, _ = c.counts()
	if a != 1 {
		t.Fatal("timer ran early")
	}
	clock.Advance(time.Second)
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 2 })
	e.Stop()
	c.Complete("0", s.Model)
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	s.Scheduled = false
	_ = e.Settings(s)
	clock.Advance(10 * time.Minute)
	a, _, _ = c.counts()
	if a != 2 {
		t.Fatal("disabled schedule fired")
	}
}

func TestDependencyFailurePausesAndNeverDisables(t *testing.T) {
	e, c, clock := makeEngine(t, 2)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 2 })
	c.mu.Lock()
	c.snapshotErr = true
	c.mu.Unlock()
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	_, d, _ := c.counts()
	if d != 0 {
		t.Fatal("dependency error disabled an account")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fault == "" || e.next != nil {
		t.Fatal("dependency failure did not pause")
	}
}

func TestRestartMarksInFlightInterrupted(t *testing.T) {
	clock := newClock()
	client := newClient(clock, 1)
	path := filepath.Join(t.TempDir(), "state.json")
	s := DefaultSettings()
	s.Model = "gpt-6-astra"
	s.Scheduled = true
	value := persisted{Version: 1, Settings: s, Rounds: []Round{{ID: "before", Status: "running", Started: clock.Now(), Items: []Item{{Status: "running"}}}}}
	raw, _ := json.Marshal(value)
	_ = os.WriteFile(path, raw, 0o600)
	e, err := NewEngine(client, path, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.data.Rounds[0].Status != "interrupted" || e.data.Rounds[0].Items[0].Status != "interrupted" {
		t.Fatal("old round was not interrupted")
	}
	a, d, _ := client.counts()
	if a != 0 || d != 0 {
		t.Fatal("restart replayed old actions")
	}
	if e.next == nil {
		t.Fatal("schedule not restored")
	}
}

func TestCorruptStateIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	_ = os.WriteFile(path, []byte("broken"), 0o600)
	clock := newClock()
	_, err := NewEngine(newClient(clock, 0), path, Options{})
	if err == nil {
		t.Fatal("invalid state accepted")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "broken" {
		t.Fatal("invalid state overwritten")
	}
}

func TestAmbiguousRecordsDoNotDisable(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	// Publish both candidates before the worker can observe either record.
	c.mu.Lock()
	c.completeLocked("0", "gpt-6-astra")
	clock.Advance(time.Nanosecond)
	c.completeLocked("0", "gpt-6-astra")
	c.mu.Unlock()
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	_, d, _ := c.counts()
	if d != 0 {
		t.Fatal("ambiguous records disabled an account")
	}
}

func TestRecordIdentityChangedDoesNotDisable(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	c.mu.Lock()
	c.credentials[0].Email = "changed"
	c.mu.Unlock()
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	_, d, _ := c.counts()
	if d != 0 {
		t.Fatal("changed identity disabled")
	}
}

func TestUnknownAttributionPauses(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	c.Complete("0", "gpt-6-astra")
	c.mu.Lock()
	for key, record := range c.records {
		record.Attribution.Decision = "future-schema"
		c.records[key] = record
	}
	c.mu.Unlock()
	eventually(t, func() bool { clock.Advance(time.Second); return idle(e) })
	_, disabled, _ := c.counts()
	if disabled != 0 {
		t.Fatal("unknown detector decision disabled an account")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fault == "" {
		t.Fatal("unknown detector schema did not pause")
	}
}

func TestPersistenceFailureBlocksActions(t *testing.T) {
	e, c, _ := makeEngine(t, 1)
	c.automatic = true
	e.path = filepath.Join(t.TempDir(), "file", "state.json")
	_ = os.WriteFile(filepath.Dir(e.path), []byte("not a directory"), 0o600)
	if _, err := e.Start("manual"); err == nil {
		t.Fatal("persistence error ignored")
	}
	_, d, _ := c.counts()
	if d != 0 {
		t.Fatal("disabled without durable intent")
	}
}

func TestRoundHistoryBound(t *testing.T) {
	e, c, _ := makeEngine(t, 0)
	c.automatic = true
	for range 23 {
		if _, err := e.Start("manual"); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { return idle(e) })
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.data.Rounds) != 20 {
		t.Fatal("history is not bounded")
	}
}
