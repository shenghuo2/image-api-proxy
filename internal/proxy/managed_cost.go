package proxy

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

type jobCost struct {
	Full         int64
	Extras       int64
	OpusEligible bool
	V5           bool
}

func estimateJob(path string, body []byte) (jobCost, error) {
	if !json.Valid(body) {
		return jobCost{}, errors.New("invalid JSON")
	}
	switch path {
	case "/ai/encode-vibe", "/image/ai/encode-vibe":
		return jobCost{Full: 2}, nil
	case "/ai/upscale", "/image/ai/upscale":
		return jobCost{Full: 200}, nil
	case "/ai/generate-image", "/ai/generate-image-stream", "/image/ai/generate-image", "/image/ai/generate-image-stream":
	default:
		return jobCost{}, errors.New("unsupported route")
	}
	var payload struct {
		Model      string `json:"model"`
		Parameters struct {
			Width      int               `json:"width"`
			Height     int               `json:"height"`
			Steps      int               `json:"steps"`
			Samples    int               `json:"n_samples"`
			Vibes      []json.RawMessage `json:"reference_image_multiple"`
			References []json.RawMessage `json:"director_reference_images"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return jobCost{}, err
	}
	p := payload.Parameters
	if !allowedGenerationModel(payload.Model) || p.Width < 64 || p.Height < 64 || p.Width > 2048 || p.Height > 2048 || p.Steps < 1 || p.Steps > 50 || p.Samples != 1 || len(p.Vibes) > 16 || len(p.References) > 10 {
		return jobCost{}, errors.New("unsupported generation dimensions")
	}
	pixels := float64(p.Width * p.Height)
	base := math.Ceil(2.951823174884865e-6*pixels + 5.753298233447344e-7*pixels*float64(p.Steps))
	v5 := strings.HasPrefix(payload.Model, "nai-diffusion-5")
	if v5 {
		base = math.Ceil(base * 1.5)
	}
	extra := max(0, len(p.Vibes)-4)*2 + len(p.References)*5
	extraHold := int64(0)
	if extra > 0 {
		extraHold = int64(math.Ceil(float64(extra)*1.2)) + 2
	}
	return jobCost{
		Full:         int64(math.Ceil((base+float64(extra))*1.2)) + 5,
		Extras:       extraHold,
		OpusEligible: p.Steps <= 28 && p.Width*p.Height <= 1048576,
		V5:           v5,
	}, nil
}

func allowedGenerationModel(model string) bool {
	switch model {
	case "nai-diffusion-3", "nai-diffusion-3-inpainting",
		"nai-diffusion-4-full", "nai-diffusion-4-full-inpainting",
		"nai-diffusion-4-curated-preview", "nai-diffusion-4-curated-preview-inpainting",
		"nai-diffusion-4-5-full", "nai-diffusion-4-5-full-inpainting",
		"nai-diffusion-4-5-curated", "nai-diffusion-4-5-curated-inpainting",
		"nai-diffusion-5-full", "nai-diffusion-5-full-inpainting",
		"nai-diffusion-5-curated":
		return true
	default:
		return false
	}
}
