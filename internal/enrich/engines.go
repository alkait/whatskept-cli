package enrich

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// Model defaults, ported from the original repo (validated on real
// Arabic/English WhatsApp archives).
const (
	// ImageModel is a cheap, strong multilingual-OCR vision model.
	ImageModel = "qwen/qwen3-vl-8b-instruct"
	// VoiceModel must be an audio-input model.
	VoiceModel = "google/gemini-2.5-flash"
	// DocumentModel only acks — the text comes from the file-parser
	// plugin, so the cheapest capable model wins.
	DocumentModel = "google/gemini-2.5-flash-lite"
)

const (
	imageMaxOutputTokens = 700
	voiceMaxOutputTokens = 8192 // a 10-minute voice note transcribes to thousands of tokens
	docMaxOutputTokens   = 8    // the reply is a no-op ack; text arrives via annotations

	// imagePrompt forces a required, separated transcription so the
	// model can't quietly summarize away the OCR — the literal
	// numbers/names/dates are the searchable value of an archive.
	imagePrompt = "You are indexing a personal chat archive. Output EXACTLY two labeled " +
		"sections and nothing else:\n\n" +
		"TEXT: Transcribe every piece of visible text VERBATIM, in its original " +
		"language and script (Arabic, English, digits, @handles, prices, dates). " +
		"Preserve reading/line order. Do NOT translate or summarize. If the image " +
		"has no readable text, write 'none'.\n\n" +
		"DESCRIPTION: 1-2 factual sentences describing the scene, people, and objects."

	// voicePrompt asks for a verbatim, translation-free transcript in
	// the original language (chats mix Arabic / English).
	voicePrompt = "Transcribe this voice message verbatim. Output ONLY the " +
		"transcription text in its original language (Arabic, English, etc.) with no " +
		"translation, no commentary, and no quotation marks. If there is no speech, output nothing."

	docPrompt = "Reply with the single word: ok"

	// file-parser engines: native text layer first (free), OCR for
	// scanned documents.
	enginePDFText    = "pdf-text"
	engineMistralOCR = "mistral-ocr"

	// docEscalateBelow: fewer non-space content characters than this
	// from pdf-text means the PDF is scanned — escalate to OCR.
	docEscalateBelow = 20

	// PDFs over docInlineMaxBytes are split before OCR (the provider
	// rejects ~30 MB documents); chunks aim at docChunkTargetBytes.
	docInlineMaxBytes   = 20 << 20
	docChunkTargetBytes = 12 << 20
)

// imageResult is one described image.
type imageResult struct {
	OCRText     string
	Description string
}

// describeImage runs one vision call and splits the reply into verbatim
// OCR and a short description. The USD cost is returned even alongside
// an error so the run's accounting never loses spend.
func describeImage(ctx context.Context, c *Client, ext string, data []byte) (imageResult, float64, error) {
	dataURI := "data:" + mimeForImageExt(ext) + ";base64," + base64.StdEncoding.EncodeToString(data)
	cr, err := c.complete(ctx, ImageModel, []contentPart{
		{Type: "text", Text: imagePrompt},
		{Type: "image_url", ImageURL: &imageURLPart{URL: dataURI}},
	}, nil, imageMaxOutputTokens, 0.1)
	if err != nil {
		return imageResult{}, cr.cost(), err
	}
	ocr, desc := splitTextDescription(cr.Choices[0].Message.Content)
	return imageResult{OCRText: ocr, Description: desc}, cr.cost(), nil
}

// transcribeVoice sends the raw Ogg/Opus bytes and returns the
// transcript and USD cost. Empty is a valid result (a silent clip).
func transcribeVoice(ctx context.Context, c *Client, opus []byte) (string, float64, error) {
	cr, err := c.complete(ctx, VoiceModel, []contentPart{
		{Type: "text", Text: voicePrompt},
		{Type: "input_audio", InputAudio: &inputAudioPart{
			Data: base64.StdEncoding.EncodeToString(opus), Format: "ogg"}},
	}, nil, voiceMaxOutputTokens, 0)
	if err != nil {
		return "", cr.cost(), err
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), cr.cost(), nil
}

// docResult is one extracted document.
type docResult struct {
	Text      string
	PageCount int
	Method    string // "cloud-text", "cloud-ocr", or "empty"
}

// extractDocument pulls a PDF's text: native text layer first (free),
// OCR when the document is scanned, split-into-page-ranges when it is
// too large for one request. The accumulated USD cost across every
// call is returned even alongside an error, so accounting never loses
// spend from a multi-call document.
func extractDocument(ctx context.Context, c *Client, filename string, pdf []byte) (docResult, float64, error) {
	pages := docPageCount(pdf) // best-effort; 0 if unreadable

	if len(pdf) > docInlineMaxBytes {
		return extractSplit(ctx, c, pdf, filename, pages, 0)
	}

	raw, textCost, err := parseOnce(ctx, c, pdf, filename, enginePDFText)
	cost := textCost
	if err == nil && len(stripPDFNoise(raw)) >= docEscalateBelow {
		return docResult{Text: cleanParsed(raw), Method: "cloud-text", PageCount: pages}, cost, nil
	}

	ocrRaw, ocrCost, ocrErr := parseOnce(ctx, c, pdf, filename, engineMistralOCR)
	cost += ocrCost
	if ocrErr != nil {
		var tooLarge *errTooLarge
		if errors.As(ocrErr, &tooLarge) {
			return extractSplit(ctx, c, pdf, filename, pages, cost)
		}
		// OCR failed but pdf-text returned some text — keep it rather
		// than failing the item.
		if err == nil {
			if t := cleanParsed(raw); t != "" {
				return docResult{Text: t, Method: "cloud-text", PageCount: pages}, cost, nil
			}
		}
		return docResult{}, cost, ocrErr
	}
	cleaned := cleanParsed(ocrRaw)
	if cleaned == "" {
		return docResult{Method: "empty", PageCount: pages}, cost, nil
	}
	return docResult{Text: cleaned, Method: "cloud-ocr", PageCount: pages}, cost, nil
}

