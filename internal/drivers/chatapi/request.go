package chatapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"agentflow/internal/core/chatreq"
	"agentflow/internal/core/media"
	"agentflow/internal/core/metrics"
	"agentflow/internal/core/session"
)

// apiError is a request refusal with the envelope fields the HTTP layer needs.
// status is the HTTP status; typ and code are the OpenAI error type and code.
type apiError struct {
	status  int
	typ     string
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(msg string) *apiError {
	return &apiError{status: http.StatusBadRequest, typ: "invalid_request_error", code: "invalid_request_error", message: msg}
}

// completionRequest is the wire shape this channel accepts. Unknown fields are
// ignored by encoding/json, which is the contract: accept and ignore.
type completionRequest struct {
	Model               string          `json:"model"`
	Messages            []wireMessage   `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       *streamOptions  `json:"stream_options"`
	Tools               []wireTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	Seed                *int64          `json:"seed"`
	ResponseFormat      map[string]any  `json:"response_format"`
	N                   *int            `json:"n"`
	User                string          `json:"user"`
	Metadata            map[string]any  `json:"metadata"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []wireToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type wirePart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
	InputAudio *struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	} `json:"input_audio"`
	File *struct {
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
	} `json:"file"`
	FileURL *struct {
		URL string `json:"url"`
	} `json:"file_url"`
	VideoURL *struct {
		URL string `json:"url"`
	} `json:"video_url"`
}

// parse validates one request and returns the parsed form plus the agent the
// request names.
func (d *Driver) parse(body []byte, h http.Header) (*chatreq.Request, string, *apiError) {
	var wire completionRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&wire); err != nil {
		return nil, "", badRequest("the request body is not valid JSON: " + err.Error())
	}
	if wire.Model == "" {
		return nil, "", badRequest(`"model" is required; use an agent name from GET /v1/models`)
	}
	if !d.exposed[wire.Model] {
		return nil, "", &apiError{
			status: http.StatusNotFound, typ: "invalid_request_error", code: "model_not_found",
			message: fmt.Sprintf("the model %q does not exist; this channel exposes %s", wire.Model, strings.Join(d.agents, ", ")),
		}
	}
	if wire.N != nil && *wire.N > 1 {
		return nil, "", badRequest(fmt.Sprintf("n=%d is not supported; this channel returns one choice (n=1)", *wire.N))
	}
	if len(wire.Messages) == 0 {
		return nil, "", badRequest(`"messages" must contain at least one message`)
	}

	req := &chatreq.Request{
		Model:             wire.Model,
		Stream:            wire.Stream,
		ToolChoice:        "",
		Temperature:       wire.Temperature,
		TopP:              wire.TopP,
		Seed:              wire.Seed,
		ResponseFormat:    wire.ResponseFormat,
		ParallelToolCalls: wire.ParallelToolCalls,
		User:              wire.User,
		Metadata:          wire.Metadata,
		Bytes:             len(body),
	}
	if wire.StreamOptions != nil {
		req.IncludeUsage = wire.StreamOptions.IncludeUsage
	}
	if wire.MaxCompletionTokens != nil {
		req.MaxTokens = *wire.MaxCompletionTokens
	} else if wire.MaxTokens != nil {
		req.MaxTokens = *wire.MaxTokens
	}
	if req.MaxTokens < 0 {
		return nil, "", badRequest("max_tokens must be >= 0")
	}

	msgs, aerr := d.parseMessages(wire.Messages)
	if aerr != nil {
		return nil, "", aerr
	}
	req.Messages = msgs

	tools, aerr := parseTools(wire.Tools)
	if aerr != nil {
		return nil, "", aerr
	}
	req.Tools = tools

	choice, aerr := parseToolChoice(wire.ToolChoice)
	if aerr != nil {
		return nil, "", aerr
	}
	req.ToolChoice = choice
	if strings.HasPrefix(choice, "function:") && len(req.Tools) == 0 {
		return nil, "", badRequest("tool_choice names a function but no tools were supplied")
	}

	stop, aerr := parseStop(wire.Stop)
	if aerr != nil {
		return nil, "", aerr
	}
	req.Stop = stop

	// conversation_id is untrusted: a client may set it to group its own
	// turns, and the route handler decides what that means. The header wins
	// because metadata is client JSON, which a proxy may rewrite.
	conv := strings.TrimSpace(h.Get("X-AgentFlow-Conversation"))
	if conv == "" {
		if v, ok := wire.Metadata["conversation_id"].(string); ok {
			conv = strings.TrimSpace(v)
		}
	}
	req.ConversationID = conv
	return req, wire.Model, nil
}

