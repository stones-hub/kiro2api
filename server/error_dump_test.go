package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"kiro2api/auth"
	"kiro2api/config"
	"kiro2api/internal/testutil"
	"kiro2api/logger"
	"kiro2api/parser"
	"kiro2api/types"
	"kiro2api/utils"
)

const (
	dumpClientKey = "dump-client-key-0123456789abcdef"
	dumpCookie    = "session=cookie-secret-value-0123456789"
)

// dumpUpstream is a fake Kiro whose replies may also be transport errors.
type dumpUpstream struct {
	mu    sync.Mutex
	calls int
	reply func(call int) (*http.Response, error)
}

func (u *dumpUpstream) RoundTrip(*http.Request) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	call := u.calls
	u.mu.Unlock()
	return u.reply(call)
}

func installDumpUpstream(t *testing.T, reply func(call int) (*http.Response, error)) {
	t.Helper()
	old := utils.SharedHTTPClient
	t.Cleanup(func() { utils.SharedHTTPClient = old })
	utils.SharedHTTPClient = &http.Client{Transport: &dumpUpstream{reply: reply}}
}

func kiroReply(status int, body []byte) (*http.Response, error) {
	h := make(http.Header)
	h.Set("X-Amzn-Requestid", "synthetic-kiro-request")
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

// exceptionFrame is an AWS event-stream frame of message type "exception".
func exceptionFrame(payload string) []byte {
	var headers bytes.Buffer
	for _, pair := range [][2]string{{":message-type", "exception"}, {":exception-type", "ThrottlingException"}, {":content-type", "application/json"}} {
		headers.WriteByte(byte(len(pair[0])))
		headers.WriteString(pair[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(pair[1])))
		headers.WriteString(pair[1])
	}
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(16+headers.Len()+len(payload)))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	frame.Write(headers.Bytes())
	frame.WriteString(payload)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}

var dumpExceptionStream = append(append([]byte{}, testutil.EventFrame("assistantResponseEvent", `{"content":"partial"}`)...),
	exceptionFrame(`{"__type":"ThrottlingException","message":"synthetic throttle"}`)...)

// useErrorDumps restores the recorder's global state after the test.
func useErrorDumps(t *testing.T) {
	t.Helper()
	oldStore, oldOff, oldCache := errorDumps.Load(), errorDumpOff.Load(), localDiscountCache
	t.Cleanup(func() {
		errorDumps.Store(oldStore)
		errorDumpOff.Store(oldOff)
		localDiscountCache = oldCache
	})
	errorDumpOff.Store(false)
}

// dumpRouter is the real /v1 router over a managed pool of n accounts.
func dumpRouter(t *testing.T, accounts int) (*gin.Engine, *auth.AccountPool, string) {
	t.Helper()
	useErrorDumps(t)
	pool, as := retryTestPool(t, accounts)
	return NewRouter(dumpClientKey, as), pool, filepath.Join(pool.DataDir(), errorDumpDir)
}

func dumpRequestBody(stream bool) string {
	return fmt.Sprintf(`{"model":"claude-opus-4-8","max_tokens":16,"stream":%v,"messages":[{"role":"user","content":"hello dump %s"}]}`,
		stream, strings.Repeat("x", 40))
}

func postDump(r http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+dumpClientKey)
	req.Header.Set("Cookie", dumpCookie)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// readDumps returns the dumps in dir oldest first, with their raw files.
func readDumps(t *testing.T, dir string) ([]errorDump, []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var dumps []errorDump
	var raws []string
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		var d errorDump
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		dumps = append(dumps, d)
		raws = append(raws, string(data))
	}
	return dumps, raws
}

// checkDumpSecrets fails when a dump file holds a full secret.
func checkDumpSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, secret := range []string{dumpClientKey, "cookie-secret-value-0123456789", "access-synthetic-retry-0-refresh-token", "access-synthetic-retry-1-refresh-token"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("dump holds secret %q:\n%s", secret, raw)
		}
	}
}

