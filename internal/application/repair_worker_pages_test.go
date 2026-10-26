package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	converteradapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/converters"
	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func TestRepairWorkerRealFPageProtocolPersistence(t *testing.T) {
	adapter := os.Getenv("DRIVE_TEST_CONVERTER_ADAPTER")
	if adapter == "" {
		t.Skip("设置 DRIVE_TEST_CONVERTER_ADAPTER 运行跨 F 真实 adapter")
	}
	s, _, queued, _, _ := workerFixture(t)
	job := repairClaim(t, s, queued)
	python := "/tmp/repair-venv/bin/python"
	pdf, err := exec.Command(python, "-c", `import io,sys
from reportlab.pdfgen import canvas
b=io.BytesIO();c=canvas.Canvas(b)
for p in [1,2]:
 c.drawString(72,720,'Repair real page %d unique aurorafencing'%p);c.showPage()
c.save();sys.stdout.buffer.write(b.getvalue())`).Output()
	if err != nil {
		t.Fatal(err)
	}
	s.Converter = &converteradapter.Process{Command: python, Args: []string{adapter}}
	recipe := s.Recipe()
	result, err := s.executeConversionPages(context.Background(), job, ports.ConversionRequest{JobID: uuidText(job.ID), AttemptID: "1", OutputScope: "tmp/" + uuidText(job.ID) + "/1/", Input: ports.ConversionInput{Name: "real.pdf", SourceVersionID: uuidText(job.SourceVersionID), ContentBase64: base64.StdEncoding.EncodeToString(pdf), Size: int64(len(pdf))}, Recipe: recipe})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ready" || result.Completeness.TotalPages != 2 || len(result.Pages) != 2 || !strings.Contains(result.Markdown, "page 1") || !strings.Contains(result.Markdown, "page 2") {
		t.Fatal(result)
	}
	rows, err := dbgen.New(s.DB).ListPageResults(context.Background(), job.ID)
	if err != nil || len(rows) != 4 {
		t.Fatal(len(rows), err)
	}
	t.Log("真实 F PDF JSONL + 原生 PG：四个阶段记录，ready 两页，增量正文完整合并")
}

const repairPageProcess = `import json, sys, time
r=json.load(open(sys.argv[-1]))
log=sys.argv[1]
pages=r.get('resume_pages') or [1,2]
if r['attempt_id']!='1' and pages!=[2]:
    raise RuntimeError('successful page was scheduled again')
for p in pages:
    with open(log,'a') as f: f.write(str(p)+'\n')
    if p==2 and r['attempt_id']=='1':
        time.sleep(10)
    page={'page':p,'block':'p%d-b01'%p,'text_status':'ready','description_status':'not_required'}
    ref={'status':'ready','engine':{'name':'fake-page-process','version':'1'},'markdown':'<!-- page:%d block:p%d-b01 -->\naurorafencing page %d\n'%(p,p,p),'assets':[], 'pages':[page], 'completeness':{'status':'ready','total_pages':2}}
    for stage,state in [('text','ready'),('description','not_required')]:
        print(json.dumps({'type':'page','page':p,'stage':stage,'state':state,'result_ref':ref}),flush=True)
print(json.dumps(dict(ref,type='result')),flush=True)
`

func TestRepairWorkerPageTimeoutPersistsPartialThenResumesAfterRestart(t *testing.T) {
	s, actor, originalJob, original, _ := workerFixture(t)
	ctx := context.Background()
	s.Search = &repairIndex{}
	// 使用非纯文本固定源触发真实 converter 子进程；原件仍由受控 oCIS 替身提供。
	repairSQL(t, s, `UPDATE files SET name='scan.pdf',path='/scan.pdf',content_type='application/pdf' WHERE current_version_id=$1`, originalJob.SourceVersionID)
	// file_versions 不可变，创建新固定源版本，不修改旧历史。
	vid := id()
	repairSQL(t, s, `INSERT INTO file_versions(id,file_id,version_seq,size,sha256,content_type,native_etag,native_revision_ref) SELECT $2,file_id,2,size,sha256,'application/pdf',native_etag,native_revision_ref FROM file_versions WHERE id=$1`, originalJob.SourceVersionID, vid)
	repairSQL(t, s, `UPDATE files SET current_version_id=$2 WHERE current_version_id=$1`, originalJob.SourceVersionID, vid)
	dir := t.TempDir()
	script := filepath.Join(dir, "converter.py")
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(script, []byte(repairPageProcess), 0600); err != nil {
		t.Fatal(err)
	}
	s.Converter = &converteradapter.Process{Command: "/tmp/repair-venv/bin/python", Args: []string{script, log}, MaxResponseBytes: 1 << 20}
	recipe := s.Recipe()
	recipe.TimeoutSeconds = 1
	q := dbgen.New(s.DB)
	job, err := q.RequestConversion(ctx, dbgen.RequestConversionParams{SourceVersionID: pgUUID(vid), RecipeFingerprint: recipe.Fingerprint, RecipeSnapshot: js(recipe)})
	if err != nil {
		t.Fatal(err)
	}
	first := repairProcess(t, s, job)
	if first.State != "partial" || first.PageCount != 2 || first.PagesReady != 1 || first.PagesFailed != 1 {
		t.Fatalf("页超时状态不准确 %+v", first)
	}
	if repairPointer(t, s, job) != uuidText(first.ArtifactSetID) {
		t.Fatal("partial 没有等索引后发布")
	}
	var fid string
	if err = s.DB.QueryRow(ctx, `SELECT file_id::text FROM file_versions WHERE id=$1`, job.SourceVersionID).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	repairVisible(t, s, actor, fid)
	oldRef, err := s.markdown(ctx, actor, Object{"file_id": fid, "artifact_set_id": uuidText(first.ArtifactSetID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("首次真实子进程 timeout 后状态=%s page_count=%d ready=%d failed=%d", first.State, first.PageCount, first.PagesReady, first.PagesFailed)
	s = &Service{DB:s.DB,Config:s.Config,Original:s.Original,Artifacts:s.Artifacts,Identity:s.Identity,Search:s.Search,Converter:s.Converter}
	repairSQL(t, s, `UPDATE conversion_jobs SET retry_at=now() WHERE id=$1`, job.ID)
	second := repairProcess(t, s, job)
	if second.State != "ready" || second.PagesReady != 2 || second.PagesFailed != 0 || second.Attempt != 2 {
		t.Fatalf("续跑未完成 %+v", second)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "1\n2\n2\n" {
		t.Fatal("成功页重复执行", string(calls))
	}
	if second.ArtifactSetID == first.ArtifactSetID {
		t.Fatal("不可变 partial 被覆盖")
	}
	if _, err = s.markdown(ctx, actor, Object{"file_id": fid, "artifact_set_id": uuidText(first.ArtifactSetID)}); err != nil {
		t.Fatal("旧 partial 固定引用失效", oldRef, err)
	}
	if original.reads != 2 {
		t.Fatal("源读取次数", original.reads)
	}
	var complete ports.Completeness
	if err = json.Unmarshal(second.Completeness, &complete); err != nil || !reflect.DeepEqual(complete.MissingTextPages, []int(nil)) || complete.Status != "ready" {
		t.Fatal(complete, err)
	}
	t.Logf("重启后仅重试缺失页 2，调用记录=%q；旧 partial 与完整产物均保留", strings.TrimSpace(string(calls)))
}
