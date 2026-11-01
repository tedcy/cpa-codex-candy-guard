package guard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func enableRecheck(t *testing.T, e *Engine, global, delay int) Settings {
	t.Helper()
	s := DefaultSettings()
	s.Model, s.Global, s.RecheckEnabled, s.RecheckDelay = "gpt-6-astra", global, true, delay
	if err := e.Settings(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func latestItem(e *Engine, credentialID string, index int) (Item, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.data.Rounds) > 0 {
		for _, item := range e.data.Rounds[len(e.data.Rounds)-1].Items {
			if item.CredentialID == credentialID && itemCheckIndex(item) == index {
				return item, true
			}
		}
	}
	return Item{}, false
}

func queuedRecheck(t *testing.T, e *Engine, credentialID string) Item {
	t.Helper()
	var item Item
	eventually(t, func() bool {
		var found bool
		item, found = latestItem(e, credentialID, 2)
		return found && item.Status == "queued" && item.DueAt != nil
	})
	return item
}

func driveClock(t *testing.T, clock *fakeClock, condition func() bool) {
	t.Helper()
	eventually(t, func() bool {
		if condition() {
			return true
		}
		clock.Advance(time.Second)
		// Let polling workers consume each virtual tick before advancing again.
		time.Sleep(time.Millisecond)
		return condition()
	})
}

func completeCheck(t *testing.T, e *Engine, c *fakeClient, clock *fakeClock, id string, index int) {
	t.Helper()
	c.Complete(id, "gpt-6-astra")
	driveClock(t, clock, func() bool {
		item, found := latestItem(e, id, index)
		return found && item.Status != "queued" && item.Status != "running"
	})
}

func startedCredentials(c *fakeClient) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.started)
}

func TestRecheckIndependentDelayAndSharedConcurrency(t *testing.T) {
	e, c, clock := makeEngine(t, 2)
	c.prediction = "gpt-6-astra"
	enableRecheck(t, e, 2, 2)
	if _, err := e.Start("manual"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 2 })
	completeCheck(t, e, c, clock, "0", 1)
	firstDue := *queuedRecheck(t, e, "0").DueAt
	if idle(e) {
		t.Fatal("round finished while a recheck was pending")
	}
	if _, err := e.Start("manual"); !errors.Is(err, ErrBusy) {
		t.Fatal("pending rechecks allowed another round")
	}
	clock.Advance(firstDue.Sub(clock.Now()) - time.Second)
	if a, _, _ := c.counts(); a != 2 {
		t.Fatal("recheck started before its due time")
	}
	clock.Advance(time.Second)
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 3 })
	if got := startedCredentials(c)[2]; got != "0" {
		t.Fatalf("expected account 0 recheck while account 1 was still on its first check, got %s", got)
	}
	completeCheck(t, e, c, clock, "0", 2)
	completeCheck(t, e, c, clock, "1", 1)
	secondDue := *queuedRecheck(t, e, "1").DueAt
	if !secondDue.After(firstDue) {
		t.Fatal("recheck time was measured from the whole round rather than each credential")
	}
	clock.Advance(secondDue.Sub(clock.Now()))
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 4 })
	completeCheck(t, e, c, clock, "1", 2)
	eventually(t, func() bool { return idle(e) })
	clock.Advance(20 * time.Minute)
	if a, d, peak := c.counts(); a != 4 || d != 0 || peak != 2 {
		t.Fatalf("starts=%d disables=%d peak=%d", a, d, peak)
	}
	for _, id := range []string{"0", "1"} {
		first, _ := latestItem(e, id, 1)
		second, _ := latestItem(e, id, 2)
		if first.RecordID == "" || first.RecordID == second.RecordID || first.Status != "completed" || second.Status != "completed" {
			t.Fatal("first and second records were not kept separately")
		}
	}
}