func checkClientSection(t *testing.T, d errorDump, path, body string) {
	t.Helper()
	if d.Client == nil || d.Client.Method != http.MethodPost || d.Client.Path != path || d.Client.Body != body || d.Client.Bytes != len(body) {
		t.Fatalf("client section %+v", d.Client)
	}
	if got := d.Client.Headers["Authorization"]; len(got) != 1 || got[0] != "Bearer dump****cdef" {
		t.Fatalf("client Authorization not masked: %v", got)
	}
	if got := d.Client.Headers["Cookie"]; len(got) != 1 || got[0] != "sess****6789" {
		t.Fatalf("client Cookie not masked: %v", got)
	}
	if got := d.Client.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("client headers incomplete: %v", d.Client.Headers)
	}
}

func checkUpstreamRequest(t *testing.T, d errorDump) {
	t.Helper()
	u := d.UpstreamRequest
	if u == nil || u.URL != config.CodeWhispererURL || !strings.Contains(u.Body, "hello dump") || u.Bytes != len(u.Body) {
		t.Fatalf("upstream request %+v", u)
	}
	auth := u.Headers["Authorization"]
	if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer acce****") || len(u.Headers["X-Amzn-Kiro-Agent-Mode"]) != 1 {
		t.Fatalf("upstream headers %v", u.Headers)
	}
}

func TestErrorDumpRecordsNon200WithAllThreeSections(t *testing.T) {
	r, pool, dir := dumpRouter(t, 1)
	installDumpUpstream(t, func(int) (*http.Response, error) {
		return kiroReply(http.StatusBadRequest, []byte(`{"message":"synthetic bad request","reason":"INVALID"}`))
	})
	body := dumpRequestBody(false)
	w := postDump(r, "/v1/messages", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client saw %d %s", w.Code, w.Body.String())
	}
	dumps, raws := readDumps(t, dir)
	if len(dumps) != 1 {
		t.Fatalf("%d dumps", len(dumps))
	}
	d := dumps[0]
	s := d.Summary
	account := pool.View().Accounts[0]
	if s.Kind != dumpUpstreamStatus || s.Attempt != 1 || s.Retried || s.UpstreamStatus != 400 || s.ClientStatus != 400 ||
		s.Model != "claude-opus-4-8" || s.Stream || s.RequestID == "" || s.RequestID != w.Header().Get("X-Request-ID") ||
		s.AccountID != account.ID || s.Account == "" || !errorDumpID.MatchString(s.ID) || s.Error == "" {
		t.Fatalf("summary %+v", s)
	}
	checkClientSection(t, d, "/v1/messages", body)
	checkUpstreamRequest(t, d)
	resp := d.UpstreamResponse
	if resp == nil || resp.Status != 400 || !strings.Contains(resp.Body, "synthetic bad request") || resp.Headers["X-Amzn-Requestid"][0] != "synthetic-kiro-request" {
		t.Fatalf("upstream response %+v", resp)
	}
	checkDumpSecrets(t, raws[0])
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("dump directory mode %v %v", info.Mode(), err)
	}
	if info, err := os.Stat(filepath.Join(dir, s.ID+".json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("dump file mode %v %v", info.Mode(), err)
	}
}