// extractSplit OCRs an oversized PDF in page-range chunks and stitches
// the text back in page order. priorCost is spend already incurred on
// this document before splitting.
func extractSplit(ctx context.Context, c *Client, pdf []byte, filename string, pages int, priorCost float64) (docResult, float64, error) {
	cost := priorCost
	chunks, err := splitPDF(pdf, pages)
	if err != nil {
		return docResult{}, cost, &Error{Class: ClassPermanent, Msg: "split oversized PDF: " + err.Error()}
	}
	var parts []string
	for i, ch := range chunks {
		raw, chunkCost, err := parseOnce(ctx, c, ch, fmt.Sprintf("%s.part%d.pdf", filename, i+1), engineMistralOCR)
		cost += chunkCost
		if err != nil {
			return docResult{}, cost, fmt.Errorf("OCR chunk %d/%d: %w", i+1, len(chunks), err)
		}
		if t := cleanParsed(raw); t != "" {
			parts = append(parts, t)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return docResult{Method: "empty", PageCount: pages}, cost, nil
	}
	return docResult{Text: text, Method: "cloud-ocr", PageCount: pages}, cost, nil
}

// parseOnce sends one PDF through one file-parser engine and returns
// the joined annotation text and the call's USD cost.
func parseOnce(ctx context.Context, c *Client, pdf []byte, filename, engine string) (string, float64, error) {
	dataURI := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf)
	cr, err := c.complete(ctx, DocumentModel, []contentPart{
		{Type: "text", Text: docPrompt},
		{Type: "file", File: &filePart{Filename: filename, FileData: dataURI}},
	}, []pdfPlugin{{ID: "file-parser", PDF: pdfPluginConfig{Engine: engine}}},
		docMaxOutputTokens, 0)
	if err != nil {
		return "", cr.cost(), err
	}
	return annotationText(cr), cr.cost(), nil
}

// annotationText joins the file-parser annotation segments, dropping the
// "<file name=...>" / "</file>" wrapper segments.
func annotationText(cr *chatResponse) string {
	if len(cr.Choices) == 0 {
		return ""
	}
	var b strings.Builder
	for _, a := range cr.Choices[0].Message.Annotations {
		if a.Type != "file" {
			continue
		}
		for _, seg := range a.File.Content {
			t := seg.Text
			if strings.HasPrefix(t, "<file ") || strings.TrimSpace(t) == "</file>" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
	}
	return strings.TrimSpace(b.String())
}

// splitTextDescription parses the model's "TEXT: … DESCRIPTION: …"
// reply into (ocr, description). Missing markers → the whole reply is
// the description. A literal "none" OCR body normalizes to "".
func splitTextDescription(s string) (ocr, desc string) {
	s = strings.TrimSpace(s)
	di := strings.Index(strings.ToLower(s), "description:")
	if di < 0 {
		return "", s
	}
	head := strings.TrimSpace(s[:di])
	desc = strings.TrimSpace(s[di+len("description:"):])
	if strings.HasPrefix(strings.ToLower(head), "text:") {
		head = strings.TrimSpace(head[len("text:"):])
	}
	if strings.EqualFold(head, "none") {
		head = ""
	}
	return head, desc
}

// cleanParsed strips the pdf-text "# file … ## Contents" header so we
// store just the body; mistral-ocr output passes through unchanged.
func cleanParsed(s string) string {
	if i := strings.Index(s, "## Contents"); i >= 0 {
		s = s[i+len("## Contents"):]
	}
	return strings.TrimSpace(s)
}

// stripPDFNoise reduces a pdf-text result to bare content characters so
// the escalation check can tell "has a real text layer" from "scanned,
// parser only emitted structure".
func stripPDFNoise(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(cleanParsed(s), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		b.WriteString(t)
	}
	return b.String()
}

// docPageCount returns the PDF's page count, or 0 if unreadable —
// metadata only, never the gate on extraction.
func docPageCount(pdf []byte) int {
	n, err := api.PageCount(bytes.NewReader(pdf), model.NewDefaultConfiguration())
	if err != nil {
		return 0
	}
	return n
}

// splitPDF cuts a PDF into page-range chunks each ≈ docChunkTargetBytes,
// sized from the document's bytes-per-page so image-heavy scans split
// finely enough to clear the provider limit.
func splitPDF(pdf []byte, pages int) ([][]byte, error) {
	if pages <= 0 {
		if pages = docPageCount(pdf); pages <= 0 {
			return nil, errors.New("cannot determine page count")
		}
	}
	bytesPerPage := len(pdf) / pages
	if bytesPerPage < 1 {
		bytesPerPage = 1
	}
	span := docChunkTargetBytes / bytesPerPage
	if span < 1 {
		span = 1
	}
	conf := model.NewDefaultConfiguration()
	var chunks [][]byte
	for start := 1; start <= pages; start += span {
		end := start + span - 1
		if end > pages {
			end = pages
		}
		var buf bytes.Buffer
		rng := fmt.Sprintf("%d-%d", start, end)
		if err := api.Trim(bytes.NewReader(pdf), &buf, []string{rng}, conf); err != nil {
			return nil, fmt.Errorf("trim pages %s: %w", rng, err)
		}
		chunks = append(chunks, append([]byte(nil), buf.Bytes()...))
	}
	return chunks, nil
}

func mimeForImageExt(ext string) string {
	switch ext {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "heic":
		return "image/heic"
	default:
		return "image/jpeg"
	}
}