func TestRecheckFirstChecksHavePriority(t *testing.T) {
	e, c, clock := makeEngine(t, 4)
	c.prediction = "gpt-6-astra"
	enableRecheck(t, e, 2, 1)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 2 })
	completeCheck(t, e, c, clock, "0", 1)
	due := *queuedRecheck(t, e, "0").DueAt
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 3 })
	if startedCredentials(c)[2] != "2" {
		t.Fatal("waiting recheck occupied the slot needed by a first check")
	}
	clock.Advance(due.Sub(clock.Now()))
	completeCheck(t, e, c, clock, "1", 1)
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 4 })
	if startedCredentials(c)[3] != "3" {
		t.Fatal("a due recheck overtook an unstarted first check")
	}
	completeCheck(t, e, c, clock, "2", 1)
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 5 })
	if startedCredentials(c)[4] != "0" {
		t.Fatal("earliest due recheck was not started")
	}
	e.Stop()
	completeCheck(t, e, c, clock, "0", 2)
	completeCheck(t, e, c, clock, "3", 1)
	driveClock(t, clock, func() bool { return idle(e) })
	if _, d, peak := c.counts(); d != 0 || peak > 2 {
		t.Fatalf("disables=%d peak=%d", d, peak)
	}
}

func TestRecheckEqualDueTimesKeepEnqueueOrder(t *testing.T) {
	e, c, clock := makeEngine(t, 3)
	c.automatic, c.prediction = true, "gpt-6-astra"
	enableRecheck(t, e, 1, 1)
	_, _ = e.Start("manual")
	due := *queuedRecheck(t, e, "0").DueAt
	queuedRecheck(t, e, "1")
	queuedRecheck(t, e, "2")
	clock.Advance(due.Sub(clock.Now()))
	eventually(t, func() bool { return idle(e) })
	if got := startedCredentials(c); !slices.Equal(got, []string{"0", "1", "2", "0", "1", "2"}) {
		t.Fatalf("unexpected order: %v", got)
	}
}

func TestRecheckFirstResultEligibility(t *testing.T) {
	for _, tt := range []struct {
		name, status, prediction, message    string
		startError, wantRecheck, wantDisable bool
	}{
		{name: "consistent", status: "completed", prediction: "gpt-6-astra", wantRecheck: true},
		{name: "partial without prediction", status: "partial", wantRecheck: true},
		{name: "no prediction", status: "completed", wantRecheck: true},
		{name: "ordinary failure", status: "failed", message: "timeout", wantRecheck: true},
		{name: "ordinary 503", status: "failed", message: "HTTP 503 overloaded", wantRecheck: true},
		{name: "mismatch", status: "completed", prediction: "other", wantDisable: true},
		{name: "overload", status: "failed", message: `{"code":"server_is_overloaded"}`, wantDisable: true},
		{name: "cancelled", status: "cancelled"},
		{name: "skipped", status: "skipped"},
		{name: "not started", startError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, c, clock := makeEngine(t, 1)
			c.automatic, c.startErr = true, tt.startError
			c.status, c.prediction, c.errorMessage = tt.status, tt.prediction, tt.message
			enableRecheck(t, e, 1, 1)
			_, _ = e.Start("manual")
			if tt.wantRecheck {
				queuedRecheck(t, e, "0")
				e.Stop()
			}
			eventually(t, func() bool { return idle(e) })
			clock.Advance(10 * time.Minute)
			_, disabled, _ := c.counts()
			if (disabled == 1) != tt.wantDisable {
				t.Fatalf("unexpected disable count %d", disabled)
			}
			if !tt.wantRecheck {
				if _, found := latestItem(e, "0", 2); found {
					t.Fatal("ineligible first result scheduled a recheck")
				}
			}
		})
	}
}