func TestErrorDumpRecordsNetworkAndProxyFailures(t *testing.T) {
	r, pool, dir := dumpRouter(t, 1)
	installDumpUpstream(t, func(int) (*http.Response, error) { return nil, errors.New("synthetic connection reset") })
	body := dumpRequestBody(true)
	if w := postDump(r, "/v1/messages", body); w.Code != http.StatusBadGateway {
		t.Fatalf("client saw %d %s", w.Code, w.Body.String())
	}
	dumps, raws := readDumps(t, dir)
	if len(dumps) != 1 || dumps[0].Summary.Kind != dumpUpstreamUnreachable || dumps[0].Summary.ClientStatus != 502 ||
		!dumps[0].Summary.Stream || dumps[0].UpstreamResponse != nil || !strings.Contains(dumps[0].Summary.Error, "connection reset") {
		t.Fatalf("network dump %+v", dumps)
	}
	checkClientSection(t, dumps[0], "/v1/messages", body)
	checkUpstreamRequest(t, dumps[0])
	checkDumpSecrets(t, raws[0])

	// The account now leaves through a proxy that refuses connections.
	proxyPort := testutil.ClosedPort(t)
	if _, err := pool.CreateProxy(auth.ProxyInput{Host: ptrTo("127.0.0.1"), Port: ptrTo(proxyPort),
		AccountIDs: &[]string{pool.View().Accounts[0].ID}}); err != nil {
		t.Fatal(err)
	}
	openai := `{"model":"claude-opus-4-8","max_tokens":16,"messages":[{"role":"user","content":"hello dump via proxy"}]}`
	if w := postDump(r, "/v1/chat/completions", openai); w.Code != http.StatusBadGateway {
		t.Fatalf("client saw %d %s", w.Code, w.Body.String())
	}
	dumps, raws = readDumps(t, dir)
	if len(dumps) != 2 || dumps[1].Summary.Kind != dumpProxyError || dumps[1].Summary.ClientStatus != 502 ||
		dumps[1].Summary.Error != "代理拒绝连接，请检查地址和端口" {
		t.Fatalf("proxy dump %+v", dumps)
	}
	// The proxy's address stays out of the record, as it does for the client.
	if strings.Contains(raws[1], fmt.Sprintf(":%d", proxyPort)) {
		t.Fatalf("proxy address in dump:\n%s", raws[1])
	}
	checkClientSection(t, dumps[1], "/v1/chat/completions", openai)
	checkUpstreamRequest(t, dumps[1])
	checkDumpSecrets(t, raws[1])
}

func TestErrorDumpRecordsExceptionsInsideA200(t *testing.T) {
	r, _, dir := dumpRouter(t, 1)
	installDumpUpstream(t, func(int) (*http.Response, error) { return kiroReply(http.StatusOK, dumpExceptionStream) })
	cases := []struct {
		path   string
		stream bool
	}{{"/v1/messages", true}, {"/v1/messages", false}, {"/v1/chat/completions", true}, {"/v1/chat/completions", false}}
	for i, tc := range cases {
		body := dumpRequestBody(tc.stream)
		if strings.Contains(tc.path, "chat") {
			body = fmt.Sprintf(`{"model":"claude-opus-4-8","max_tokens":16,"stream":%v,"messages":[{"role":"user","content":"hello dump openai"}]}`, tc.stream)
		}
		w := postDump(r, tc.path, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s stream=%v: client saw %d %s", tc.path, tc.stream, w.Code, w.Body.String())
		}
		dumps, raws := readDumps(t, dir)
		if len(dumps) != i+1 {
			t.Fatalf("%s stream=%v: %d dumps", tc.path, tc.stream, len(dumps))
		}
		d := dumps[i]
		if d.Summary.Kind != dumpStreamException || d.Summary.ClientStatus != 200 || d.Summary.UpstreamStatus != 200 ||
			d.Summary.Stream != tc.stream || !strings.Contains(d.Summary.Error, "ThrottlingException") {
			t.Fatalf("%s stream=%v: summary %+v", tc.path, tc.stream, d.Summary)
		}
		checkClientSection(t, d, tc.path, body)
		checkUpstreamRequest(t, d)
		resp := d.UpstreamResponse
		event, _ := resp.Event.(map[string]any)
		if resp.Status != 200 || event["exception_message"] != "synthetic throttle" || resp.ReadBytes == 0 || resp.Events == 0 {
			t.Fatalf("%s stream=%v: response %+v", tc.path, tc.stream, resp)
		}
		// A stream has no body to keep; a non-stream response keeps all of it.
		if tc.stream && resp.Bytes != 0 || !tc.stream && resp.Bytes != len(dumpExceptionStream) {
			t.Fatalf("%s stream=%v: response body of %d bytes", tc.path, tc.stream, resp.Bytes)
		}
		checkDumpSecrets(t, raws[i])
	}
}

