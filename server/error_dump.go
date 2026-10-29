package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/logger"
	"kiro2api/parser"
)

// Error dumps keep the full scene of a failed try at Kiro — what the client
// sent, what went to Kiro, what Kiro answered — under the management data
// directory, for the admin page's error log. See docs/error-dump.md.
const (
	errorDumpDir     = "error-dumps"
	errorDumpKeep    = 200
	errorDumpBodyMax = 2 << 20

	errorDumpClientBodyKey = "error_dump_client_body"
	errorDumpUpstreamKey   = "error_dump_upstream_request"
	errorDumpResponseKey   = "error_dump_upstream_response"
	errorDumpIDKey         = "error_dump_id"
	errorDumpStreamKey     = "error_dump_stream_recorded"
)

// Kinds of failure a dump records.
const (
	dumpUpstreamStatus      = "upstream_status"
	dumpUpstreamUnreachable = "upstream_unreachable"
	dumpProxyError          = "proxy_error"
	dumpStreamException     = "stream_exception"
	dumpParseError          = "parse_error"
	dumpNoAccount           = "no_account"
	dumpConversionError     = "conversion_error"
	dumpBuildError          = "build_error"
)

// errorDumpID matches the ids this file generates and nothing else, so an id
// from a URL can never name a path outside the dump directory.
var errorDumpID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9]{3}-[0-9a-f]{8}$`)

var (
	errorDumpOff atomic.Bool
	errorDumpSeq atomic.Uint32
	// errorDumps is the store requests write to; nil records nothing.
	errorDumps atomic.Pointer[errorDumpStore]
)

// SetErrorDump applies KIRO_ERROR_DUMP: unset or empty records, "off" does
// not, anything else is rejected so a "false" or "0" is not silently ignored.
func SetErrorDump(raw string) error {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		errorDumpOff.Store(false)
	case "off":
		errorDumpOff.Store(true)
	default:
		return fmt.Errorf("KIRO_ERROR_DUMP 只能留空（开启）或设为 off（关闭），当前值为 %q", raw)
	}
	return nil
}

// errorDumpStoreFor is the dump store of a managed pool, or nil when dumps are
// off or the bridge is not in management mode.
func errorDumpStoreFor(pool *auth.AccountPool) *errorDumpStore {
	if pool == nil || errorDumpOff.Load() {
		return nil
	}
	return &errorDumpStore{dir: filepath.Join(pool.DataDir(), errorDumpDir)}
}

func logErrorDumpMode(pool *auth.AccountPool) {
	switch {
	case pool == nil:
		logger.Info("错误现场记录：非管理模式，不记录")
	case errorDumpOff.Load():
		logger.Info("错误现场记录：已关闭（KIRO_ERROR_DUMP=off）")
	default:
		logger.Info("错误现场记录：已开启", logger.String("dir", filepath.Join(pool.DataDir(), errorDumpDir)))
	}
}

type errorDumpStore struct {
	dir string
	mu  sync.Mutex
}

type errorDumpSummary struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	RequestID string    `json:"request_id,omitempty"`
	// Attempt counts tries at Kiro: 2 is the try on another account.
	Attempt int `json:"attempt"`
	// Retried marks a failure the request moved past onto another account;
	// the client never saw it.
	Retried        bool   `json:"retried"`
	AccountID      string `json:"account_id,omitempty"`
	Account        string `json:"account,omitempty"`
	Model          string `json:"model,omitempty"`
	Stream         bool   `json:"stream"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	ClientStatus   int    `json:"client_status,omitempty"`
	Error          string `json:"error"`
}

type errorDumpBody struct {
	Body string `json:"body"`
	// Encoding is "base64" when the bytes were not valid UTF-8.
	Encoding string `json:"body_encoding,omitempty"`
	// Bytes is the original size, before any truncation.
	Bytes     int  `json:"body_bytes"`
	Truncated bool `json:"body_truncated,omitempty"`
}

