package llm

import (
	"encoding/base64"
	"testing"
)

func TestGeminiOutputMediaPart(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("fake-png-bytes"))

	p, ok := geminiOutputMediaPart(map[string]any{
		"type":      "image",
		"mime_type": "image/png",
		"data":      b64,
	})
	if !ok {
		t.Fatal("expected image output media part")
	}
	if p.Type != "image" || p.MIME != "image/png" || p.Data != b64 {
		t.Fatalf("unexpected image part: %+v", p)
	}

	doc, ok := geminiOutputMediaPart(map[string]any{
		"type":      "document",
		"mime_type": "application/pdf",
		"uri":       "https://example.com/report.pdf",
	})
	if !ok {
		t.Fatal("expected document output media part")
	}
	if doc.Type != "file" || doc.MIME != "application/pdf" || doc.URL != "https://example.com/report.pdf" {
		t.Fatalf("unexpected doc part: %+v", doc)
	}

	if _, ok := geminiOutputMediaPart(map[string]any{"type": "text", "text": "hello"}); ok {
		t.Fatal("text part must not be extracted as media")
	}
	if _, ok := geminiOutputMediaPart(map[string]any{"type": "image", "mime_type": "image/png"}); ok {
		t.Fatal("empty image part without data/uri must be ignored")
	}
}

func TestResponsesImagePart(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("generated-webp"))
	raw := `{"type":"image_generation_call","result":"` + b64 + `","output_format":"webp"}`

	p, ok := responsesImagePart(raw)
	if !ok {
		t.Fatal("expected responses image part")
	}
	if p.Type != "image" || p.MIME != "image/webp" || p.Data != b64 {
		t.Fatalf("unexpected responses image part: %+v", p)
	}

	rawDefault := `{"type":"image_generation_call","result":"` + b64 + `"}`
	pDef, ok := responsesImagePart(rawDefault)
	if !ok || pDef.MIME != "image/png" {
		t.Fatalf("expected default image/png, got ok=%v part=%+v", ok, pDef)
	}

	rawOther := `{"type":"message","result":"` + b64 + `"}`
	if _, ok := responsesImagePart(rawOther); ok {
		t.Fatal("non-image_generation_call item must be ignored")
	}
}

func TestOpenAIMediaExtraction(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("raw-image"))
	dataURI := "data:image/jpeg;base64," + b64

	whole := `{
		"choices": [{
			"message": {
				"content": "Here is your image",
				"images": [
					{"image_url": {"url": "` + dataURI + `"}},
					{"b64_json": "` + b64 + `", "mime_type": "image/png"}
				],
				"audio": {"data": "` + b64 + `", "format": "mp3"}
			}
		}]
	}`
	parts := openaiWholeMedia(whole)
	if len(parts) != 3 {
		t.Fatalf("expected 3 media parts from whole completion, got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != "image" || parts[0].MIME != "image/jpeg" || parts[0].Data != b64 {
		t.Fatalf("unexpected part[0]: %+v", parts[0])
	}
	if parts[1].Type != "image" || parts[1].MIME != "image/png" || parts[1].Data != b64 {
		t.Fatalf("unexpected part[1]: %+v", parts[1])
	}
	if parts[2].Type != "audio" || parts[2].MIME != "audio/mpeg" || parts[2].Data != b64 {
		t.Fatalf("unexpected part[2]: %+v", parts[2])
	}

	chunk := `{
		"choices": [{
			"delta": {
				"images": [{"url": "https://cdn.example.com/pic.png"}],
				"audio": {"data": "` + b64 + `"}
			}
		}]
	}`
	cparts := openaiChunkMedia(chunk)
	if len(cparts) != 2 {
		t.Fatalf("expected 2 media parts from chunk delta, got %d: %+v", len(cparts), cparts)
	}
	if cparts[0].URL != "https://cdn.example.com/pic.png" {
		t.Fatalf("unexpected chunk part[0]: %+v", cparts[0])
	}
	if cparts[1].Type != "audio" || cparts[1].MIME != "audio/wav" {
		t.Fatalf("unexpected chunk part[1]: %+v", cparts[1])
	}
}
