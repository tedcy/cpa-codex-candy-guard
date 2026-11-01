package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Clock        Clock
	PollInterval time.Duration
	RecordWait   time.Duration
}
type Engine struct {
	mu           sync.Mutex
	actionMu     sync.Mutex
	client       Client
	path         string
	clock        Clock
	poll         time.Duration
	recordWait   time.Duration
	data         persisted
	active       string
	starting     bool
	stopping     bool
	closed       bool
	fault        string
	storageFault string
	next         *time.Time
	timer        Timer
	generation   uint64
	sequence     uint64
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

type View struct {
	Ready       bool         `json:"ready"`
	Version     string       `json:"version"`
	Settings    Settings     `json:"settings"`
	Rounds      []Round      `json:"rounds"`
	Running     bool         `json:"running"`
	Stopping    bool         `json:"stopping"`
	Next        *time.Time   `json:"next_run,omitempty"`
	Error       string       `json:"error,omitempty"`
	Credentials []Credential `json:"credentials"`
	Models      []string     `json:"models"`
	Dependency  string       `json:"dependency_version"`
}

func NewEngine(client Client, path string, options Options) (*Engine, error) {
	if client == nil {
		return nil, errors.New("未配置 CPA 客户端")
	}
	if options.Clock == nil {
		options.Clock = realClock{}
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 2 * time.Second
	}
	if options.RecordWait <= 0 {
		options.RecordWait = 30 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{client: client, path: path, clock: options.Clock, poll: options.PollInterval, recordWait: options.RecordWait, ctx: ctx, cancel: cancel, data: persisted{Version: 1, Settings: DefaultSettings(), Rounds: []Round{}}}
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &e.data) != nil || e.data.Version != 1 {
			cancel()
			return nil, errors.New("巡检状态文件不兼容，未覆盖原文件")
		}
		if err = e.data.Settings.Validate(); err != nil {
			cancel()
			return nil, errors.New("巡检状态文件中的设置无效")
		}
		for i := range e.data.Rounds {
			r := &e.data.Rounds[i]
			if r.Finished == nil {
				now := e.clock.Now()
				r.Finished = &now
				r.Status = "interrupted"
				for j := range r.Items {
					if r.Items[j].Status == "queued" || r.Items[j].Status == "running" {
						r.Items[j].Status = "interrupted"
					}
				}
			}
		}
		if err = e.persistLocked(); err != nil {
			cancel()
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		cancel()
		return nil, errors.New("无法读取巡检状态文件")
	}
	e.scheduleLocked()
	return e, nil
}

func (e *Engine) persistLocked() error {
	if e.storageFault != "" {
		return errors.New(e.storageFault)
	}
	if len(e.data.Rounds) > 20 {
		e.data.Rounds = e.data.Rounds[len(e.data.Rounds)-20:]
	}
	raw, err := json.MarshalIndent(e.data, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(e.path), 0o700)
	}
	if err == nil {
		err = os.WriteFile(e.path+".tmp", raw, 0o600)
	}
	if err == nil {
		err = os.Rename(e.path+".tmp", e.path)
	}
	if err != nil {
		e.storageFault = "巡检状态保存失败，已暂停任务和自动关闭"
		e.fault = e.storageFault
		e.stopping = true
		e.clearTimerLocked()
		return errors.New(e.storageFault)
	}
	return nil
}

func (e *Engine) clearTimerLocked() {
	e.generation++
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.next = nil
}
func (e *Engine) scheduleLocked() {
	e.clearTimerLocked()
	if !e.data.Settings.Scheduled || e.closed || e.active != "" || e.starting || e.fault != "" {
		return
	}
	next := e.clock.Now().Add(time.Duration(e.data.Settings.IntervalMinutes) * time.Minute)
	e.next = &next
	generation := e.generation
	e.timer = e.clock.AfterFunc(time.Duration(e.data.Settings.IntervalMinutes)*time.Minute, func() {
		e.mu.Lock()
		if e.closed || e.generation != generation {
			e.mu.Unlock()
			return
		}
		e.next = nil
		e.mu.Unlock()
		_, _ = e.Start("timer")
	})
}