// parseMessages converts the wire messages to the runtime's neutral shape.
func (d *Driver) parseMessages(in []wireMessage) ([]session.ChatMessage, *apiError) {
	out := make([]session.ChatMessage, 0, len(in))
	for i, m := range in {
		content, parts, aerr := d.parseContent(m.Content)
		if aerr != nil {
			return nil, &apiError{status: aerr.status, typ: aerr.typ, code: aerr.code,
				message: fmt.Sprintf("messages[%d]: %s", i, aerr.message)}
		}
		cm := session.ChatMessage{
			Role:       m.Role,
			Content:    content,
			Parts:      parts,
			ToolCallID: m.ToolCallID,
		}
		switch m.Role {
		case "system", "developer", "user", "assistant":
		case "tool":
			if m.ToolCallID == "" {
				return nil, badRequest(fmt.Sprintf("messages[%d]: a tool message needs tool_call_id", i))
			}
		default:
			return nil, badRequest(fmt.Sprintf("messages[%d]: role %q is not supported (use system, developer, user, assistant or tool)", i, m.Role))
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "" {
				return nil, badRequest(fmt.Sprintf("messages[%d]: a tool call has no function name", i))
			}
			args := map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					return nil, badRequest(fmt.Sprintf("messages[%d]: tool call %q arguments are not a JSON object", i, tc.Function.Name))
				}
			}
			cm.ToolCalls = append(cm.ToolCalls, session.ToolCallSpec{ID: tc.ID, Name: tc.Function.Name, Args: args})
		}
		out = append(out, cm)
	}
	return out, nil
}

// parseContent turns a message's content into text plus media parts. A string
// is the fast path; an array may mix text, images, audio, video and files.
func (d *Driver) parseContent(raw json.RawMessage) (string, []media.Part, *apiError) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil, nil
	}
	var parts []wirePart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, badRequest("content must be a string or an array of content parts")
	}
	var b strings.Builder
	var out []media.Part
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return "", nil, badRequest("an image_url part needs image_url.url")
			}
			part, aerr := d.mediaFromURL("image", p.ImageURL.URL)
			if aerr != nil {
				return "", nil, aerr
			}
			out = append(out, part)
		case "input_audio":
			if p.InputAudio == nil || p.InputAudio.Data == "" {
				return "", nil, badRequest("an input_audio part needs input_audio.data")
			}
			mime := "audio/wav"
			if p.InputAudio.Format == "mp3" {
				mime = "audio/mpeg"
			}
			if !d.mediaAllowed(mime) {
				return "", nil, mediaRefused(mime)
			}
			if d.opt.Store != nil {
				part, aerr := d.mediaFromURL("audio", "data:"+mime+";base64,"+p.InputAudio.Data)
				if aerr != nil {
					return "", nil, aerr
				}
				out = append(out, part)
			} else {
				out = append(out, media.Part{Type: "audio", MIME: mime, Data: p.InputAudio.Data})
			}
		case "file":
			// file.file_data is a data URL in the OpenAI API, so it takes the
			// same path as an inline image: the channel's media policy decides
			// whether it is accepted, and the store holds the bytes.
			switch {
			case p.File != nil && p.File.FileData != "":
				raw := p.File.FileData
				if !strings.HasPrefix(raw, "data:") {
					raw = "data:application/octet-stream;base64," + raw
				}
				part, aerr := d.mediaFromURL("file", raw)
				if aerr != nil {
					return "", nil, aerr
				}
				part.Name = p.File.Filename
				out = append(out, part)
			case p.FileURL != nil && p.FileURL.URL != "":
				part, aerr := d.mediaFromURL("file", p.FileURL.URL)
				if aerr != nil {
					return "", nil, aerr
				}
				out = append(out, part)
			default:
				return "", nil, badRequest("a file part needs file.file_data or file_url.url")
			}
		case "video_url":
			if p.VideoURL == nil || p.VideoURL.URL == "" {
				return "", nil, badRequest("a video_url part needs video_url.url")
			}
			part, aerr := d.mediaFromURL("video", p.VideoURL.URL)
			if aerr != nil {
				return "", nil, aerr
			}
			out = append(out, part)
		default:
			return "", nil, badRequest(fmt.Sprintf("content part type %q is not supported (use text, image_url, input_audio, file or video_url)", p.Type))
		}
	}
	return b.String(), out, nil
}

