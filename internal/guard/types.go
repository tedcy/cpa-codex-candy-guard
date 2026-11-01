package guard

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	PluginID     = "cpa-codex-candy-guard"
	Version      = "0.1.0"
	CandyID      = "cpa-codex-candy-eval"
	CandyVersion = "0.4.4"
	APIBase      = "/v0/management/plugins/" + PluginID
	UIPath       = "/v0/resource/plugins/" + PluginID + "/ui"
)

var ErrBusy = errors.New("已有巡检正在运行")

type DependencyError struct{ Message string }

func (e *DependencyError) Error() string { return e.Message }

type Settings struct {
	Model           string   `json:"model"`
	PerCredential   int      `json:"per_credential_concurrency"`
	Global          int      `json:"credential_concurrency"`
	IntervalMinutes int      `json:"interval_minutes"`
	Scheduled       bool     `json:"schedule_enabled"`
	Scope           string   `json:"scope"`
	CredentialIDs   []string `json:"credential_ids"`
}

func DefaultSettings() Settings {
	return Settings{PerCredential: 3, Global: 2, IntervalMinutes: 10, Scope: "all_enabled", CredentialIDs: []string{}}
}

func (s Settings) Validate() error {
	if len(s.Model) > 200 || strings.ContainsAny(s.Model, "\r\n\x00()") {
		return errors.New("模型名称无效，请使用不带推理强度后缀的模型")
	}
	if s.PerCredential < 1 || s.PerCredential > 3 {
		return errors.New("每凭证并发须为 1–3")
	}
	if s.Global < 1 || s.Global > 256 {
		return errors.New("同时检测账号数须为 1–256")
	}
	if s.IntervalMinutes < 1 || s.IntervalMinutes > 525600 {
		return errors.New("定时间隔须为 1–525600 分钟")
	}
	if s.Scope != "all_enabled" && s.Scope != "selected" {
		return errors.New("账号范围无效")
	}
	if len(s.CredentialIDs) > 10000 {
		return errors.New("选择的账号过多")
	}
	if s.Scheduled && strings.TrimSpace(s.Model) == "" {
		return errors.New("启用定时前请选择模型")
	}
	if s.Scheduled && s.Scope == "selected" && len(s.CredentialIDs) == 0 {
		return errors.New("请至少选择一个账号")
	}
	return nil
}

type Sample struct {
	Error string `json:"error"`
}
type Attribution struct {
	Decision    string   `json:"decision"`
	Prediction  string   `json:"prediction"`
	Probability *float64 `json:"probability"`
}
type Record struct {
	ID          string       `json:"id"`
	Time        time.Time    `json:"time"`
	Model       string       `json:"model"`
	Status      string       `json:"status"`
	Error       string       `json:"error"`
	Attribution *Attribution `json:"attribution"`
	Samples     []Sample     `json:"samples"`
}
type Progress struct {
	Test  string `json:"test"`
	Model string `json:"model"`
}
type Credential struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	AuthIndex   string    `json:"-"`
	Email       string    `json:"-"`
	Disabled    bool      `json:"disabled"`
	Unavailable bool      `json:"unavailable"`
	Running     *Progress `json:"running,omitempty"`
	Records     []Record  `json:"-"`
	Models      []string  `json:"models"`
}
type Snapshot struct{ Credentials []Credential }
type StartAck struct {
	Started int `json:"started"`
	Busy    int `json:"busy"`
	Skipped int `json:"skipped"`
}
type Client interface {
	Snapshot(context.Context) (Snapshot, error)
	Models(context.Context, Credential) ([]string, error)
	Start(context.Context, Credential, Settings) (StartAck, error)
	Detail(context.Context, string, string) (Record, error)
	Disable(context.Context, Credential) error
}

type Item struct {
	CredentialID string `json:"credential_id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Model        string `json:"model"`
	Prediction   string `json:"prediction,omitempty"`
	RecordID     string `json:"record_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Action       string `json:"action,omitempty"`
}
type Round struct {
	ID       string     `json:"id"`
	Trigger  string     `json:"trigger"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	Status   string     `json:"status"`
	Settings Settings   `json:"settings"`
	Items    []Item     `json:"items"`
}
type persisted struct {
	Version  int      `json:"version"`
	Settings Settings `json:"settings"`
	Rounds   []Round  `json:"rounds"`
}

type Timer interface{ Stop() bool }
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) Timer
}
type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now().UTC() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

type Verdict struct {
	Disable bool
	Reason  string
}

// This normalization matches Candy Eval v0.4.4, including its two enrolled aliases.
func modelID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = value[strings.LastIndex(value, "/")+1:]
	switch value {
	case "gpt-6.1-sol":
		return "gpt-6-astra"
	case "deepseek-flash":
		return "deepseek-v4.1-flash-1017"
	}
	return value
}

func Judge(r Record) Verdict {
	if r.Status == "cancelled" || r.Status == "skipped" {
		return Verdict{}
	}
	if r.Status != "completed" && r.Status != "partial" && r.Status != "failed" {
		return Verdict{}
	}
	if overload(r.Error) {
		return Verdict{true, "final_overload"}
	}
	for _, s := range r.Samples {
		if overload(s.Error) {
			return Verdict{true, "final_overload"}
		}
	}
	if (r.Status == "completed" || r.Status == "partial") && r.Attribution != nil && r.Attribution.Decision != "not_model" && strings.TrimSpace(r.Attribution.Prediction) != "" && modelID(r.Model) != modelID(r.Attribution.Prediction) {
		return Verdict{true, "model_mismatch"}
	}
	return Verdict{}
}
