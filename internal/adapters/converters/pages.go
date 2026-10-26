package converters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

type pageResponse struct {
	result   ports.ConversionResult
	report   func(ports.PageEvent) error
	maxPages int
}

func (r *pageResponse) decode(reader io.Reader, maxBytes int64) error {
	bounded := &io.LimitedReader{R: reader, N: maxBytes + 1}
	decoder := json.NewDecoder(bounded)
	finished := false
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if bounded.N <= 0 {
			return ports.ErrLimit
		}
		if errors.Is(err, io.EOF) {
			if finished {
				return nil
			}
			return ports.ErrUnavailable
		}
		if err != nil {
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				return ports.ErrIntegrity
			}
			return ports.ErrUnavailable
		}
		if finished {
			return ports.ErrIntegrity
		}
		var event ports.PageEvent
		if json.Unmarshal(raw, &event) != nil {
			return ports.ErrIntegrity
		}
		switch event.Type {
		case "page":
			if err = ValidatePageEvent(event, r.maxPages); err != nil {
				return err
			}
			if r.report != nil {
				if err = r.report(event); err != nil {
					return err
				}
			}
		case "result", "":
			if json.Unmarshal(raw, &r.result) != nil {
				return ports.ErrIntegrity
			}
			finished = true
		default:
			return ports.ErrIntegrity
		}
	}
}

func ValidatePageEvent(event ports.PageEvent, maxPages int) error {
	if event.Page < 1 || event.Page > maxPages {
		return ports.ErrLimit
	}
	switch event.Stage {
	case "render", "text", "ocr", "description":
	default:
		return ports.ErrIntegrity
	}
	switch event.State {
	case "ready", "not_required":
	case "failed":
		if event.Error == nil || event.Error.Code == "" {
			return ports.ErrIntegrity
		}
	default:
		return ports.ErrIntegrity
	}
	if len(event.ResultRef) == 0 || !json.Valid(event.ResultRef) {
		return ports.ErrIntegrity
	}
	return nil
}

// Process 为受信本地 adapter 提供与 HTTP 相同的增量协议；执行路径只能由部署者配置。
type Process struct {
	Command          string
	Args             []string
	MaxResponseBytes int64
}

func (p *Process) Capabilities(context.Context) (ports.Capabilities, error) {
	return ports.Capabilities{}, ports.ErrUnavailable
}
func (p *Process) Execute(ctx context.Context, in ports.ConversionRequest) (ports.ConversionResult, error) {
	return p.ExecutePages(ctx, in, nil)
}
func (p *Process) ExecutePages(ctx context.Context, in ports.ConversionRequest, report func(ports.PageEvent) error) (ports.ConversionResult, error) {
	dir, err := os.MkdirTemp("", "drive-conversion-")
	if err != nil {
		return ports.ConversionResult{}, err
	}
	defer os.RemoveAll(dir)
	body, err := json.Marshal(in)
	if err != nil {
		return ports.ConversionResult{}, err
	}
	input := filepath.Join(dir, "input.json")
	if err = os.WriteFile(input, body, 0600); err != nil {
		return ports.ConversionResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(in.Recipe.TimeoutSeconds)*time.Second)
	defer cancel()
	args := append(append([]string{}, p.Args...), "--execute", input)
	cmd := exec.CommandContext(ctx, p.Command, args...)
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ports.ConversionResult{}, err
	}
	if err = cmd.Start(); err != nil {
		return ports.ConversionResult{}, ports.ErrUnavailable
	}
	limit := p.MaxResponseBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	response := pageResponse{report: report, maxPages: in.Recipe.MaxPages}
	err = response.decode(stdout, limit)
	if err != nil && ctx.Err() == nil {
		cancel()
		_ = cmd.Wait()
		return response.result, err
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return response.result, ctx.Err()
	}
	if err != nil {
		return response.result, err
	}
	if waitErr != nil {
		return response.result, ports.ErrUnavailable
	}
	return response.result, ValidateResult(response.result, in.Recipe.MaxPages, limit)
}

var _ ports.PageConverter = (*HTTP)(nil)
var _ ports.PageConverter = (*Process)(nil)