func (e *Engine) Settings(s Settings) error {
	s.Model = strings.TrimSpace(s.Model)
	if err := s.Validate(); err != nil {
		return err
	}
	s.CredentialIDs = slices.Clone(s.CredentialIDs)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return errors.New("插件正在停止")
	}
	wasScheduled := e.data.Settings.Scheduled
	e.data.Settings = s
	if err := e.persistLocked(); err != nil {
		e.mu.Unlock()
		return err
	}
	if !s.Scheduled {
		e.clearTimerLocked()
	} else if wasScheduled {
		e.scheduleLocked()
	}
	start := s.Scheduled && !wasScheduled && e.active == "" && !e.starting
	e.mu.Unlock()
	if start {
		_, err := e.Start("timer")
		return err
	}
	return nil
}

func (e *Engine) Start(trigger string) (string, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return "", errors.New("插件正在停止")
	}
	if e.storageFault != "" {
		e.mu.Unlock()
		return "", errors.New(e.storageFault)
	}
	if e.active != "" || e.starting {
		e.mu.Unlock()
		return "", ErrBusy
	}
	s := e.data.Settings
	s.CredentialIDs = slices.Clone(s.CredentialIDs)
	if s.Model == "" {
		e.mu.Unlock()
		return "", errors.New("请先选择模型")
	}
	if s.Scope == "selected" && len(s.CredentialIDs) == 0 {
		e.mu.Unlock()
		return "", errors.New("请至少选择一个账号")
	}
	e.starting = true
	e.stopping = false
	e.clearTimerLocked()
	e.mu.Unlock()
	snapshot, err := e.client.Snapshot(e.ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.starting = false
	if err != nil {
		e.fault = "检测依赖不可用，已暂停巡检"
		return "", err
	}
	if e.closed {
		return "", errors.New("插件正在停止")
	}
	if e.stopping {
		e.stopping = false
		e.scheduleLocked()
		return "", errors.New("巡检启动已取消")
	}
	e.fault = ""
	e.stopping = false
	now := e.clock.Now()
	e.sequence++
	id := fmt.Sprintf("%d-%d", now.UnixNano(), e.sequence)
	round := Round{ID: id, Trigger: trigger, Started: now, Status: "running", Settings: s, Items: []Item{}}
	var queue []Credential
	for _, cred := range snapshot.Credentials {
		if s.Scope == "selected" && !slices.Contains(s.CredentialIDs, cred.ID) {
			continue
		}
		item := Item{CredentialID: cred.ID, Name: cred.Name, Model: s.Model, Status: "queued"}
		switch {
		case cred.Disabled:
			item.Status = "skipped"
			item.Reason = "disabled"
		case cred.Unavailable:
			item.Status = "skipped"
			item.Reason = "cooldown"
		case cred.Running != nil:
			item.Status = "skipped"
			item.Reason = "busy"
		default:
			queue = append(queue, cred)
		}
		round.Items = append(round.Items, item)
	}
	e.data.Rounds = append(e.data.Rounds, round)
	e.active = id
	if err = e.persistLocked(); err != nil {
		e.active = ""
		return "", err
	}
	e.wg.Add(1)
	go e.runRound(id, s, queue)
	return id, nil
}

func (e *Engine) currentLocked(id string) *Round {
	for i := range e.data.Rounds {
		if e.data.Rounds[i].ID == id {
			return &e.data.Rounds[i]
		}
	}
	return nil
}
func (e *Engine) setItem(id, credentialID string, update func(*Item)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.currentLocked(id)
	if r == nil {
		return errors.New("巡检轮次已不存在")
	}
	for i := range r.Items {
		if r.Items[i].CredentialID == credentialID {
			update(&r.Items[i])
			return e.persistLocked()
		}
	}
	return errors.New("巡检账号已不存在")
}
func (e *Engine) halted(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed || e.stopping || e.active != id || e.fault != ""
}
func (e *Engine) pause() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fault = "检测依赖异常或结果格式变化，已暂停自动关闭"
	e.stopping = true
	e.clearTimerLocked()
}

