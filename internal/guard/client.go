package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const candyBase = "/v0/management/plugins/" + CandyID

type cachedModels struct {
	Values []string
	Until  time.Time
}
type HTTPClient struct {
	base   string
	key    string
	http   *http.Client
	mu     sync.Mutex
	models map[string]cachedModels
}

func NewHTTPClient(base, key string) (*HTTPClient, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("CPA 地址必须为容器内回环 HTTP/HTTPS 地址")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("未配置巡检管理密钥环境变量")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &HTTPClient{base: strings.TrimRight(base, "/"), key: key, http: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, models: map[string]cachedModels{}}, nil
}

func (c *HTTPClient) call(ctx context.Context, method, path string, body, output any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("本地 CPA 管理接口连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("本地 CPA 管理接口返回非成功状态")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return errors.New("读取 CPA 响应失败")
	}
	if len(raw) > 16<<20 {
		return &DependencyError{"CPA 响应超出允许大小"}
	}
	if output != nil && json.Unmarshal(raw, output) != nil {
		return &DependencyError{"CPA 或检测插件响应格式不兼容"}
	}
	return nil
}

type hostCredential struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	AuthIndex   string    `json:"auth_index"`
	Email       string    `json:"email"`
	Provider    string    `json:"provider"`
	Type        string    `json:"type"`
	Disabled    bool      `json:"disabled"`
	RuntimeOnly bool      `json:"runtime_only"`
	Unavailable bool      `json:"unavailable"`
	NextRetry   time.Time `json:"next_retry_after"`
}
type candyCredential struct {
	ID           string    `json:"id"`
	Source       string    `json:"source"`
	Provider     string    `json:"provider"`
	Disabled     bool      `json:"disabled"`
	Running      *Progress `json:"running"`
	Fingerprints []Record  `json:"fingerprints"`
}

func (c *HTTPClient) hostFiles(ctx context.Context) ([]hostCredential, error) {
	var value struct {
		Files *[]hostCredential `json:"files"`
	}
	if err := c.call(ctx, "GET", "/v8/management/credentials", nil, &value); err != nil {
		return nil, err
	}
	if value.Files == nil {
		return nil, &DependencyError{"CPA 账号接口结构不兼容"}
	}
	return *value.Files, nil
}

func (c *HTTPClient) Snapshot(ctx context.Context) (Snapshot, error) {
	var list struct {
		Plugins []struct {
			ID         string `json:"id"`
			Registered bool   `json:"registered"`
			Enabled    bool   `json:"effective_enabled"`
			Metadata   struct {
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"plugins"`
	}
	if err := c.call(ctx, "GET", "/v8/management/plugins", nil, &list); err != nil {
		return Snapshot{}, err
	}
	compatible := false
	for _, p := range list.Plugins {
		if p.ID == CandyID && p.Registered && p.Enabled && p.Metadata.Version == CandyVersion {
			compatible = true
		}
	}
	if !compatible {
		return Snapshot{}, &DependencyError{"需要已启用的 Codex 降智测试 v0.4.4，已暂停巡检"}
	}
	files, err := c.hostFiles(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	var state struct {
		Credentials *[]candyCredential `json:"credentials"`
		Error       string             `json:"storage_error"`
	}
	if err = c.call(ctx, "GET", candyBase+"/state", nil, &state); err != nil {
		return Snapshot{}, err
	}
	if state.Credentials == nil || state.Error != "" {
		return Snapshot{}, &DependencyError{"检测插件状态格式不兼容或历史读取失败"}
	}
	byID := map[string]candyCredential{}
	for _, item := range *state.Credentials {
		byID[item.ID] = item
	}
	result := Snapshot{Credentials: []Credential{}}
	for _, file := range files {
		provider := file.Provider
		if provider == "" {
			provider = file.Type
		}
		if provider != "codex" || file.RuntimeOnly || !strings.HasSuffix(file.Name, ".json") {
			continue
		}
		item, ok := byID[file.ID]
		if !ok || item.Source != "auth_files" || item.Provider != "codex" || file.AuthIndex == "" {
			return Snapshot{}, &DependencyError{"检测插件与 CPA 账号身份不一致"}
		}
		unavailable := file.Unavailable && (file.NextRetry.IsZero() || file.NextRetry.After(time.Now()))
		result.Credentials = append(result.Credentials, Credential{ID: file.ID, Name: file.Name, AuthIndex: file.AuthIndex, Email: file.Email, Disabled: file.Disabled || item.Disabled, Unavailable: unavailable, Running: item.Running, Records: item.Fingerprints})
	}
	return result, nil
}

func (c *HTTPClient) Models(ctx context.Context, cred Credential) ([]string, error) {
	key := cred.ID + "/" + cred.AuthIndex
	c.mu.Lock()
	cached, ok := c.models[key]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.Until) {
		return append([]string{}, cached.Values...), nil
	}
	query := url.Values{"name": {cred.Name}, "auth_index": {cred.AuthIndex}}
	var response struct {
		Models *[]struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := c.call(ctx, "GET", "/v8/management/credentials/models?"+query.Encode(), nil, &response); err != nil {
		return nil, err
	}
	if response.Models == nil {
		return nil, &DependencyError{"账号模型目录格式不兼容"}
	}
	models := []string{}
	for _, m := range *response.Models {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	c.mu.Lock()
	c.models[key] = cachedModels{Values: models, Until: time.Now().Add(time.Minute)}
	c.mu.Unlock()
	return models, nil
}

func (c *HTTPClient) Start(ctx context.Context, cred Credential, s Settings) (StartAck, error) {
	var ack StartAck
	body := map[string]any{"credential_ids": []string{cred.ID}, "model": s.Model, "concurrency": s.PerCredential}
	err := c.call(ctx, "POST", candyBase+"/fingerprint/run", body, &ack)
	return ack, err
}
func (c *HTTPClient) Detail(ctx context.Context, id, recordID string) (Record, error) {
	var record Record
	q := url.Values{"credential_id": {id}, "id": {recordID}}
	err := c.call(ctx, "GET", candyBase+"/fingerprint/record?"+q.Encode(), nil, &record)
	if err == nil && (record.ID == "" || record.Model == "" || record.Time.IsZero()) {
		err = &DependencyError{"检测记录格式不兼容"}
	}
	return record, err
}

func (c *HTTPClient) Disable(ctx context.Context, cred Credential) error {
	files, err := c.hostFiles(ctx)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.ID != cred.ID {
			continue
		}
		provider := file.Provider
		if provider == "" {
			provider = file.Type
		}
		if file.Name != cred.Name || file.AuthIndex != cred.AuthIndex || file.Email != cred.Email || provider != "codex" || file.RuntimeOnly {
			return errors.New("账号身份已变化，未执行关闭")
		}
		if file.Disabled {
			return nil
		}
		if err = c.call(ctx, "PATCH", "/v8/management/credentials/status", map[string]any{"name": cred.Name, "auth_index": cred.AuthIndex, "disabled": true}, nil); err != nil {
			return err
		}
		updated, readErr := c.hostFiles(ctx)
		if readErr != nil {
			return readErr
		}
		for _, fresh := range updated {
			if fresh.ID == cred.ID && fresh.Name == cred.Name && fresh.AuthIndex == cred.AuthIndex && fresh.Disabled {
				return nil
			}
		}
		return errors.New("关闭后回读状态未确认")
	}
	return errors.New("账号已不存在，未执行关闭")
}