// mediaFromURL handles the two image sources: a data URL is ingested through
// the channel's media policy, a remote URL is passed to the provider.
func (d *Driver) mediaFromURL(typ, raw string) (media.Part, *apiError) {
	if !strings.HasPrefix(raw, "data:") {
		return media.Part{Type: typ, URL: raw}, nil
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 || !strings.HasSuffix(raw[:comma], ";base64") {
		return media.Part{}, badRequest("a data URL must be data:<mime>;base64,<payload>")
	}
	meta := strings.TrimSuffix(strings.TrimPrefix(raw[:comma], "data:"), ";base64")
	mime := meta
	if mime == "" {
		mime = "application/octet-stream"
	}
	payload := raw[comma+1:]
	if !d.mediaAllowed(mime) {
		return media.Part{}, mediaRefused(mime)
	}
	rawBytes, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return media.Part{}, badRequest("a data URL payload is not valid base64")
	}
	if d.opt.Store != nil {
		ref, err := d.opt.Store.Put(strings.NewReader(string(rawBytes)), mime, d.opt.Policy)
		if err != nil {
			return media.Part{}, &apiError{status: http.StatusBadRequest, typ: "invalid_request_error", code: "invalid_request_error", message: "could not store the attached media: " + err.Error()}
		}
		metrics.Add("agentflow_media_bytes", ref.Size)
		metrics.Inc("agentflow_media_ingested")
		return media.Part{Type: media.Classify(mime), MIME: mime, Handle: ref.Handle}, nil
	}
	metrics.Inc("agentflow_media_ingested")
	return media.Part{Type: media.Classify(mime), MIME: mime, Data: base64.StdEncoding.EncodeToString(rawBytes)}, nil
}

// mediaAllowed applies the channel's media policy. A data URL is refused when
// media is disabled or the MIME is not allowed — never silently dropped: the
// client asked for the image to be seen, and a turn without it would answer a
// different question.
func (d *Driver) mediaAllowed(mime string) bool {
	if !d.opt.Policy.Enabled() {
		return false
	}
	return d.opt.Policy.Allows(mime)
}

func mediaRefused(mime string) *apiError {
	return badRequest(fmt.Sprintf("this channel does not accept %s media: add it to the channel's media.allow list (and make sure a media store is configured)", mime))
}

func parseTools(in []wireTool) ([]session.ToolSpec, *apiError) {
	out := make([]session.ToolSpec, 0, len(in))
	for i, t := range in {
		if t.Type != "function" {
			return nil, badRequest(fmt.Sprintf("tools[%d]: type %q is not supported; only function tools are accepted (server tools are configured on the model)", i, t.Type))
		}
		if t.Function.Name == "" {
			return nil, badRequest(fmt.Sprintf("tools[%d]: function.name is required", i))
		}
		out = append(out, session.ToolSpec{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out, nil
}

func parseToolChoice(raw json.RawMessage) (string, *apiError) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s, nil
		default:
			return "", badRequest(fmt.Sprintf("tool_choice %q is not supported (use auto, none, required, or {\"type\":\"function\",\"function\":{\"name\":\"...\"}})", s))
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function.Name == "" {
		return "", badRequest(`tool_choice must be auto, none, required, or {"type":"function","function":{"name":"..."}}`)
	}
	return "function:" + obj.Function.Name, nil
}

func parseStop(raw json.RawMessage) ([]string, *apiError) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	return nil, badRequest("stop must be a string or an array of strings")
}