func (e *Engine) runRound(id string, s Settings, queue []Credential) {
	defer e.wg.Done()
	jobs := make(chan Credential, len(queue))
	for _, cred := range queue {
		jobs <- cred
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(s.Global, len(queue)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for cred := range jobs {
				if e.halted(id) {
					_ = e.setItem(id, cred.ID, func(item *Item) { item.Status = "cancelled" })
					continue
				}
				e.runCredential(id, s, cred)
			}
		}()
	}
	workers.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	if r := e.currentLocked(id); r != nil {
		now := e.clock.Now()
		r.Finished = &now
		r.Status = "completed"
		if e.closed {
			r.Status = "interrupted"
		} else if e.stopping {
			r.Status = "stopped"
		}
		if e.fault != "" {
			r.Status = "paused"
		}
	}
	e.active = ""
	e.stopping = false
	_ = e.persistLocked()
	e.scheduleLocked()
}

func findCredential(snapshot Snapshot, id string) (Credential, bool) {
	for _, cred := range snapshot.Credentials {
		if cred.ID == id {
			return cred, true
		}
	}
	return Credential{}, false
}

func (e *Engine) runCredential(roundID string, s Settings, original Credential) {
	finish := func(status, reason string) {
		_ = e.setItem(roundID, original.ID, func(item *Item) { item.Status = status; item.Reason = reason })
	}
	snapshot, err := e.client.Snapshot(e.ctx)
	if err != nil {
		e.pause()
		finish("error", "dependency_error")
		return
	}
	cred, ok := findCredential(snapshot, original.ID)
	if !ok || cred.Name != original.Name || cred.AuthIndex != original.AuthIndex || cred.Email != original.Email {
		finish("skipped", "identity_changed")
		return
	}
	if cred.Disabled {
		finish("skipped", "disabled")
		return
	}
	if cred.Unavailable {
		finish("skipped", "cooldown")
		return
	}
	if cred.Running != nil {
		finish("skipped", "busy")
		return
	}
	models, err := e.client.Models(e.ctx, cred)
	if err != nil {
		e.pause()
		finish("error", "model_catalog_error")
		return
	}
	if !slices.Contains(models, s.Model) {
		finish("skipped", "unsupported_model")
		return
	}
	known := map[string]bool{}
	for _, r := range cred.Records {
		known[r.ID] = true
	}
	submitted := e.clock.Now()
	e.actionMu.Lock()
	if e.halted(roundID) {
		e.actionMu.Unlock()
		finish("cancelled", "")
		return
	}
	if err = e.setItem(roundID, cred.ID, func(item *Item) { item.Status = "running" }); err != nil {
		e.actionMu.Unlock()
		return
	}
	ack, err := e.client.Start(e.ctx, cred, s)
	e.actionMu.Unlock()
	if err != nil {
		finish("error", "start_failed")
		return
	}
	if ack.Started != 1 {
		if ack.Busy > 0 {
			finish("skipped", "busy")
		} else if ack.Skipped > 0 {
			finish("skipped", "unsupported_model")
		} else {
			finish("error", "start_not_confirmed")
		}
		return
	}
	for {
		if e.ctx.Err() != nil {
			finish("interrupted", "")
			return
		}
		if e.clock.Now().Sub(submitted) > e.recordWait {
			finish("unknown", "record_timeout")
			return
		}
		snapshot, err = e.client.Snapshot(e.ctx)
		if err != nil {
			e.pause()
			finish("error", "dependency_error")
			return
		}
		fresh, exists := findCredential(snapshot, cred.ID)
		if !exists || fresh.AuthIndex != cred.AuthIndex || fresh.Email != cred.Email {
			finish("unknown", "identity_changed")
			return
		}
		candidates := []Record{}
		for _, r := range fresh.Records {
			if !known[r.ID] && r.Model == s.Model && !r.Time.Before(submitted) {
				candidates = append(candidates, r)
			}
		}
		if len(candidates) > 1 {
			finish("unknown", "ambiguous_records")
			return
		}
		if len(candidates) == 1 {
			summary := candidates[0]
			if summary.ID == "" || summary.Time.IsZero() {
				e.pause()
				finish("error", "invalid_record")
				return
			}
			record, err := e.client.Detail(e.ctx, cred.ID, summary.ID)
			if err != nil {
				e.pause()
				finish("error", "record_read_error")
				return
			}
			if record.ID != summary.ID || !record.Time.Equal(summary.Time) || record.Model != s.Model {
				e.pause()
				finish("unknown", "ambiguous_records")
				return
			}
			if !slices.Contains([]string{"completed", "partial", "failed", "cancelled", "skipped"}, record.Status) {
				e.pause()
				finish("error", "invalid_record_status")
				return
			}
			if record.Attribution != nil && !slices.Contains([]string{"", "partial", "not_model"}, record.Attribution.Decision) {
				e.pause()
				finish("error", "invalid_record")
				return
			}
			verdict := Judge(record)
			_ = e.setItem(roundID, cred.ID, func(item *Item) {
				item.RecordID = record.ID
				item.Status = record.Status
				if record.Attribution != nil {
					item.Prediction = record.Attribution.Prediction
				}
			})
			if !verdict.Disable {
				return
			}
			// Stop and disable are serialized; once Stop returns, no new disable can begin.
			e.actionMu.Lock()
			if e.halted(roundID) {
				e.actionMu.Unlock()
				finish("cancelled", "stopped_before_action")
				return
			}
			err = e.setItem(roundID, cred.ID, func(item *Item) { item.Reason = verdict.Reason; item.Action = "disable_pending" })
			if err == nil {
				err = e.client.Disable(e.ctx, cred)
			}
			e.actionMu.Unlock()
			_ = e.setItem(roundID, cred.ID, func(item *Item) {
				if err != nil {
					item.Action = "disable_failed"
				} else {
					item.Action = "disabled"
				}
			})
			return
		}
		tick := make(chan struct{}, 1)
		timer := e.clock.AfterFunc(e.poll, func() { tick <- struct{}{} })
		select {
		case <-e.ctx.Done():
			timer.Stop()
			finish("interrupted", "")
			return
		case <-tick:
		}
	}
}

