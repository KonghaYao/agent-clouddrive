package core

import (
	"github.com/agentclouddrive/agent-clouddrive/internal/config"
	"github.com/agentclouddrive/agent-clouddrive/internal/ports"
)

func Recipe(cfg config.Config) ports.Recipe {
	r := ports.Recipe{Engine: "anydoc", EngineVersion: cfg.PipelineVersion, Model: cfg.VisionModel, DescribeImages: true, TimeoutSeconds: 300, MaxPages: cfg.ConvertPages, MaxOutputBytes: 32 << 20, PromptVersion: "image-description-v1", MarkdownVersion: "v1", ChunkVersion: "v1", Config: map[string]any{"convert_max_bytes": cfg.ConvertMax, "ocr_model_manifest_sha256": cfg.OCRModelManifest, "vision_provider": cfg.VisionProvider}}
	r.Fingerprint = "sha256:" + Hash(r)
	return r
}
