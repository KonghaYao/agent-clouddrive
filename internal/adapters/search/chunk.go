package search

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
	"strings"
	"unicode/utf8"
)

// TextBlock carries provenance from the manifest. Page boundaries are not
// guessed from Markdown; callers provide them from converter page results.
type TextBlock struct {
	Text               string
	Page               int
	BlockID            string
	Headings, AssetIDs []string
}

func Chunk(base ports.SearchDocument, blocks []TextBlock, targetBytes int) ([]ports.SearchDocument, error) {
	if targetBytes < 256 || targetBytes > 16384 {
		return nil, ports.ErrInvalid
	}
	out := []ports.SearchDocument{}
	for _, block := range blocks {
		if !utf8.ValidString(block.Text) {
			return nil, ports.ErrInvalid
		}
		buf := ""
		flush := func() {
			if strings.TrimSpace(buf) == "" {
				buf = ""
				return
			}
			d := base
			d.Content = buf
			d.Page = block.Page
			d.BlockID = block.BlockID
			if d.BlockID == "" {
				d.BlockID = fmt.Sprintf("p%d-b%d", block.Page, len(out)+1)
			}
			d.Headings = append([]string(nil), block.Headings...)
			d.AssetIDs = append([]string(nil), block.AssetIDs...)
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", d.FileID, d.VersionID, d.ArtifactSetID, len(out))))
			d.ID = hex.EncodeToString(sum[:])
			out = append(out, d)
			buf = ""
		}
		// Scan one paragraph at a time. Splitting a large file containing only
		// separators would allocate millions of slices before yielding any chunk.
		remaining := block.Text
		for {
			p, rest, more := strings.Cut(remaining, "\n\n")
			remaining = rest
			if len(buf) > 0 && len(buf)+2+len(p) > targetBytes {
				flush()
			}
			for len(p) > targetBytes {
				if buf != "" {
					flush()
				}
				n := targetBytes
				for n > 0 && !utf8.RuneStart(p[n]) {
					n--
				}
				buf = p[:n]
				flush()
				p = p[n:]
			}
			if p != "" {
				if buf != "" {
					buf += "\n\n"
				}
				buf += p
			}
			if !more {
				break
			}
		}
		flush()
	}
	return out, nil
}