type errorDumpClient struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	errorDumpBody
}

type errorDumpUpstreamRequest struct {
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers"`
	errorDumpBody
}

type errorDumpUpstreamResponse struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	errorDumpBody
	// Event is the exception or error event Kiro sent inside a 200 response;
	// past errorDumpBodyMax once encoded it is kept as truncated JSON text.
	Event          any  `json:"event,omitempty"`
	EventBytes     int  `json:"event_bytes,omitempty"`
	EventTruncated bool `json:"event_truncated,omitempty"`
	// ReadBytes and Events count what was read before that event in a stream.
	ReadBytes int `json:"read_bytes,omitempty"`
	Events    int `json:"events,omitempty"`
}

// errorDump is one failed try. The summary comes first in the file so the
// list can read it without loading the bodies.
type errorDump struct {
	Summary          errorDumpSummary           `json:"summary"`
	Client           *errorDumpClient           `json:"client"`
	UpstreamRequest  *errorDumpUpstreamRequest  `json:"upstream_request"`
	UpstreamResponse *errorDumpUpstreamResponse `json:"upstream_response"`
	store            *errorDumpStore
}

// upstreamCapture is the request last sent to Kiro, kept by reference so a
// successful request pays nothing beyond holding it.
type upstreamCapture struct {
	url    string
	header http.Header
	body   []byte
}

// rememberClientBody keeps the raw client body for a later dump; the slice is
// the one already read, not a copy.
func rememberClientBody(c *gin.Context, body []byte) {
	if errorDumps.Load() != nil {
		c.Set(errorDumpClientBodyKey, body)
	}
}

func rememberUpstreamRequest(c *gin.Context, req *http.Request, body []byte) {
	if errorDumps.Load() != nil {
		c.Set(errorDumpUpstreamKey, &upstreamCapture{url: req.URL.String(), header: req.Header, body: body})
	}
}

func rememberUpstreamResponse(c *gin.Context, resp *http.Response) {
	if errorDumps.Load() != nil {
		c.Set(errorDumpResponseKey, resp)
	}
}

// newErrorDump captures the client side of the current request for a failure
// of the given kind; nil when dumps are off. All methods accept nil.
func newErrorDump(c *gin.Context, kind, message string) *errorDump {
	store := errorDumps.Load()
	if store == nil {
		return nil
	}
	now := time.Now()
	// A sequence ahead of the random part keeps dumps of the same millisecond
	// in order.
	var random [2]byte
	_, _ = rand.Read(random[:])
	id := fmt.Sprintf("%s-%04x%s", strings.Replace(now.UTC().Format("20060102-150405.000"), ".", "-", 1),
		uint16(errorDumpSeq.Add(1)), hex.EncodeToString(random[:]))
	d := &errorDump{store: store, Summary: errorDumpSummary{ID: id, Time: now, Kind: kind, RequestID: GetRequestID(c),
		Attempt: 1, Stream: c.GetBool("request_stream"), Error: message}}
	if v, ok := c.Get(accountSwitcherKey); ok {
		if s, _ := v.(*accountSwitcher); s != nil {
			d.Summary.Model = s.model
			if s.moved {
				d.Summary.Attempt = 2
			}
			if r := accountMeasurement(c); r != nil {
				d.Summary.AccountID = r.lease.AccountID()
				d.Summary.Account = s.pool.AccountEmail(d.Summary.AccountID)
			}
		}
	}
	if c.Request != nil {
		body, _ := c.Get(errorDumpClientBodyKey)
		raw, _ := body.([]byte)
		d.Client = &errorDumpClient{Method: c.Request.Method, Path: c.Request.URL.RequestURI(),
			Headers: maskHeaders(c.Request.Header), errorDumpBody: dumpBody(raw)}
	}
	c.Set(errorDumpIDKey, id)
	return d
}