func TestRecheckRevalidatesCredential(t *testing.T) {
	for _, tt := range []struct {
		name, reason string
		change       func(*fakeClient)
	}{
		{"disabled", "disabled", func(c *fakeClient) { c.credentials[0].Disabled = true }},
		{"cooldown", "cooldown", func(c *fakeClient) { c.credentials[0].Unavailable = true }},
		{"busy", "busy", func(c *fakeClient) { c.credentials[0].Running = &Progress{Test: "candy"} }},
		{"unsupported", "unsupported_model", func(c *fakeClient) { c.credentials[0].Models = []string{"other"} }},
		{"name changed", "identity_changed", func(c *fakeClient) { c.credentials[0].Name = "replaced.json" }},
		{"index changed", "identity_changed", func(c *fakeClient) { c.credentials[0].AuthIndex = "replaced" }},
		{"email changed", "identity_changed", func(c *fakeClient) { c.credentials[0].Email = "other@example.test" }},
		{"dependency error", "dependency_error", func(c *fakeClient) { c.snapshotErr = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, c, clock := makeEngine(t, 1)
			c.automatic, c.prediction = true, "gpt-6-astra"
			enableRecheck(t, e, 1, 1)
			_, _ = e.Start("manual")
			due := *queuedRecheck(t, e, "0").DueAt
			c.mu.Lock()
			tt.change(c)
			c.mu.Unlock()
			clock.Advance(due.Sub(clock.Now()))
			eventually(t, func() bool { return idle(e) })
			item, found := latestItem(e, "0", 2)
			if !found || item.Reason != tt.reason {
				t.Fatalf("unexpected recheck outcome: %+v", item)
			}
			clock.Advance(10 * time.Minute)
			if a, d, _ := c.counts(); a != 1 || d != 0 {
				t.Fatalf("starts=%d disables=%d", a, d)
			}
		})
	}
}

func TestRecheckIgnoresFirstRecordAndCanDisableOnSecond(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	c.automatic, c.prediction = true, "gpt-6-astra"
	enableRecheck(t, e, 1, 1)
	_, _ = e.Start("manual")
	due := *queuedRecheck(t, e, "0").DueAt
	c.mu.Lock()
	c.prediction = "other"
	c.mu.Unlock()
	clock.Advance(due.Sub(clock.Now()))
	eventually(t, func() bool { return idle(e) })
	first, _ := latestItem(e, "0", 1)
	second, _ := latestItem(e, "0", 2)
	if first.Prediction != "gpt-6-astra" || first.Action != "" || second.Prediction != "other" || second.Action != "disabled" || first.RecordID == second.RecordID {
		t.Fatalf("record isolation failed: first=%+v second=%+v", first, second)
	}
	if a, d, _ := c.counts(); a != 2 || d != 1 {
		t.Fatalf("starts=%d disables=%d", a, d)
	}
}

func TestRecheckStopWakesWaitingQueueWithoutAdvancingTime(t *testing.T) {
	e, c, _ := makeEngine(t, 1)
	c.automatic, c.prediction = true, "gpt-6-astra"
	enableRecheck(t, e, 1, 525600)
	_, _ = e.Start("manual")
	queuedRecheck(t, e, "0")
	e.Stop()
	eventually(t, func() bool { return idle(e) })
	item, _ := latestItem(e, "0", 2)
	if item.Status != "cancelled" {
		t.Fatalf("waiting recheck not cancelled: %+v", item)
	}
	if a, d, _ := c.counts(); a != 1 || d != 0 {
		t.Fatalf("starts=%d disables=%d", a, d)
	}
}

func TestRecheckCloseMarksWaitingChecksInterrupted(t *testing.T) {
	e, c, _ := makeEngine(t, 1)
	c.automatic, c.prediction = true, "gpt-6-astra"
	enableRecheck(t, e, 1, 10)
	_, _ = e.Start("manual")
	queuedRecheck(t, e, "0")
	e.Close()
	item, _ := latestItem(e, "0", 2)
	if item.Status != "interrupted" || !idle(e) {
		t.Fatal("close did not interrupt the waiting recheck")
	}
}

func TestRecheckAmbiguousFirstResultDoesNotQueue(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	c.prediction = "gpt-6-astra"
	enableRecheck(t, e, 1, 1)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	c.mu.Lock()
	c.completeLocked("0", "gpt-6-astra")
	clock.Advance(time.Nanosecond)
	c.completeLocked("0", "gpt-6-astra")
	c.mu.Unlock()
	driveClock(t, clock, func() bool { return idle(e) })
	if _, exists := latestItem(e, "0", 2); exists {
		t.Fatal("ambiguous first-check record queued a recheck")
	}
}

