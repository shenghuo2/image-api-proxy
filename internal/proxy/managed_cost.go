package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"strings"
)

type jobCost struct {
	Full         int64
	Extras       int64
	OpusEligible bool
	V5           bool
	MultiImage   bool
	Samples      int
}

func estimateJob(path string, body []byte, contentType string) (jobCost, error) {
	request, err := jobRequestJSON(body, contentType)
	if err != nil || !json.Valid(request) {
		return jobCost{}, errors.New("invalid JSON")
	}
	switch path {
	case "/ai/encode-vibe", "/image/ai/encode-vibe":
		return jobCost{Full: 2}, nil
	case "/ai/upscale", "/image/ai/upscale":
		return jobCost{Full: 200}, nil
	case "/ai/augment-image", "/image/ai/augment-image":
		var payload struct {
			Image  string `json:"image"`
			Type   string `json:"req_type"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		}
		if json.Unmarshal(request, &payload) != nil || payload.Image == "" || payload.Width < 64 || payload.Height < 64 || payload.Width > 2048 || payload.Height > 2048 {
			return jobCost{}, errors.New("unsupported augmentation dimensions")
		}
		switch payload.Type {
		case "emotion", "bg-removal", "colorize", "declutter", "lineart", "sketch":
		default:
			return jobCost{}, errors.New("unsupported augmentation type")
		}
		pixels := float64(max(65536, payload.Width*payload.Height))
		base := math.Max(math.Ceil(2.951823174884865e-6*pixels+5.753298233447344e-7*pixels*28), 2)
		if payload.Type == "bg-removal" {
			base = base*3 + 5
		}
		return jobCost{Full: int64(math.Ceil(base*1.2)) + 5}, nil
	case "/ai/generate-image", "/ai/generate-image-stream", "/image/ai/generate-image", "/image/ai/generate-image-stream":
	default:
		return jobCost{}, errors.New("unsupported route")
	}
	var payload struct {
		Model      string `json:"model"`
		Parameters struct {
			Width            int               `json:"width"`
			Height           int               `json:"height"`
			Steps            int               `json:"steps"`
			Samples          int               `json:"n_samples"`
			Vibes            []json.RawMessage `json:"reference_image_multiple"`
			VibesCached      []json.RawMessage `json:"reference_image_multiple_cached"`
			References       []json.RawMessage `json:"director_reference_images"`
			ReferencesCached []json.RawMessage `json:"director_reference_images_cached"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(request, &payload); err != nil {
		return jobCost{}, err
	}
	p := payload.Parameters
	vibeCount := len(p.Vibes) + len(p.VibesCached)
	referenceCount := len(p.References) + len(p.ReferencesCached)
	if !allowedGenerationModel(payload.Model) || p.Width < 64 || p.Height < 64 || p.Width > 2048 || p.Height > 2048 || p.Steps < 1 || p.Steps > 50 || p.Samples < 1 || p.Samples > 4 || vibeCount > 16 || referenceCount > 10 {
		return jobCost{}, errors.New("unsupported generation dimensions")
	}
	pixels := float64(p.Width * p.Height)
	base := math.Ceil(2.951823174884865e-6*pixels + 5.753298233447344e-7*pixels*float64(p.Steps))
	v5 := strings.HasPrefix(payload.Model, "nai-diffusion-5")
	if v5 {
		base = math.Ceil(base * 1.5)
	}
	extra := max(0, vibeCount-4)*2 + referenceCount*5
	extraHold := int64(0)
	if extra > 0 {
		extraHold = int64(math.Ceil(float64(extra)*1.2)) + 2
	}
	singleImageHold := int64(math.Ceil((base+float64(extra))*1.2)) + 5
	return jobCost{
		Full:         singleImageHold * int64(p.Samples),
		Extras:       extraHold,
		OpusEligible: p.Samples == 1 && p.Steps <= 28 && p.Width*p.Height <= 1048576,
		V5:           v5,
		MultiImage:   p.Samples > 1,
		Samples:      p.Samples,
	}, nil
}

func jobRequestJSON(body []byte, contentType string) ([]byte, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/") {
		return body, nil
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, errors.New("invalid multipart content type")
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var request []byte
	found := false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if part.FormName() != "request" {
			continue
		}
		if found {
			return nil, errors.New("duplicate request part")
		}
		found = true
		request, err = io.ReadAll(part)
		if err != nil {
			return nil, err
		}
	}
	if !found {
		return nil, errors.New("request part missing")
	}
	return request, nil
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
