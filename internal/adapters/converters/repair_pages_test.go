package converters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func repairTestPython(t *testing.T, modules ...string) string {
	t.Helper()
	python := os.Getenv("DRIVE_TEST_PYTHON")
	if python == "" {
		var err error
		python, err = exec.LookPath("python3")
		if err != nil {
			t.Skip("python3 unavailable for converter subprocess test")
		}
	}
	if len(modules) > 0 {
		if err := exec.Command(python, "-c", "import "+strings.Join(modules, ",")).Run(); err != nil {
			t.Skipf("converter python dependencies unavailable: %v", err)
		}
	}
	return python
}

func TestRepairConversionHTTPStreamsPagesBeforeResult(t *testing.T) {
	pageSent := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in ports.ConversionRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !in.PageReport || !reflect.DeepEqual(in.ResumePages, []int{2}) {
			t.Error("v2 请求", in, err)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"page","page":2,"stage":"text","state":"ready","result_ref":{"markdown":"saved"}}` + "\n"))
		w.(http.Flusher).Flush()
		close(pageSent)
		<-release
		_, _ = w.Write([]byte(`{"type":"result","status":"ready","engine":{"name":"test","version":"1"},"markdown":"saved","completeness":{"status":"ready","total_pages":2}}` + "\n"))
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	confirmed := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, e := client.ExecutePages(context.Background(), ports.ConversionRequest{PageReport: true, ResumePages: []int{2}, JobID: "j", AttemptID: "1", OutputScope: "tmp/j/1/", Input: ports.ConversionInput{SourceVersionID: "v"}, Recipe: ports.Recipe{MaxPages: 3, TimeoutSeconds: 2}}, func(page ports.PageEvent) error { confirmed <- struct{}{}; return nil })
		done <- e
	}()
	<-pageSent
	select {
	case <-confirmed:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("等完整结果才回调，页进度未流式")
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRepairConversionProcessTimeoutKillAndBounds(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake.py")
	body := `import json,sys,time,os
r=json.load(open(sys.argv[-1]))
print(json.dumps({'type':'page','page':1,'stage':'text','state':'ready','result_ref':{'markdown':'saved'}}),flush=True)
if r['attempt_id']=='kill': os._exit(9)
time.sleep(10)
`
	if err := os.WriteFile(script, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	p := &Process{Command: repairTestPython(t), Args: []string{script}, MaxResponseBytes: 4096}
	for _, attempt := range []string{"timeout", "kill"} {
		calls := 0
		_, err := p.ExecutePages(context.Background(), ports.ConversionRequest{JobID: "j", AttemptID: attempt, Recipe: ports.Recipe{MaxPages: 2, TimeoutSeconds: 1}}, func(page ports.PageEvent) error { calls++; return nil })
		if err == nil || calls != 1 {
			t.Fatal(attempt, calls, err)
		}
		if attempt == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
	for _, input := range []string{
		`{"type":"page","page":5,"stage":"text","state":"ready","result_ref":{}}`,
		`{"type":"page","page":1,"stage":"text","state":"failed","result_ref":{}}`,
		`{"type":"result","status":"ready"}` + "\n" + `{"type":"page","page":1,"stage":"text","state":"ready","result_ref":{}}`,
	} {
		r := pageResponse{maxPages: 2}
		if err := r.decode(strings.NewReader(input), 4096); err == nil {
			t.Fatal("非法事件被接受", input)
		}
	}
	r := pageResponse{maxPages: 2}
	if err := r.decode(strings.NewReader(strings.Repeat(" ", 1024)), 100); !errors.Is(err, ports.ErrLimit) {
		t.Fatal("输出上限", err)
	}
}

// 合成 PDF 使用 reportlab，转换使用真实 --execute，不含 OCR/视觉替身。
// 本分支旧 adapter 返回单 JSON：本测试验证兼容与真实文本 PDF，不冒充 v2 集成。
func TestRepairConversionRealAdapterSyntheticPDF(t *testing.T) {
	python := repairTestPython(t, "pypdf", "pypdfium2", "PIL", "cairosvg", "reportlab")
	pdf, err := exec.Command(python, "-c", `import io,sys
from reportlab.pdfgen import canvas
b=io.BytesIO();c=canvas.Canvas(b)
for p in [1,2]:
 c.drawString(72,720,'Repair synthetic text page %d unique aurorafencing'%p);c.showPage()
c.save();sys.stdout.buffer.write(b.getvalue())`).Output()
	if err != nil {
		t.Fatal("生成真实 PDF", err)
	}
	adapter, err := filepath.Abs("../../../converters/adapter.py")
	if err != nil {
		t.Fatal(err)
	}
	// macOS 的旧 CLI 在 RLIMIT_AS 设置时失败；不修改 F 所有的 adapter。
	// 此受控子进程加载真实 execute，仅验证 PDF/协议，不声称验证资源沙箱。
	p := &Process{Command: python, Args: []string{"-c", `import importlib.util,json,sys
spec=importlib.util.spec_from_file_location('real_adapter',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
print(json.dumps(m.execute(json.load(open(sys.argv[-1])))))`, adapter}}
	// 可选路径用于合并前直接消费 F 工作树的真实 --execute 输出。
	if path := os.Getenv("DRIVE_TEST_CONVERTER_ADAPTER"); path != "" {
		p.Args = []string{path}
		adapter = path
	}
	in := ports.ConversionRequest{PageReport: true, JobID: "real-pdf", AttemptID: "1", Capability: "document.convert", OutputScope: "tmp/real-pdf/1/", Input: ports.ConversionInput{SourceVersionID: "fixed-real-version", Name: "synthetic.pdf", MIMEType: "application/pdf", ContentBase64: base64.StdEncoding.EncodeToString(pdf), Size: int64(len(pdf))}, Recipe: ports.Recipe{MaxPages: 3, TimeoutSeconds: 10, MaxOutputBytes: 1 << 20}}
	in.Recipe.PageReport = true
	events := 0
	var captured []ports.PageEvent
	result, err := p.ExecutePages(context.Background(), in, func(event ports.PageEvent) error { events++; captured = append(captured, event); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ready" || result.Completeness.TotalPages != 2 || len(result.Pages) != 2 || !strings.Contains(result.Markdown, "page 2 unique") {
		t.Fatal("真实 PDF 漏页", result)
	}
	t.Logf("真实 PDF 文本提取：status=%s pages=%d engine=%s/%s（OCR/视觉 not_run）", result.Status, len(result.Pages), result.Engine.Name, result.Engine.Version)
	t.Logf("真实 adapter=%s，page events=%d", adapter, events)
	// 用同一次真实输出拼接真实页事件和 pretty-print result，验证不按行误判尾部。
	var stream strings.Builder
	if len(captured) == 0 {
		for _, page := range result.Pages {
			captured = append(captured, ports.PageEvent{Type: "page", Page: page.Page, Stage: "text", State: "ready", ResultRef: json.RawMessage(`{}`)})
		}
	}
	for _, event := range captured {
		line, _ := json.Marshal(event)
		stream.Write(line)
		stream.WriteString("\n")
	}
	pretty, _ := json.MarshalIndent(struct {
		Type string `json:"type"`
		ports.ConversionResult
	}{"result", result}, "", "  ")
	stream.Write(pretty)
	decoded := pageResponse{maxPages: 3, report: func(event ports.PageEvent) error { return ValidatePageEvent(event, 3) }}
	if err = decoded.decode(strings.NewReader(stream.String()), 1<<20); err != nil || decoded.result.Markdown != result.Markdown {
		t.Fatal("真实 F/旧 adapter 页事件后多行 result", err)
	}
	t.Logf("pretty-print 完整 result 已消费：行数=%d", strings.Count(stream.String(), "\n")+1)
	in.Recipe.MaxPages = 1
	result, err = p.Execute(context.Background(), in)
	if err != nil && !errors.Is(err, ports.ErrLimit) {
		t.Fatal(err)
	}
	if err == nil && result.Status == "ready" {
		t.Fatal("页数超限被静默截断")
	}
}