// withUpstreamRequest adds the request last sent to Kiro.
func (d *errorDump) withUpstreamRequest(c *gin.Context) *errorDump {
	if d == nil {
		return nil
	}
	if v, ok := c.Get(errorDumpUpstreamKey); ok {
		if u, _ := v.(*upstreamCapture); u != nil {
			d.UpstreamRequest = &errorDumpUpstreamRequest{URL: u.url, Headers: maskHeaders(u.header), errorDumpBody: dumpBody(u.body)}
		}
	}
	return d
}

// withUpstreamResponse adds what Kiro answered; body may be nil.
func (d *errorDump) withUpstreamResponse(resp *http.Response, body []byte) *errorDump {
	if d == nil || resp == nil {
		return d
	}
	d.Summary.UpstreamStatus = resp.StatusCode
	d.UpstreamResponse = &errorDumpUpstreamResponse{Status: resp.StatusCode, Headers: maskHeaders(resp.Header), errorDumpBody: dumpBody(body)}
	return d
}

func (d *errorDump) id() string {
	if d == nil {
		return ""
	}
	return d.Summary.ID
}

// save writes the dump. clientStatus is what the client was answered; a
// retried failure reached no client. A client that left records nothing, and a
// write failure only warns.
func (d *errorDump) save(c *gin.Context, clientStatus int, retried bool) {
	if d == nil {
		return
	}
	// The id may already be in an error line; say why it has no file.
	if !clientPresent(c) {
		logErrorDumpSkipped("客户端已断开，未保存错误现场", addReqFields(c, logger.String("error_dump", d.Summary.ID))...)
		return
	}
	d.Summary.Retried = retried
	if !retried {
		d.Summary.ClientStatus = clientStatus
	}
	if err := d.store.write(d); err != nil {
		logger.Warn("错误现场写入失败", addReqFields(c, logger.String("error_dump", d.Summary.ID), logger.Err(err))...)
	}
}

// logErrorDumpSkipped reports a dump not saved; tests replace it.
var logErrorDumpSkipped = logger.Info

// currentErrorDump is the id of the dump the request's latest failure created.
func currentErrorDump(c *gin.Context) string {
	return c.GetString(errorDumpIDKey)
}

// withErrorDump is addReqFields plus the latest dump id, when there is one.
func withErrorDump(c *gin.Context, fields ...logger.Field) []logger.Field {
	if id := currentErrorDump(c); id != "" {
		fields = append(fields, logger.String("error_dump", id))
	}
	return addReqFields(c, fields...)
}

// recordNoAccount records a request no account was free to serve.
func recordNoAccount(c *gin.Context, model string, status int, err error) {
	d := newErrorDump(c, dumpNoAccount, err.Error())
	if d == nil {
		return
	}
	d.Summary.Model = model
	logger.Warn("没有可用账号", withErrorDump(c, logger.String("model", model), logger.Err(err))...)
	d.save(c, status, false)
}

// recordUpstreamException records the first exception or error event Kiro sent
// inside a 200 response. A stream has no body to keep: it records the event and
// how far the stream got. A non-stream response keeps its full body.
func recordUpstreamException(c *gin.Context, event map[string]any, body []byte, readBytes, events int) {
	if errorDumps.Load() == nil || c.GetBool(errorDumpStreamKey) || !recordableException(event) {
		return
	}
	c.Set(errorDumpStreamKey, true)
	kind, _ := event["type"].(string)
	message := upstreamEventMessage(event)
	d := newErrorDump(c, dumpStreamException, "Kiro 返回 "+kind+": "+message).withUpstreamRequest(c)
	if v, ok := c.Get(errorDumpResponseKey); ok {
		resp, _ := v.(*http.Response)
		d.withUpstreamResponse(resp, body)
	}
	if d.UpstreamResponse != nil {
		d.UpstreamResponse.setEvent(event)
		d.UpstreamResponse.ReadBytes = readBytes
		d.UpstreamResponse.Events = events
	}
	logger.Warn("上游响应中出现异常事件", withErrorDump(c, logger.String("event_type", kind), logger.String("detail", message),
		logger.Int("upstream_bytes", readBytes), logger.Int("upstream_events", events))...)
	d.save(c, http.StatusOK, false)
}