func TestRecheckAllowsUnattributedAndNonModelAnswers(t *testing.T) {
	for _, decision := range []string{"missing attribution", "not_model"} {
		t.Run(decision, func(t *testing.T) {
			e, c, clock := makeEngine(t, 1)
			enableRecheck(t, e, 1, 1)
			_, _ = e.Start("manual")
			eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
			c.mu.Lock()
			c.completeLocked("0", "gpt-6-astra")
			for id, record := range c.records {
				if decision == "missing attribution" {
					record.Attribution = nil
				} else {
					record.Attribution.Decision = decision
				}
				c.records[id] = record
			}
			c.mu.Unlock()
			driveClock(t, clock, func() bool { item, _ := latestItem(e, "0", 1); return item.Status == "completed" })
			queuedRecheck(t, e, "0")
			e.Stop()
			eventually(t, func() bool { return idle(e) })
			if _, d, _ := c.counts(); d != 0 {
				t.Fatal("unattributed answer disabled an account")
			}
		})
	}
}

func TestRecheckPersistenceFailureWakesWaitingQueue(t *testing.T) {
	e, c, _ := makeEngine(t, 1)
	c.automatic, c.prediction = true, "gpt-6-astra"
	s := enableRecheck(t, e, 1, 525600)
	_, _ = e.Start("manual")
	queuedRecheck(t, e, "0")
	path := filepath.Join(t.TempDir(), "not-a-directory", "state.json")
	if err := os.WriteFile(filepath.Dir(path), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.path = path
	e.mu.Unlock()
	if err := e.Settings(s); err == nil {
		t.Fatal("ignored persistence failure")
	}
	eventually(t, func() bool { return idle(e) })
	if a, d, _ := c.counts(); a != 1 || d != 0 {
		t.Fatal("persistence failure allowed another check or disable")
	}
}

func TestRecheckStopPreventsDisableAfterSubmission(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	c.prediction = "gpt-6-astra"
	enableRecheck(t, e, 1, 1)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	completeCheck(t, e, c, clock, "0", 1)
	due := *queuedRecheck(t, e, "0").DueAt
	clock.Advance(due.Sub(clock.Now()))
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 2 })
	c.mu.Lock()
	c.prediction = "other"
	c.mu.Unlock()
	e.Stop()
	completeCheck(t, e, c, clock, "0", 2)
	driveClock(t, clock, func() bool { return idle(e) })
	if _, d, _ := c.counts(); d != 0 {
		t.Fatal("stopped recheck disabled an account")
	}
}

func TestRecheckKeepsRoundSettingsSnapshot(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	c.prediction = "gpt-6-astra"
	s := enableRecheck(t, e, 2, 1)
	_, _ = e.Start("manual")
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 1 })
	s.Model, s.PerCredential, s.Global, s.RecheckDelay, s.RecheckEnabled = "gpt-6.1-sol", 1, 1, 30, false
	if err := e.Settings(s); err != nil {
		t.Fatal(err)
	}
	completeCheck(t, e, c, clock, "0", 1)
	due := *queuedRecheck(t, e, "0").DueAt
	if due.Sub(clock.Now()) > time.Minute {
		t.Fatal("recheck delay changed in flight")
	}
	if err := e.Settings(s); err != nil {
		t.Fatal(err)
	}
	view := e.View(context.Background())
	if view.Next == nil || !view.Next.Equal(due) {
		t.Fatal("saving next-round settings reset the active recheck time")
	}
	clock.Advance(due.Sub(clock.Now()))
	eventually(t, func() bool { a, _, _ := c.counts(); return a == 2 })
	completeCheck(t, e, c, clock, "0", 2)
	eventually(t, func() bool { return idle(e) })
	second, _ := latestItem(e, "0", 2)
	if second.Model != "gpt-6-astra" {
		t.Fatal("recheck model changed in flight")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.per, []int{3, 3}) {
		t.Fatal("recheck per-credential concurrency changed in flight")
	}
}

