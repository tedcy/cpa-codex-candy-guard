package guard

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

//go:embed web
var assets embed.FS

type Envelope struct {
	OK     bool           `json:"ok"`
	Result any            `json:"result,omitempty"`
	Error  *EnvelopeError `json:"error,omitempty"`
}
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status"`
}
type managementRequest struct {
	Method string `json:"Method"`
	Path   string `json:"Path"`
	Body   []byte `json:"Body"`
}
type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

var pluginMu sync.Mutex
var engine *Engine
var initError string

func marshal(value any) []byte  { raw, _ := json.Marshal(value); return raw }
func envelope(value any) []byte { return marshal(Envelope{OK: true, Result: value}) }
func response(status int, value any) managementResponse {
	return managementResponse{status, http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, marshal(value)}
}

func initialize() {
	pluginMu.Lock()
	defer pluginMu.Unlock()
	if engine != nil {
		return
	}
	base := os.Getenv("CPA_CANDY_GUARD_BASE_URL")
	if base == "" {
		base = "http://127.0.0.1:8317"
	}
	client, err := NewHTTPClient(base, os.Getenv("CPA_CANDY_GUARD_MANAGEMENT_KEY"))
	if err != nil {
		initError = err.Error()
		return
	}
	engine, err = NewEngine(client, "plugins/"+PluginID+"-state.json", Options{})
	if err != nil {
		initError = err.Error()
		return
	}
	initError = ""
}

func Quiesce() {
	pluginMu.Lock()
	e := engine
	engine = nil
	pluginMu.Unlock()
	if e != nil {
		e.Close()
	}
}

func HandleMethod(method string, raw []byte) (result []byte) {
	defer func() {
		if recover() != nil {
			result = marshal(Envelope{Error: &EnvelopeError{Code: "plugin_error", Message: "巡检插件内部错误，未执行新的关闭操作", HTTPStatus: 500}})
		}
	}()
	switch method {
	case "plugin.register", "plugin.reconfigure":
		initialize()
		logo, _ := assets.ReadFile("web/logo.svg")
		return envelope(map[string]any{"schema_version": 6, "metadata": map[string]any{"Name": "Codex 自动巡检", "Version": Version, "Author": "tedcy", "GitHubRepository": "https://github.com/tedcy/cpa-codex-candy-guard", "Logo": "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(logo), "ConfigFields": []any{}}, "capabilities": map[string]bool{"management_api": true}})
	case "management.register":
		routes := []map[string]string{}
		for _, item := range [][2]string{{"GET", "/state"}, {"PUT", "/settings"}, {"POST", "/run"}, {"POST", "/stop"}} {
			routes = append(routes, map[string]string{"Method": item[0], "Path": APIBase + item[1]})
		}
		return envelope(map[string]any{"routes": routes, "resources": []map[string]string{{"Path": UIPath, "Menu": "Codex 自动巡检", "Description": "Codex 账号指纹巡检"}}})
	case "management.handle":
		var req managementRequest
		if json.Unmarshal(raw, &req) != nil {
			return envelope(response(400, map[string]string{"error": "请求格式错误"}))
		}
		pluginMu.Lock()
		e, problem := engine, initError
		pluginMu.Unlock()
		return envelope(serve(e, problem, req))
	case "plugin.quiesce":
		Quiesce()
		return envelope(map[string]any{})
	}
	return marshal(Envelope{Error: &EnvelopeError{Code: "unknown_method", Message: "Unsupported method", HTTPStatus: 404}})
}

func serve(e *Engine, problem string, req managementRequest) managementResponse {
	path := strings.TrimRight(req.Path, "/")
	if req.Method == "GET" && path == UIPath {
		raw, _ := assets.ReadFile("web/index.html")
		css, _ := assets.ReadFile("web/style.css")
		js, _ := assets.ReadFile("web/app.js")
		icons, _ := assets.ReadFile("web/icons.js")
		html := strings.NewReplacer("/*STYLES*/", string(css), "/*ICONS*/", string(icons), "/*APP*/", string(js)).Replace(string(raw))
		return managementResponse{200, http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"}}, []byte(html)}
	}
	if e == nil {
		if problem == "" {
			problem = "巡检插件尚未初始化"
		}
		return response(503, map[string]string{"error": problem})
	}
	switch {
	case req.Method == "GET" && path == APIBase+"/state":
		return response(200, e.View(context.Background()))
	case req.Method == "PUT" && path == APIBase+"/settings":
		var settings Settings
		if json.Unmarshal(req.Body, &settings) != nil {
			return response(400, map[string]string{"error": "设置格式错误"})
		}
		if err := e.Settings(settings); err != nil {
			return response(400, map[string]string{"error": err.Error()})
		}
		return response(200, map[string]bool{"saved": true})
	case req.Method == "POST" && path == APIBase+"/run":
		id, err := e.Start("manual")
		if err != nil {
			status := 400
			if errors.Is(err, ErrBusy) {
				status = 409
			}
			return response(status, map[string]string{"error": err.Error()})
		}
		return response(200, map[string]string{"round_id": id})
	case req.Method == "POST" && path == APIBase+"/stop":
		e.Stop()
		return response(200, map[string]bool{"stopping": true})
	}
	return response(404, map[string]string{"error": "接口不存在"})
}

// Handler is used by the isolated development server; CPA supplies authentication in production.
func Handler(e *Engine) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "Invalid request", 400)
			return
		}
		result := serve(e, "", managementRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		for key, values := range result.Headers {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(result.StatusCode)
		_, _ = w.Write(result.Body)
	})
}