// recordableException excludes the exceptions the bridge answers as a normal
// max_tokens stop: the client sees a truncated answer, not an error.
func recordableException(event map[string]any) bool {
	if t, _ := event["type"].(string); t == "exception" {
		exceptionType, _ := event["exception_type"].(string)
		return !maxTokensException(exceptionType)
	}
	return true
}

// upstreamEventMessage describes an exception or error event in one line,
// falling back to the nested message when the parser found no code or text.
func upstreamEventMessage(event map[string]any) string {
	str := func(v any) string { s, _ := v.(string); return s }
	nested := func(v any) string { m, _ := v.(map[string]any); return str(m["message"]) }
	var code, text string
	if t, _ := event["type"].(string); t == "error" {
		code, text = str(event["error_code"]), str(event["error_message"])
		if text == "" {
			text = nested(event["error"])
		}
	} else {
		code, text = str(event["exception_type"]), str(event["exception_message"])
	}
	if text == "" {
		text = nested(event["raw_data"])
	}
	if text == "" {
		text = str(event["message"])
	}
	return strings.TrimSpace(code + " " + text)
}

// setEvent keeps the event, as truncated JSON text when it is too large.
func (r *errorDumpUpstreamResponse) setEvent(event map[string]any) {
	r.Event = event
	encoded, err := json.Marshal(event)
	if err != nil || len(encoded) <= errorDumpBodyMax {
		return
	}
	b := dumpBody(encoded)
	r.Event, r.EventBytes, r.EventTruncated = b.Body, b.Bytes, true
}

// recordParseFailure starts the dump of a non-stream response that could not be
// parsed; the caller saves it once the client status is known.
func recordParseFailure(c *gin.Context, resp *http.Response, body []byte, err error) *errorDump {
	return newErrorDump(c, dumpParseError, "响应解析失败: "+err.Error()).withUpstreamRequest(c).withUpstreamResponse(resp, body)
}

// firstExceptionEvent finds an exception or error event in parsed events.
func firstExceptionEvent(events []parser.SSEEvent) map[string]any {
	for _, e := range events {
		if e.Event == "exception" || e.Event == "error" {
			if data, _ := e.Data.(map[string]any); data != nil && recordableException(data) {
				return data
			}
		}
	}
	return nil
}

func (s *errorDumpStore) write(d *errorDump) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0700); err != nil {
		return err
	}
	// Temp files a crash left behind; only this writer creates them.
	if leftovers, _ := filepath.Glob(filepath.Join(s.dir, ".dump-*.tmp")); len(leftovers) > 0 {
		for _, name := range leftovers {
			_ = os.Remove(name)
		}
	}
	f, err := os.CreateTemp(s.dir, ".dump-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(buf.Bytes())
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(s.dir, d.Summary.ID+".json")); err != nil {
		return err
	}
	ids, err := s.idsLocked()
	if err != nil {
		return err
	}
	for len(ids) > errorDumpKeep {
		_ = os.Remove(filepath.Join(s.dir, ids[0]+".json"))
		ids = ids[1:]
	}
	return nil
}

// idsLocked lists the dump ids oldest first; ids sort by time.
func (s *errorDumpStore) idsLocked() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && e.Type().IsRegular() && errorDumpID.MatchString(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// list returns the summaries, newest first.
func (s *errorDumpStore) list() ([]errorDumpSummary, error) {
	s.mu.Lock()
	ids, err := s.idsLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]errorDumpSummary, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		if summary, err := s.readSummary(ids[i]); err == nil {
			out = append(out, summary)
		}
	}
	return out, nil
}

