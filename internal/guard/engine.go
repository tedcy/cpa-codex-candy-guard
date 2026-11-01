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
	wake         chan struct{}
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
		for i := range e.data.Rounds {
			for j := range e.data.Rounds[i].Items {
				if e.data.Rounds[i].Items[j].CheckIndex == 0 {
					e.data.Rounds[i].Items[j].CheckIndex = 1
				}
			}
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
		e.signalWake(e.wake)
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
	hadTimer := e.timer != nil
	e.data.Settings = s
	if err := e.persistLocked(); err != nil {
		e.mu.Unlock()
		return err
	}
	if e.active != "" {
		e.updateNextLocked(e.active)
	} else if !s.Scheduled {
		e.clearTimerLocked()
	} else if hadTimer {
		e.scheduleLocked()
	}
	e.mu.Unlock()
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
		item := Item{CredentialID: cred.ID, Name: cred.Name, CheckIndex: 1, Model: s.Model, Status: "queued"}
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
	wake := make(chan struct{}, 1)
	e.wake = wake
	if err = e.persistLocked(); err != nil {
		e.active = ""
		e.wake = nil
		return "", err
	}
	e.wg.Add(1)
	go e.runRound(id, s, queue, wake)
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
func itemCheckIndex(item Item) int {
	if item.CheckIndex <= 0 {
		return 1
	}
	return item.CheckIndex
}

func (e *Engine) updateNextLocked(id string) {
	if e.active != id {
		return
	}
	var next *time.Time
	if r := e.currentLocked(id); r != nil {
		for _, item := range r.Items {
			if itemCheckIndex(item) != 2 || item.Status != "queued" || item.DueAt == nil {
				continue
			}
			if next == nil || item.DueAt.Before(*next) {
				at := *item.DueAt
				next = &at
			}
		}
	}
	e.next = next
}

func (e *Engine) setItem(id, credentialID string, checkIndex int, update func(*Item)) error {
	if checkIndex <= 0 {
		checkIndex = 1
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.currentLocked(id)
	if r == nil {
		return errors.New("巡检轮次已不存在")
	}
	for i := range r.Items {
		if r.Items[i].CredentialID == credentialID && itemCheckIndex(r.Items[i]) == checkIndex {
			update(&r.Items[i])
			e.updateNextLocked(id)
			return e.persistLocked()
		}
	}
	return errors.New("巡检账号已不存在")
}

func (e *Engine) addRecheck(id string, credential Credential, model string, due time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.currentLocked(id)
	if r == nil || e.closed || e.stopping || e.active != id || e.fault != "" {
		return false
	}
	for _, item := range r.Items {
		if item.CredentialID == credential.ID && itemCheckIndex(item) == 2 {
			return false
		}
	}
	dueCopy := due
	r.Items = append(r.Items, Item{CredentialID: credential.ID, Name: credential.Name, CheckIndex: 2, DueAt: &dueCopy, Model: model, Status: "queued"})
	e.updateNextLocked(id)
	return e.persistLocked() == nil
}

func (e *Engine) signalWake(wake chan struct{}) {
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}
func (e *Engine) halted(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed || e.stopping || e.active != id || e.fault != ""
}
func (e *Engine) pause() {
	e.mu.Lock()
	e.fault = "检测依赖异常或结果格式变化，已暂停自动关闭"
	e.stopping = true
	e.clearTimerLocked()
	wake := e.wake
	e.mu.Unlock()
	e.signalWake(wake)
}

type checkTask struct {
	credential Credential
	checkIndex int
	due        time.Time
}

type checkResult struct {
	task            checkTask
	scheduleRecheck bool
	finished        time.Time
}

func (e *Engine) cancelPending(id string, tasks []checkTask) {
	status := "cancelled"
	if e.ctx.Err() != nil {
		status = "interrupted"
	}
	for _, task := range tasks {
		_ = e.setItem(id, task.credential.ID, task.checkIndex, func(item *Item) {
			if item.Status == "queued" {
				item.Status = status
				item.Reason = "stopped_before_start"
			}
		})
	}
}

func (e *Engine) runRound(id string, s Settings, queue []Credential, wake chan struct{}) {
	defer e.wg.Done()
	first := make([]checkTask, 0, len(queue))
	for _, cred := range queue {
		first = append(first, checkTask{credential: cred, checkIndex: 1})
	}
	var rechecks []checkTask
	done := make(chan checkResult, max(1, s.Global))
	running := 0

	for {
		stopped := e.halted(id)
		if stopped {
			e.cancelPending(id, first)
			e.cancelPending(id, rechecks)
			first = nil
			rechecks = nil
		}

		launched := false
		for running < s.Global && !stopped {
			var task checkTask
			haveTask := false
			if len(first) > 0 {
				task, first = first[0], first[1:]
				haveTask = true
			} else if len(rechecks) > 0 && !rechecks[0].due.After(e.clock.Now()) {
				task, rechecks = rechecks[0], rechecks[1:]
				haveTask = true
			}
			if !haveTask {
				break
			}
			running++
			launched = true
			go func(task checkTask) {
				recheck := e.runCredential(id, s, task.credential, task.checkIndex)
				done <- checkResult{task: task, scheduleRecheck: recheck, finished: e.clock.Now()}
			}(task)
		}
		if running == 0 && len(first) == 0 && len(rechecks) == 0 {
			break
		}
		if launched {
			continue
		}

		var timer Timer
		var timerCh <-chan struct{}
		if !stopped && running < s.Global && len(first) == 0 && len(rechecks) > 0 {
			delay := rechecks[0].due.Sub(e.clock.Now())
			if delay <= 0 {
				continue
			}
			ch := make(chan struct{}, 1)
			timer = e.clock.AfterFunc(delay, func() {
				select {
				case ch <- struct{}{}:
				default:
				}
			})
			if !rechecks[0].due.After(e.clock.Now()) {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
			timerCh = ch
		}
		ctxDone := e.ctx.Done()
		if stopped {
			ctxDone = nil
		}

		select {
		case result := <-done:
			running--
			if result.scheduleRecheck && !e.halted(id) {
				due := result.finished.Add(time.Duration(s.RecheckDelay) * time.Minute)
				if e.addRecheck(id, result.task.credential, s.Model, due) {
					rechecks = append(rechecks, checkTask{credential: result.task.credential, checkIndex: 2, due: due})
					sort.SliceStable(rechecks, func(i, j int) bool { return rechecks[i].due.Before(rechecks[j].due) })
				}
			}
		case <-wake:
		case <-ctxDone:
		case <-timerCh:
		}
		if timer != nil {
			timer.Stop()
		}
	}

	e.mu.Lock()
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
	e.next = nil
	e.active = ""
	e.stopping = false
	if e.wake == wake {
		e.wake = nil
	}
	_ = e.persistLocked()
	e.scheduleLocked()
	e.mu.Unlock()
}

func findCredential(snapshot Snapshot, id string) (Credential, bool) {
	for _, cred := range snapshot.Credentials {
		if cred.ID == id {
			return cred, true
		}
	}
	return Credential{}, false
}

func (e *Engine) runCredential(roundID string, s Settings, original Credential, checkIndex int) bool {
	finish := func(status, reason string) {
		_ = e.setItem(roundID, original.ID, checkIndex, func(item *Item) { item.Status = status; item.Reason = reason })
	}
	if e.halted(roundID) {
		finish("cancelled", "stopped_before_start")
		return false
	}
	snapshot, err := e.client.Snapshot(e.ctx)
	if err != nil {
		e.pause()
		finish("error", "dependency_error")
		return false
	}
	cred, ok := findCredential(snapshot, original.ID)
	if !ok || cred.Name != original.Name || cred.AuthIndex != original.AuthIndex || cred.Email != original.Email {
		finish("skipped", "identity_changed")
		return false
	}
	if cred.Disabled {
		finish("skipped", "disabled")
		return false
	}
	if cred.Unavailable {
		finish("skipped", "cooldown")
		return false
	}
	if cred.Running != nil {
		finish("skipped", "busy")
		return false
	}
	models, err := e.client.Models(e.ctx, cred)
	if err != nil {
		e.pause()
		finish("error", "model_catalog_error")
		return false
	}
	if !slices.Contains(models, s.Model) {
		finish("skipped", "unsupported_model")
		return false
	}
	known := map[string]bool{}
	for _, r := range cred.Records {
		known[r.ID] = true
	}
	submitted := e.clock.Now()
	e.actionMu.Lock()
	if e.halted(roundID) {
		e.actionMu.Unlock()
		finish("cancelled", "stopped_before_start")
		return false
	}
	if err = e.setItem(roundID, cred.ID, checkIndex, func(item *Item) { item.Status = "running" }); err != nil {
		e.actionMu.Unlock()
		return false
	}
	ack, err := e.client.Start(e.ctx, cred, s)
	e.actionMu.Unlock()
	if err != nil {
		finish("error", "start_failed")
		return false
	}
	if ack.Started != 1 {
		if ack.Busy > 0 {
			finish("skipped", "busy")
		} else if ack.Skipped > 0 {
			finish("skipped", "unsupported_model")
		} else {
			finish("error", "start_not_confirmed")
		}
		return false
	}
	for {
		if e.ctx.Err() != nil {
			finish("interrupted", "")
			return false
		}
		if e.clock.Now().Sub(submitted) > e.recordWait {
			finish("unknown", "record_timeout")
			return false
		}
		snapshot, err = e.client.Snapshot(e.ctx)
		if err != nil {
			e.pause()
			finish("error", "dependency_error")
			return false
		}
		fresh, exists := findCredential(snapshot, cred.ID)
		if !exists || fresh.AuthIndex != cred.AuthIndex || fresh.Email != cred.Email {
			finish("unknown", "identity_changed")
			return false
		}
		candidates := []Record{}
		for _, r := range fresh.Records {
			if !known[r.ID] && r.Model == s.Model && !r.Time.Before(submitted) {
				candidates = append(candidates, r)
			}
		}
		if len(candidates) > 1 {
			finish("unknown", "ambiguous_records")
			return false
		}
		if len(candidates) == 1 {
			summary := candidates[0]
			if summary.ID == "" || summary.Time.IsZero() {
				e.pause()
				finish("error", "invalid_record")
				return false
			}
			record, err := e.client.Detail(e.ctx, cred.ID, summary.ID)
			if err != nil {
				e.pause()
				finish("error", "record_read_error")
				return false
			}
			if record.ID != summary.ID || !record.Time.Equal(summary.Time) || record.Model != s.Model {
				finish("unknown", "ambiguous_records")
				return false
			}
			if !slices.Contains([]string{"completed", "partial", "failed", "cancelled", "skipped"}, record.Status) {
				e.pause()
				finish("error", "invalid_record_status")
				return false
			}
			if record.Attribution != nil && !slices.Contains([]string{"", "partial", "not_model"}, record.Attribution.Decision) {
				e.pause()
				finish("error", "invalid_record")
				return false
			}
			verdict := Judge(record)
			_ = e.setItem(roundID, cred.ID, checkIndex, func(item *Item) {
				item.RecordID = record.ID
				item.Status = record.Status
				if record.Attribution != nil {
					item.Prediction = record.Attribution.Prediction
				}
			})
			if !verdict.Disable {
				return checkIndex == 1 && s.RecheckEnabled && slices.Contains([]string{"completed", "partial", "failed"}, record.Status) && !e.halted(roundID)
			}
			// Stop and disable are serialized; once Stop returns, no new disable can begin.
			e.actionMu.Lock()
			if e.halted(roundID) {
				e.actionMu.Unlock()
				finish("cancelled", "stopped_before_action")
				return false
			}
			err = e.setItem(roundID, cred.ID, checkIndex, func(item *Item) { item.Reason = verdict.Reason; item.Action = "disable_pending" })
			if err == nil {
				err = e.client.Disable(e.ctx, cred)
			}
			e.actionMu.Unlock()
			_ = e.setItem(roundID, cred.ID, checkIndex, func(item *Item) {
				if err != nil {
					item.Action = "disable_failed"
				} else {
					item.Action = "disabled"
				}
			})
			return false
		}
		tick := make(chan struct{}, 1)
		timer := e.clock.AfterFunc(e.poll, func() { tick <- struct{}{} })
		select {
		case <-e.ctx.Done():
			timer.Stop()
			finish("interrupted", "")
			return false
		case <-tick:
		}
	}
}

func (e *Engine) Stop() {
	e.actionMu.Lock()
	e.mu.Lock()
	e.stopping = e.active != "" || e.starting
	_ = e.persistLocked()
	wake := e.wake
	e.mu.Unlock()
	e.actionMu.Unlock()
	e.signalWake(wake)
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