func TestRecheckRestartDoesNotReplayPendingChecks(t *testing.T) {
	e, c, clock := makeEngine(t, 1)
	c.automatic, c.prediction = true, "gpt-6-astra"
	enableRecheck(t, e, 1, 1)
	_, _ = e.Start("manual")
	queuedRecheck(t, e, "0")
	raw, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	client := newClient(clock, 1)
	restarted, err := NewEngine(client, path, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	clock.Advance(10 * time.Minute)
	view := restarted.View(context.Background())
	item, _ := latestItem(restarted, "0", 2)
	if !view.Settings.RecheckEnabled || view.Running || view.Next != nil || view.Rounds[0].Status != "interrupted" || item.Status != "interrupted" {
		t.Fatal("restart replayed or lost the interrupted recheck")
	}
	if a, d, _ := client.counts(); a != 0 || d != 0 {
		t.Fatal("restart acted on old records")
	}
}

func TestRecheckSettingsValidationAndCompatibility(t *testing.T) {
	defaults := DefaultSettings()
	if defaults.RecheckEnabled || defaults.RecheckDelay != 10 {
		t.Fatal("unexpected defaults")
	}
	for _, delay := range []int{0, -1, 525601} {
		s := defaults
		s.RecheckDelay = delay
		if s.Validate() == nil {
			t.Fatalf("accepted invalid delay %d", delay)
		}
	}
	s := defaults
	s.Model, s.Scheduled, s.RecheckEnabled = "gpt-6-astra", true, true
	if s.Validate() == nil {
		t.Fatal("accepted simultaneous timed and recheck modes")
	}
	e, c, clock := makeEngine(t, 1)
	if result := serve(e, "", managementRequest{Method: "PUT", Path: APIBase + "/settings", Body: marshal(s)}); result.StatusCode != 400 {
		t.Fatal("management API accepted conflicting modes")
	}
	legacy := map[string]any{}
	_ = json.Unmarshal(marshal(defaults), &legacy)
	delete(legacy, "recheck_delay_minutes")
	delete(legacy, "recheck_enabled")
	legacy["model"] = "gpt-6-astra"
	result := serve(e, "", managementRequest{Method: "PUT", Path: APIBase + "/settings", Body: marshal(legacy)})
	if result.StatusCode != 200 || e.View(context.Background()).Settings.RecheckDelay != 10 {
		t.Fatal("old settings clients no longer work")
	}
	legacy["recheck_delay_minutes"] = 0
	if result := serve(e, "", managementRequest{Method: "PUT", Path: APIBase + "/settings", Body: marshal(legacy)}); result.StatusCode != 400 {
		t.Fatal("explicit zero delay was treated as a missing field")
	}
	legacy["recheck_delay_minutes"] = 1.5
	if result := serve(e, "", managementRequest{Method: "PUT", Path: APIBase + "/settings", Body: marshal(legacy)}); result.StatusCode != 400 {
		t.Fatal("accepted a fractional delay")
	}
	s = defaults
	s.Model, s.Scheduled = "gpt-6-astra", true
	for range 2 {
		if err := e.Settings(s); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(time.Hour)
	if a, _, _ := c.counts(); a != 0 || !idle(e) {
		t.Fatal("selecting or saving a mode started detection")
	}
}

func TestRecheckLoadsLegacyState(t *testing.T) {
	clock := newClock()
	c := newClient(clock, 1)
	settings := map[string]any{}
	_ = json.Unmarshal(marshal(DefaultSettings()), &settings)
	delete(settings, "recheck_enabled")
	delete(settings, "recheck_delay_minutes")
	settings["model"] = "gpt-6-astra"
	path := filepath.Join(t.TempDir(), "state.json")
	old := map[string]any{
		"version": 1, "settings": settings,
		"rounds": []any{map[string]any{
			"id": "legacy", "status": "completed", "started": clock.Now(), "finished": clock.Now(), "settings": settings,
			"items": []any{map[string]any{"credential_id": "0", "name": "legacy.json", "status": "completed"}},
		}},
	}
	if err := os.WriteFile(path, marshal(old), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(c, path, Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	view := e.View(context.Background())
	if view.Settings.RecheckEnabled || view.Settings.RecheckDelay != 10 || len(view.Rounds) != 1 || view.Rounds[0].Items[0].CheckIndex != 1 || view.Rounds[0].Settings.RecheckDelay != 10 {
		t.Fatal("legacy state did not receive recheck defaults")
	}
}