// readSummary decodes only the leading summary object of a dump file.
func (s *errorDumpStore) readSummary(id string) (errorDumpSummary, error) {
	var out errorDumpSummary
	f, err := os.Open(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return out, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if _, err = dec.Token(); err != nil {
		return out, err
	}
	key, err := dec.Token()
	if err != nil {
		return out, err
	}
	if key != "summary" {
		return out, fmt.Errorf("dump %s does not start with its summary", id)
	}
	err = dec.Decode(&out)
	return out, err
}

// read returns a dump file as stored; ok is false for an invalid or unknown id.
func (s *errorDumpStore) read(id string) ([]byte, bool, error) {
	if !errorDumpID.MatchString(id) {
		return nil, false, nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, id+".json"))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return data, err == nil, err
}

// errorDumpSecretHeaders are masked wherever they appear: client requests,
// the request to Kiro and Kiro's response.
var errorDumpSecretHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"X-Api-Key":           true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"X-Kiro-Admin-Token":  true,
}

func maskHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, values := range h {
		copied := append([]string(nil), values...)
		if errorDumpSecretHeaders[http.CanonicalHeaderKey(k)] {
			for i, v := range copied {
				copied[i] = maskHeaderValue(v)
			}
		}
		out[k] = copied
	}
	return out
}

// maskHeaderValue keeps an auth scheme such as "Bearer" readable.
func maskHeaderValue(v string) string {
	if scheme, rest, ok := strings.Cut(v, " "); ok && (strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "Basic")) {
		return scheme + " " + maskSecret(strings.TrimSpace(rest))
	}
	return maskSecret(v)
}

// maskSecret keeps the first and last 4 bytes of a secret; one too short to
// hide anything in between is masked whole.
func maskSecret(s string) string {
	if len(s) < 12 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// dumpBody keeps at most errorDumpBodyMax bytes, as text when they are UTF-8
// and as base64 otherwise.
func dumpBody(b []byte) errorDumpBody {
	out := errorDumpBody{Bytes: len(b)}
	if len(b) > errorDumpBodyMax {
		b = b[:errorDumpBodyMax]
		out.Truncated = true
	}
	text := b
	// The cut may split the last character.
	for i := 0; out.Truncated && i < utf8.UTFMax-1 && len(text) > 0 && !utf8.Valid(text); i++ {
		text = text[:len(text)-1]
	}
	if utf8.Valid(text) {
		out.Body = string(text)
		return out
	}
	out.Body = base64.StdEncoding.EncodeToString(b)
	out.Encoding = "base64"
	return out
}

// registerErrorDumpRoutes serves the admin page's error log.
func registerErrorDumpRoutes(r *gin.Engine, pool *auth.AccountPool) {
	r.GET("/api/error-dumps", func(c *gin.Context) {
		store := errorDumpStoreFor(pool)
		if store == nil {
			c.JSON(200, gin.H{"enabled": false, "dumps": []errorDumpSummary{}})
			return
		}
		dumps, err := store.list()
		if err != nil {
			c.JSON(500, gin.H{"error": "读取错误日志失败"})
			return
		}
		c.JSON(200, gin.H{"enabled": true, "dumps": dumps, "keep": errorDumpKeep})
	})
	r.GET("/api/error-dumps/:id", func(c *gin.Context) {
		id := c.Param("id")
		if !errorDumpID.MatchString(id) {
			c.JSON(400, gin.H{"error": "记录编号无效"})
			return
		}
		store := errorDumpStoreFor(pool)
		if store == nil {
			c.JSON(404, gin.H{"error": "错误日志已关闭"})
			return
		}
		data, ok, err := store.read(id)
		if err != nil {
			c.JSON(500, gin.H{"error": "读取错误日志失败"})
			return
		}
		if !ok {
			c.JSON(404, gin.H{"error": "记录不存在或已被清理"})
			return
		}
		c.Data(200, "application/json; charset=utf-8", data)
	})
}