func (e *Engine) Stop() {
	e.actionMu.Lock()
	defer e.actionMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopping = e.active != "" || e.starting
	_ = e.persistLocked()
}

func (e *Engine) View(ctx context.Context) View {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	e.mu.Lock()
	raw, _ := json.Marshal(e.data)
	next := e.next
	if next != nil {
		copy := *next
		next = &copy
	}
	view := View{Version: Version, Running: e.active != "" || e.starting, Stopping: e.stopping, Next: next, Error: e.fault, Credentials: []Credential{}, Models: []string{}, Dependency: CandyVersion}
	e.mu.Unlock()
	var snapshotData persisted
	_ = json.Unmarshal(raw, &snapshotData)
	view.Settings = snapshotData.Settings
	view.Rounds = snapshotData.Rounds
	snapshot, err := e.client.Snapshot(ctx)
	if err != nil {
		view.Error = "检测依赖不可用：" + err.Error()
		return view
	}
	set := map[string]bool{}
	view.Credentials = snapshot.Credentials
	for i, cred := range snapshot.Credentials {
		models, err := e.client.Models(ctx, cred)
		if err != nil {
			view.Error = "无法读取账号模型目录"
			return view
		}
		view.Credentials[i].Models = models
		if !cred.Disabled {
			for _, m := range models {
				set[m] = true
			}
		}
	}
	for m := range set {
		view.Models = append(view.Models, m)
	}
	sort.Strings(view.Models)
	e.mu.Lock()
	view.Ready = e.storageFault == "" && !e.closed
	e.mu.Unlock()
	return view
}

func (e *Engine) Close() {
	e.actionMu.Lock()
	e.mu.Lock()
	e.closed = true
	e.stopping = true
	e.clearTimerLocked()
	e.cancel()
	e.mu.Unlock()
	e.actionMu.Unlock()
	e.wg.Wait()
}