func TestErrorDumpRecordsNoAccountAndConversionFailures(t *testing.T) {
	r, _, dir := dumpRouter(t, 0)
	body := dumpRequestBody(false)
	if w := postDump(r, "/v1/messages", body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("client saw %d %s", w.Code, w.Body.String())
	}
	dumps, raws := readDumps(t, dir)
	if len(dumps) != 1 || dumps[0].Summary.Kind != dumpNoAccount || dumps[0].Summary.ClientStatus != 503 ||
		dumps[0].Summary.Model != "claude-opus-4-8" || dumps[0].UpstreamRequest != nil || dumps[0].UpstreamResponse != nil {
		t.Fatalf("no-account dump %+v", dumps)
	}
	checkClientSection(t, dumps[0], "/v1/messages", body)
	checkDumpSecrets(t, raws[0])

	// The converter rejects a request with no messages; the routes catch that
	// earlier, so drive the handler directly.
	_, as := retryTestPool(t, 1)
	installDumpUpstream(t, func(int) (*http.Response, error) { t.Fatal("conversion failure reached Kiro"); return nil, nil })
	req := retryRequest("unused", false)
	req.Messages = nil
	before := len(dumps)
	got := runRoute(t, as, req, "anthropic")
	if got.status != http.StatusBadRequest {
		t.Fatalf("client saw %d %s", got.status, got.body)
	}
	// runRoute's pool keeps its dumps in the store NewRouter left installed.
	dumps, _ = readDumps(t, dir)
	if len(dumps) != before+1 || dumps[before].Summary.Kind != dumpConversionError || dumps[before].Summary.ClientStatus != 400 ||
		dumps[before].UpstreamRequest != nil || dumps[before].Client == nil || !strings.Contains(dumps[before].Summary.Error, "消息列表为空") {
		t.Fatalf("conversion dump %+v", dumps)
	}
}

// A failure the request moved past is recorded and marked; the client's answer
// is byte for byte what it is with recording off.
func TestErrorDumpMarksTheFailureARetryMovedPast(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondFail bool
	}{{"retry succeeds", false}, {"retry fails", true}} {
		t.Run(tc.name, func(t *testing.T) {
			reply := func(call int) (*http.Response, error) {
				if call%2 == 1 || tc.secondFail {
					return kiroReply(http.StatusInternalServerError, []byte(fmt.Sprintf(`{"message":"synthetic failure %d"}`, call)))
				}
				return kiroReply(http.StatusOK, retryOKFrame)
			}
			// The same request with recording off answers first (calls 1-2).
			_, pool, dir := dumpRouter(t, 2)
			installDumpUpstream(t, reply)
			errorDumpOff.Store(true)
			_, offAS := retryTestPool(t, 2)
			off := postDump(NewRouter(dumpClientKey, offAS), "/v1/messages", dumpRequestBody(false))
			if errorDumps.Load() != nil {
				t.Fatal("off still installed a store")
			}
			errorDumpOff.Store(false)
			r := NewRouter(dumpClientKey, auth.NewManagedAuthService(pool))

			on := postDump(r, "/v1/messages", dumpRequestBody(false))
			if on.Code != off.Code || stripMessageIDs(on.Body.String()) != stripMessageIDs(off.Body.String()) {
				t.Fatalf("recording changed the answer:\non  %d %s\noff %d %s", on.Code, on.Body.String(), off.Code, off.Body.String())
			}
			dumps, raws := readDumps(t, dir)
			want := 1
			if tc.secondFail {
				want = 2
			}
			if len(dumps) != want {
				t.Fatalf("%d dumps, want %d", len(dumps), want)
			}
			first := dumps[0].Summary
			if first.Attempt != 1 || !first.Retried || first.ClientStatus != 0 || first.UpstreamStatus != 500 ||
				!strings.Contains(dumps[0].UpstreamResponse.Body, "synthetic failure 3") {
				t.Fatalf("first try %+v", first)
			}
			if tc.secondFail {
				second := dumps[1].Summary
				if second.Attempt != 2 || second.Retried || second.ClientStatus != on.Code || second.AccountID == first.AccountID ||
					second.RequestID != first.RequestID || !strings.Contains(dumps[1].UpstreamResponse.Body, "synthetic failure 4") {
					t.Fatalf("second try %+v after %+v", second, first)
				}
			}
			for _, raw := range raws {
				checkDumpSecrets(t, raw)
			}
		})
	}
}

// stripMessageIDs drops the time-based ids a success carries.
func stripMessageIDs(body string) string {
	var m map[string]any
	if json.Unmarshal([]byte(body), &m) != nil {
		return body
	}
	delete(m, "id")
	out, _ := json.Marshal(m)
	return string(out)
}

