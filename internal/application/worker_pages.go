package application

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	converteradapter "github.com/agentclouddrive/agent-clouddrive/internal/adapters/converters"
	"github.com/agentclouddrive/agent-clouddrive/internal/dbgen"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func stringField(raw []byte, key string) string {
	var fields map[string]json.RawMessage
	var value string
	_ = json.Unmarshal(raw, &fields)
	_ = json.Unmarshal(fields[key], &value)
	return value
}

// result_ref 内联保存页 bytes，不依赖 converter 的临时目录。文档总页数必须在首个事件
// 中声明，才能在进程被杀后精确调度缺失页，不能以“已收到的最大页号”猜测总页数。
type conversionProgress struct {
	total     int
	pages     map[int]ports.ConversionResult
	stages    map[int]map[string]string
	attempts  map[int]int32
	preserved map[int]bool
}

func (s *Service) loadConversionProgress(ctx context.Context, job dbgen.ConversionJob) (*conversionProgress, error) {
	rows, err := dbgen.New(s.DB).ListPageResults(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	p := &conversionProgress{total: int(job.PageCount), pages: map[int]ports.ConversionResult{}, stages: map[int]map[string]string{}, attempts: map[int]int32{}, preserved: map[int]bool{}}
	for _, row := range rows {
		page := int(row.PageNo)
		if p.attempts[page] != 0 && row.Attempt != p.attempts[page] {
			continue
		}
		if p.stages[page] == nil {
			p.stages[page] = map[string]string{}
		}
		if _, ok := p.stages[page][row.Stage]; ok {
			continue
		}
		p.stages[page][row.Stage] = row.State
		var ref ports.ConversionResult
		if json.Unmarshal(row.ResultRef, &ref) != nil {
			return nil, ports.ErrIntegrity
		}
		if ref.Completeness.TotalPages > p.total {
			p.total = ref.Completeness.TotalPages
		}
		if len(ref.Pages) > 0 && p.attempts[page] == 0 {
			p.pages[page] = ref
			p.attempts[page] = row.Attempt
		}
	}
	for page, states := range p.stages {
		p.preserved[page] = pageReady(states) && len(p.pages[page].Pages) > 0
	}
	return p, nil
}

func normalizePageRef(event ports.PageEvent) (ports.ConversionResult, error) {
	var ref ports.ConversionResult
	if json.Unmarshal(event.ResultRef, &ref) != nil {
		return ref, ports.ErrIntegrity
	}
	var meta struct {
		TotalPages int    `json:"total_pages"`
		Block      string `json:"block"`
	}
	_ = json.Unmarshal(event.ResultRef, &meta)
	if ref.Completeness.TotalPages == 0 {
		ref.Completeness.TotalPages = meta.TotalPages
	}
	if ref.Completeness.TotalPages != 0 && ref.Completeness.TotalPages < event.Page {
		return ref, ports.ErrIntegrity
	}
	if len(ref.Pages) == 0 {
		ref.Pages = []ports.ConversionPage{{Page: event.Page, Block: meta.Block}}
	}
	if len(ref.Pages) != 1 || ref.Pages[0].Page != event.Page {
		return ref, ports.ErrIntegrity
	}
	if ref.Pages[0].Block == "" {
		ref.Pages[0].Block = "p" + strconv.Itoa(event.Page) + "-b01"
	}
	if ref.Engine.Model == "" {
		ref.Engine.Model = reportedModel(ref.Model)
	}
	var metadata ports.EngineInfo
	_ = json.Unmarshal(event.ResultRef, &metadata)
	if ref.Engine.Model == "" {
		ref.Engine.Model = metadata.Model
	}
	if ref.Engine.Provider == "" {
		ref.Engine.Provider = metadata.Provider
	}
	return ref, nil
}

func (s *Service) persistConversionPage(ctx context.Context, job dbgen.ConversionJob, in ports.ConversionRequest, p *conversionProgress, event ports.PageEvent) error {
	if err := converteradapter.ValidatePageEvent(event, in.Recipe.MaxPages); err != nil {
		return err
	}
	if p.preserved[event.Page] {
		return nil
	}
	ref, err := normalizePageRef(event)
	if err != nil {
		return err
	}
	if ref.Completeness.TotalPages > in.Recipe.MaxPages {
		return ports.ErrLimit
	}
	if ref.Completeness.TotalPages != 0 && p.total != 0 && p.total != ref.Completeness.TotalPages {
		return ports.ErrIntegrity
	}
	if ref.Completeness.TotalPages > 0 {
		p.total = ref.Completeness.TotalPages
	}
	if in.Recipe.Model != "" && ref.Engine.Model != "" && ref.Engine.Model != in.Recipe.Model {
		return &conversionFailure{"blocked", "model_unavailable", false}
	}
	// v2 result_ref 是阶段增量；PG 存储的则是累计页快照。首次重试失败页时
	// 重建该页快照，避免重复渲染/OCR片段与资产；已成功页根本不会进入该分支。
	previous := p.pages[event.Page]
	if p.attempts[event.Page] != job.Attempt {
		previous = ports.ConversionResult{}
		p.attempts[event.Page] = job.Attempt
		p.stages[event.Page] = map[string]string{}
	}
	if ref.Status != "" {
		// 兼容旧完整页快照与最终结果拆页。
		if ref.Markdown == "" {
			ref.Markdown = previous.Markdown
		}
	} else {
		ref.Markdown = previous.Markdown + ref.Markdown
	}
	assets := map[string]bool{}
	for _, asset := range ref.Assets {
		assets[asset.ID] = true
	}
	for _, asset := range previous.Assets {
		if !assets[asset.ID] {
			ref.Assets = append(ref.Assets, asset)
		}
	}
	if ref.Engine.Name == "" {
		ref.Engine.Name = previous.Engine.Name
		ref.Engine.Version = previous.Engine.Version
	}
	if ref.Engine.Model == "" {
		ref.Engine.Model = previous.Engine.Model
	}
	if ref.Engine.Provider == "" {
		ref.Engine.Provider = previous.Engine.Provider
	}
	if int64(len(js(ref))) > in.Recipe.MaxOutputBytes {
		return ports.ErrLimit
	}
	var code *string
	if event.Error != nil {
		code = &event.Error.Code
	}
	_, err = dbgen.New(s.DB).SavePageResult(ctx, dbgen.SavePageResultParams{JobID: job.ID, PageNo: int32(event.Page), Stage: event.Stage, Attempt: job.Attempt, LeaseToken: job.LeaseToken, State: event.State, ResultRef: js(ref), ErrorCode: code})
	if err != nil {
		return err
	}
	if p.stages[event.Page] == nil {
		p.stages[event.Page] = map[string]string{}
	}
	p.stages[event.Page][event.Stage] = event.State
	p.pages[event.Page] = ref
	return s.saveConversionProgress(ctx, job, p)
}

func (s *Service) saveConversionProgress(ctx context.Context, job dbgen.ConversionJob, p *conversionProgress) error {
	result, ready := p.result(ports.EngineInfo{})
	n, err := s.DB.Exec(ctx, `UPDATE conversion_jobs SET page_count=$3,pages_ready=$4,pages_failed=$3::integer-$4::integer,completeness=$5,actual_engine=$6
	WHERE id=$1 AND lease_token=$2 AND state='processing' AND lease_until>now()`, job.ID, job.LeaseToken, p.total, ready, js(result.Completeness), js(result.Engine))
	if err != nil {
		return err
	}
	if n.RowsAffected() != 1 {
		return ports.ErrConflict
	}
	return nil
}

func (s *Service) executeConversionPages(ctx context.Context, job dbgen.ConversionJob, in ports.ConversionRequest) (ports.ConversionResult, error) {
	progress, err := s.loadConversionProgress(ctx, job)
	if err != nil {
		return ports.ConversionResult{}, err
	}
	if progress.total > in.Recipe.MaxPages {
		return ports.ConversionResult{}, ports.ErrLimit
	}
	in.PageReport = true
	if progress.total == 0 && len(progress.pages) > 0 {
		// 旧 v2 producer 在首事件未声明总页数：保存了证据，但不能安全枚举缺页。
		return ports.ConversionResult{}, &conversionFailure{"blocked", "page_metadata_missing", false}
	}
	for page := 1; page <= progress.total; page++ {
		if !progress.preserved[page] {
			in.ResumePages = append(in.ResumePages, page)
		}
	}
	in.Recipe.PageReport = true
	in.Recipe.ResumePages = in.ResumePages
	persist := func(event ports.PageEvent) error { return s.persistConversionPage(ctx, job, in, progress, event) }
	var result ports.ConversionResult
	if progress.total == 0 || len(in.ResumePages) > 0 {
		execCtx, cancel := context.WithTimeout(ctx, time.Duration(in.Recipe.TimeoutSeconds)*time.Second)
		if converter, ok := s.Converter.(ports.PageConverter); ok {
			result, err = converter.ExecutePages(execCtx, in, persist)
		} else {
			result, err = s.Converter.Execute(execCtx, in)
		}
		cancel()
	}
	if errors.Is(err, ports.ErrLimit) || errors.Is(err, ports.ErrIntegrity) || errors.Is(err, ports.ErrConflict) {
		return result, err
	}
	var failure *conversionFailure
	if errors.As(err, &failure) {
		return result, err
	}
	if result.Completeness.TotalPages > in.Recipe.MaxPages {
		return result, ports.ErrLimit
	}
	if result.Completeness.TotalPages > 0 {
		progress.total = result.Completeness.TotalPages
	}
	if result.Status != "" && result.Status != "ready" && result.Status != "partial" {
		for _, failure := range result.Errors {
			if failure.Code == "limit_exceeded" {
				return result, ports.ErrLimit
			}
		}
		if result.Status == "blocked" || len(progress.pages) == 0 {
			return result, err
		}
	}
	// 兼容旧单 JSON：最终结果按页拆开。已成功页不覆盖，失败页允许下一 attempt 补全。
	if err == nil && len(result.Pages) > 0 {
		if err = s.persistFinalPages(result, progress, persist); err != nil {
			return result, err
		}
	}
	if len(progress.pages) > 0 && progress.total == 0 {
		return result, &conversionFailure{"blocked", "page_metadata_missing", false}
	}
	if len(progress.pages) == 0 || progress.total == 0 {
		return result, err
	}
	if result.Engine.Model == "" {
		result.Engine.Model = reportedModel(result.Model)
	}
	engine := result.Engine
	if result.Status != "ready" && result.Status != "partial" {
		engine = ports.EngineInfo{}
	}
	merged, _ := progress.result(engine)
	if strings.TrimSpace(merged.Markdown) == "" && len(merged.Assets) == 0 {
		return result, err
	}
	if in.Recipe.Model != "" && merged.Engine.Model != in.Recipe.Model {
		return result, &conversionFailure{"blocked", "model_unavailable", false}
	}
	if err = converteradapter.ValidateResult(merged, in.Recipe.MaxPages, in.Recipe.MaxOutputBytes); err != nil {
		return result, err
	}
	if err = s.saveConversionProgress(ctx, job, progress); err != nil {
		return result, err
	}
	return merged, nil
}

func (s *Service) persistFinalPages(result ports.ConversionResult, p *conversionProgress, persist func(ports.PageEvent) error) error {
	for _, page := range result.Pages {
		if p.preserved[page.Page] {
			continue
		}
		ref := pageSlice(result, page)
		for _, stage := range []struct{ name, state string }{{"text", page.TextStatus}, {"description", page.DescriptionStatus}} {
			state := stage.state
			if !stageReady(state) {
				state = "failed"
			}
			event := ports.PageEvent{Type: "page", Page: page.Page, Stage: stage.name, State: state, ResultRef: js(ref)}
			if state == "failed" {
				event.Error = &ports.ConversionError{Code: "stage_failed", Retryable: true}
			}
			if err := persist(event); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *conversionProgress) result(engine ports.EngineInfo) (ports.ConversionResult, int) {
	out := ports.ConversionResult{Status: "ready", Engine: engine, Completeness: ports.Completeness{Status: "ready", TotalPages: p.total}}
	keys := make([]int, 0, len(p.pages))
	for page := range p.pages {
		keys = append(keys, page)
	}
	sort.Ints(keys)
	for _, page := range keys {
		ref := p.pages[page]
		if out.Engine.Name == "" {
			out.Engine = ref.Engine
		}
		out.Markdown += ref.Markdown + "\n"
		out.Assets = append(out.Assets, ref.Assets...)
		block := "p" + strconv.Itoa(page) + "-b01"
		if len(ref.Pages) > 0 {
			block = ref.Pages[0].Block
		}
		out.Pages = append(out.Pages, ports.ConversionPage{Page: page, Block: block, TextStatus: p.stages[page]["text"], DescriptionStatus: p.stages[page]["description"]})
	}
	ready := 0
	for page := 1; page <= p.total; page++ {
		if !stageReady(p.stages[page]["text"]) {
			out.Completeness.MissingTextPages = append(out.Completeness.MissingTextPages, page)
		}
		if !stageReady(p.stages[page]["description"]) {
			out.Completeness.MissingDescriptionPages = append(out.Completeness.MissingDescriptionPages, page)
		}
		if pageReady(p.stages[page]) {
			ready++
		}
	}
	if ready < p.total {
		out.Status = "partial"
		out.Completeness.Status = "partial"
	}
	return out, ready
}

func pageSlice(result ports.ConversionResult, page ports.ConversionPage) ports.ConversionResult {
	out := ports.ConversionResult{Status: result.Status, Engine: result.Engine, Model: result.Model, Completeness: result.Completeness, Pages: []ports.ConversionPage{page}}
	for _, block := range conversionBlocks(result.Markdown, conversionManifest{Pages: result.Pages, Completeness: result.Completeness}) {
		if block.Page == page.Page {
			out.Markdown += "<!-- page:" + strconv.Itoa(page.Page) + " block:" + block.BlockID + " -->\n" + block.Text + "\n"
		}
	}
	if len(result.Pages) == 1 && out.Markdown == "" {
		out.Markdown = result.Markdown
	}
	for _, asset := range result.Assets {
		if asset.Page == page.Page {
			out.Assets = append(out.Assets, asset)
		}
	}
	return out
}
func reportedModel(raw json.RawMessage) string {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name
	}
	return stringField(raw, "name")
}

func stageReady(state string) bool { return state == "ready" || state == "not_required" }
func pageReady(stages map[string]string) bool {
	return stageReady(stages["text"]) && stageReady(stages["description"])
}