func TestErrorDumpSkipsClientErrorsAndSuccesses(t *testing.T) {
	r, _, dir := dumpRouter(t, 1)
	installDumpUpstream(t, func(int) (*http.Response, error) { return kiroReply(http.StatusOK, retryOKFrame) })
	for _, body := range []string{
		`{not json`,
		`{"model":"no-such-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"claude-opus-4-8","max_tokens":16,"messages":[]}`,
		`{"model":"claude-opus-4-8","max_tokens":16,"messages":[{"role":"user","content":"  "}]}`,
	} {
		if w := postDump(r, "/v1/messages", body); w.Code < 400 || w.Code >= 500 {
			t.Fatalf("%s: client saw %d", body, w.Code)
		}
	}
	for _, stream := range []bool{false, true} {
		if w := postDump(r, "/v1/messages", dumpRequestBody(stream)); w.Code != http.StatusOK {
			t.Fatalf("success failed: %d %s", w.Code, w.Body.String())
		}
	}
	if dumps, _ := readDumps(t, dir); len(dumps) != 0 {
		t.Fatalf("recorded %d dumps for client errors and successes", len(dumps))
	}
}

func TestErrorDumpOffWritesNothing(t *testing.T) {
	for _, raw := range []string{"", "  ", "off", " OFF ", "Off"} {
		if err := SetErrorDump(raw); err != nil {
			t.Fatalf("%q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{"false", "0", "on", "true", "no", "disabled"} {
		if err := SetErrorDump(raw); err == nil {
			t.Fatalf("%q accepted", raw)
		}
	}
	useErrorDumps(t)
	pool, as := retryTestPool(t, 1)
	if err := SetErrorDump("off"); err != nil {
		t.Fatal(err)
	}
	installDumpUpstream(t, func(int) (*http.Response, error) { return kiroReply(http.StatusInternalServerError, []byte(`{}`)) })
	r := NewRouter(dumpClientKey, as)
	if w := postDump(r, "/v1/messages", dumpRequestBody(false)); w.Code < 500 {
		t.Fatalf("client saw %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(pool.DataDir(), errorDumpDir)); !os.IsNotExist(err) {
		t.Fatalf("off created the dump directory: %v", err)
	}
}

func TestErrorDumpBodyTruncation(t *testing.T) {
	big := bytes.Repeat([]byte("a"), errorDumpBodyMax+10)
	if b := dumpBody(big); !b.Truncated || b.Bytes != len(big) || len(b.Body) != errorDumpBodyMax || b.Encoding != "" {
		t.Fatalf("truncated body: %d bytes, %+v", len(b.Body), b.Truncated)
	}
	// A cut through a multi-byte character keeps the text readable.
	cjk := append(bytes.Repeat([]byte("a"), errorDumpBodyMax-1), []byte("字字")...)
	if b := dumpBody(cjk); !b.Truncated || b.Encoding != "" || !utf8.ValidString(b.Body) || len(b.Body) != errorDumpBodyMax-1 {
		t.Fatalf("multi-byte cut: encoding %q, %d bytes", b.Encoding, len(b.Body))
	}
	if b := dumpBody([]byte{0xff, 0xfe, 0x00}); b.Encoding != "base64" || b.Body != "//4A" || b.Bytes != 3 || b.Truncated {
		t.Fatalf("binary body %+v", b)
	}
	if b := dumpBody(nil); b.Body != "" || b.Bytes != 0 || b.Truncated {
		t.Fatalf("empty body %+v", b)
	}
}

func TestErrorDumpMasking(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer abcdefghijklmnopqrstuvwxyz")
	h.Set("X-Api-Key", "short-key")
	h.Set("X-Kiro-Admin-Token", "admin-token-abcdefghijklmnop")
	h.Set("Cookie", "a=b")
	h.Set("Set-Cookie", "sid=0123456789abcdef; Path=/")
	h.Set("User-Agent", "synthetic-agent")
	got := maskHeaders(h)
	for k, want := range map[string]string{
		"Authorization":      "Bearer abcd****wxyz",
		"X-Api-Key":          "*********",
		"X-Kiro-Admin-Token": "admi****mnop",
		"Cookie":             "***",
		"Set-Cookie":         "sid=****th=/",
		"User-Agent":         "synthetic-agent",
	} {
		if got[k][0] != want {
			t.Fatalf("%s: %q, want %q", k, got[k][0], want)
		}
	}
	if h.Get("Authorization") != "Bearer abcdefghijklmnopqrstuvwxyz" {
		t.Fatal("masking changed the live headers")
	}
}

func TestErrorDumpKeepsTheNewest200(t *testing.T) {
	store := &errorDumpStore{dir: filepath.Join(t.TempDir(), errorDumpDir)}
	base := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < errorDumpKeep+5; i++ {
		id := fmt.Sprintf("%s-%08x", strings.Replace(base.Add(time.Duration(i)*time.Second).Format("20060102-150405.000"), ".", "-", 1), i)
		ids = append(ids, id)
		if err := store.write(&errorDump{Summary: errorDumpSummary{ID: id, Kind: dumpUpstreamStatus}}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != errorDumpKeep || list[0].ID != ids[len(ids)-1] || list[len(list)-1].ID != ids[5] {
		t.Fatalf("kept %d, newest %s, oldest %s", len(list), list[0].ID, list[len(list)-1].ID)
	}
	if _, ok, _ := store.read(ids[4]); ok {
		t.Fatal("oldest dump survived")
	}
}

func TestAdminErrorDumpRoutes(t *testing.T) {
	useErrorDumps(t)
	pool, _ := retryTestPool(t, 1)
	secret := strings.Repeat("private-admin-key-", 3)
	h, err := NewAdminRouter(pool, secret, []string{"https://panel.example.com"}, "../static/admin")
	if err != nil {
		t.Fatal(err)
	}
	get := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Kiro-Admin-Token", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	store := errorDumpStoreFor(pool)
	ids := []string{"20261029-080000-000-0000000a", "20261029-080001-000-0000000b", "20261029-080002-000-0000000c"}
	for _, id := range ids {
		d := &errorDump{Summary: errorDumpSummary{ID: id, Kind: dumpUpstreamStatus, Error: "synthetic " + id},
			Client: &errorDumpClient{Method: "POST", Path: "/v1/messages", errorDumpBody: dumpBody([]byte(`{"x":"<script>"}`))}}
		if err := store.write(d); err != nil {
			t.Fatal(err)
		}
	}
	if w := get("/api/error-dumps", "wrong-key"); w.Code != 401 {
		t.Fatalf("unauthorized list: %d", w.Code)
	}
	if w := get("/api/error-dumps/"+ids[0], "wrong-key"); w.Code != 401 {
		t.Fatalf("unauthorized detail: %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/error-dumps", nil)
	req.Header.Set("X-Kiro-Admin-Token", secret)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("cross-origin list: %d", w.Code)
	}

	w = get("/api/error-dumps", secret)
	var list struct {
		Enabled bool               `json:"enabled"`
		Dumps   []errorDumpSummary `json:"dumps"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || !list.Enabled || len(list.Dumps) != 3 ||
		list.Dumps[0].ID != ids[2] || list.Dumps[2].ID != ids[0] || strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("list %d %s", w.Code, w.Body.String())
	}
	w = get("/api/error-dumps/"+ids[1], secret)
	var detail errorDump
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Summary.ID != ids[1] || detail.Client.Body != `{"x":"<script>"}` {
		t.Fatalf("detail %d %s", w.Code, w.Body.String())
	}
	for _, bad := range []string{"..", "..%2F..%2Faccounts", "20261029-080000-000-0000000A", "accounts", "20261029-080000-000-0000000a.json"} {
		if w := get("/api/error-dumps/"+bad, secret); w.Code != 400 && w.Code != 404 {
			t.Fatalf("id %q: %d %s", bad, w.Code, w.Body.String())
		}
	}
	if w := get("/api/error-dumps/not-an-id", secret); w.Code != 400 {
		t.Fatalf("invalid id: %d", w.Code)
	}
	if w := get("/api/error-dumps/20261029-080009-000-0000000f", secret); w.Code != 404 {
		t.Fatalf("unknown id: %d", w.Code)
	}

	errorDumpOff.Store(true)
	w = get("/api/error-dumps", secret)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || list.Enabled || len(list.Dumps) != 0 {
		t.Fatalf("disabled list %d %s", w.Code, w.Body.String())
	}
}

// The parser is lenient enough that no fake response makes it fail, so the
// parse-failure record is checked on its own.
func TestErrorDumpRecordsUnparsableNonStreamResponse(t *testing.T) {
	useErrorDumps(t)
	dir := filepath.Join(t.TempDir(), errorDumpDir)
	errorDumps.Store(&errorDumpStore{dir: dir})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("X-Api-Key", dumpClientKey)
	rememberClientBody(c, []byte(`{"model":"m"}`))
	garbage := []byte{0, 0, 0, 5, 0xff, 0xfe}
	resp, _ := kiroReply(http.StatusOK, garbage)
	recordParseFailure(c, resp, garbage, errors.New("synthetic parse failure")).save(c, http.StatusInternalServerError, false)
	dumps, raws := readDumps(t, dir)
	if len(dumps) != 1 {
		t.Fatalf("%d dumps", len(dumps))
	}
	d := dumps[0]
	if d.Summary.Kind != dumpParseError || d.Summary.ClientStatus != 500 || d.Summary.UpstreamStatus != 200 ||
		d.Client.Body != `{"model":"m"}` || d.Client.Headers["X-Api-Key"][0] != "dump****cdef" ||
		d.UpstreamResponse.Encoding != "base64" || d.UpstreamResponse.Bytes != len(garbage) || currentErrorDump(c) != d.Summary.ID {
		t.Fatalf("parse failure dump %s", raws[0])
	}
	checkDumpSecrets(t, raws[0])
	fields := withErrorDump(c)
	if last := fields[len(fields)-1]; last.Key != "error_dump" || last.Value != d.Summary.ID {
		t.Fatalf("log fields %+v", fields)
	}
}

// A dump whose client left is not saved, and the log says so under its id.
func TestErrorDumpSkippedSaveIsLogged(t *testing.T) {
	useErrorDumps(t)
	dir := filepath.Join(t.TempDir(), errorDumpDir)
	errorDumps.Store(&errorDumpStore{dir: dir})
	var logged []string
	old := logErrorDumpSkipped
	t.Cleanup(func() { logErrorDumpSkipped = old })
	logErrorDumpSkipped = func(msg string, fields ...logger.Field) {
		for _, f := range fields {
			if f.Key == "error_dump" {
				logged = append(logged, msg+" "+f.Value.(string))
			}
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	d := newErrorDump(c, dumpUpstreamUnreachable, "synthetic")
	cancel()
	d.save(c, http.StatusBadGateway, false)
	if dumps, _ := readDumps(t, dir); len(dumps) != 0 {
		t.Fatal("dump saved for a client that left")
	}
	if len(logged) != 1 || logged[0] != "客户端已断开，未保存错误现场 "+d.Summary.ID {
		t.Fatalf("skip log %v", logged)
	}
}

// Kiro's content-length exception is answered as a max_tokens stop; it is not
// an error and leaves no dump, on any route.
func TestErrorDumpSkipsMaxTokensExceptions(t *testing.T) {
	r, _, dir := dumpRouter(t, 1)
	stream := append(append([]byte{}, testutil.EventFrame("assistantResponseEvent", `{"content":"partial"}`)...),
		exceptionFrame(`{"__type":"ContentLengthExceededException","message":"synthetic too long"}`)...)
	installDumpUpstream(t, func(int) (*http.Response, error) { return kiroReply(http.StatusOK, stream) })
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		for _, isStream := range []bool{true, false} {
			body := fmt.Sprintf(`{"model":"claude-opus-4-8","max_tokens":16,"stream":%v,"messages":[{"role":"user","content":"hello dump"}]}`, isStream)
			if w := postDump(r, path, body); w.Code != http.StatusOK {
				t.Fatalf("%s stream=%v: %d %s", path, isStream, w.Code, w.Body.String())
			}
		}
	}
	if dumps, _ := readDumps(t, dir); len(dumps) != 0 {
		t.Fatalf("max_tokens exception recorded: %+v", dumps)
	}
	// A real failure after it in the same response is still found.
	events := []parser.SSEEvent{
		{Event: "exception", Data: map[string]any{"type": "exception", "exception_type": "ContentLengthExceededException"}},
		{Event: "exception", Data: map[string]any{"type": "exception", "exception_type": "ThrottlingException"}},
	}
	if got := firstExceptionEvent(events); got == nil || got["exception_type"] != "ThrottlingException" {
		t.Fatalf("first recordable exception %v", got)
	}
	if firstExceptionEvent(events[:1]) != nil {
		t.Fatal("max_tokens exception treated as a failure")
	}
}

// A request that fails to build for any reason other than conversion is the
// bridge's 500, not the client's 400.
func TestErrorDumpRecordsBuildFailureAs500(t *testing.T) {
	// NewRouter installs the store; the handler runs on a second pool.
	_, _, dir := dumpRouter(t, 1)
	_, as := retryTestPool(t, 1)
	installDumpUpstream(t, func(int) (*http.Response, error) { t.Fatal("build failure reached Kiro"); return nil, nil })
	req := retryRequest("unmarshalable tool schema", false)
	req.Tools = []types.AnthropicTool{{Name: "probe", Description: "probe", InputSchema: map[string]any{"type": "object", "bad": make(chan int)}}}
	got := runRoute(t, as, req, "anthropic")
	if got.status != http.StatusInternalServerError {
		t.Fatalf("client saw %d %s", got.status, got.body)
	}
	dumps, _ := readDumps(t, dir)
	if len(dumps) != 1 || dumps[0].Summary.Kind != dumpBuildError || dumps[0].Summary.ClientStatus != 500 {
		t.Fatalf("build failure dump %+v", dumps)
	}
}

func TestErrorDumpEventSizeLimit(t *testing.T) {
	var r errorDumpUpstreamResponse
	small := map[string]any{"type": "exception", "exception_message": "short"}
	if r.setEvent(small); r.EventTruncated || r.EventBytes != 0 {
		t.Fatalf("small event changed: %+v", r)
	}
	if m, ok := r.Event.(map[string]any); !ok || m["exception_message"] != "short" {
		t.Fatalf("small event %v", r.Event)
	}
	big := map[string]any{"type": "exception", "exception_message": strings.Repeat("字", errorDumpBodyMax)}
	encoded, _ := json.Marshal(big)
	r.setEvent(big)
	text, ok := r.Event.(string)
	if !ok || !r.EventTruncated || r.EventBytes != len(encoded) || len(text) > errorDumpBodyMax || !utf8.ValidString(text) || !strings.HasPrefix(text, `{"exception_message":"字`) {
		t.Fatalf("big event: truncated=%v bytes=%d len=%d", r.EventTruncated, r.EventBytes, len(text))
	}
}

func TestErrorDumpWriteRemovesLeftoverTempFiles(t *testing.T) {
	store := &errorDumpStore{dir: filepath.Join(t.TempDir(), errorDumpDir)}
	if err := os.MkdirAll(store.dir, 0700); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(store.dir, ".dump-123.tmp")
	other := filepath.Join(store.dir, "notes.txt")
	for _, name := range []string{leftover, other} {
		if err := os.WriteFile(name, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.write(&errorDump{Summary: errorDumpSummary{ID: "20261029-080000-000-0001abcd"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatal("leftover temp file kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated file removed")
	}
}

func TestUpstreamEventMessageNeverShowsNil(t *testing.T) {
	for _, tc := range []struct {
		event map[string]any
		want  string
	}{
		{map[string]any{"type": "error", "error_code": "InternalError", "error_message": "boom"}, "InternalError boom"},
		{map[string]any{"type": "error", "error_code": "", "error_message": "", "raw_data": map[string]any{"message": "from raw"}}, "from raw"},
		{map[string]any{"type": "error", "error": map[string]any{"message": "nested"}}, "nested"},
		{map[string]any{"type": "error", "raw_data": nil}, ""},
		{map[string]any{"type": "exception"}, ""},
		{map[string]any{"type": "exception", "exception_type": "ThrottlingException", "raw_data": map[string]any{"message": "slow down"}}, "ThrottlingException slow down"},
	} {
		if got := upstreamEventMessage(tc.event); got != tc.want || strings.Contains(got, "<nil>") {
			t.Fatalf("%v: %q, want %q", tc.event, got, tc.want)
		}
	}
}
