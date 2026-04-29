package codex

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

type Adaptor struct {
}

// codexClientWantsStreamKey records the original client streaming preference
// so DoResponse can fold the upstream SSE back into a single JSON body for
// non-stream callers (codex backend always requires stream:true on the wire).
const codexClientWantsStreamKey = "codex_client_wants_stream"

// codexImageGenerationDefaultInstructions matches the instructions used by
// upstream Codex tooling so the mainline model reliably invokes the
// image_generation tool when forwarded under a ChatGPT account.
const codexImageGenerationDefaultInstructions = "Use the image_generation tool when the user asks to draw, create, generate, or edit an image."

// codexImageGenerationToolDefaults matches the tool parameters that codex CLI
// sends for the hosted image_generation tool. The upstream rejects requests
// where these are missing, so we always populate sensible defaults.
var codexImageGenerationToolDefaults = map[string]any{
	"size":          "auto",
	"quality":       "auto",
	"output_format": "png",
	"background":    "auto",
	"action":        "auto",
}

func (a *Adaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

func (a *Adaptor) ConvertClaudeRequest(*gin.Context, *relaycommon.RelayInfo, *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/messages endpoint not supported")
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("codex channel: endpoint not supported")
}

// ConvertImageRequest bridges /v1/images/generations to the codex Responses
// API: the mainline model has to be a real text model (gpt-5.2);
// gpt-image-2 only lives on the hosted image_generation tool entry.
func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" {
		return nil, errors.New("codex channel: image prompt is required")
	}

	tool := map[string]any{
		"type":  "image_generation",
		"model": codexImageGenerationModel,
	}
	for k, v := range codexImageGenerationToolDefaults {
		tool[k] = v
	}
	if size := strings.TrimSpace(request.Size); size != "" {
		tool["size"] = size
	}
	if quality := strings.TrimSpace(request.Quality); quality != "" {
		tool["quality"] = quality
	}
	if len(request.OutputFormat) > 0 {
		var fmtVal any
		if err := common.Unmarshal(request.OutputFormat, &fmtVal); err == nil && fmtVal != nil {
			tool["output_format"] = fmtVal
		}
	}
	if len(request.Background) > 0 {
		var bgVal any
		if err := common.Unmarshal(request.Background, &bgVal); err == nil && bgVal != nil {
			tool["background"] = bgVal
		}
	}

	// Build the user message content: text prompt first, then any input images.
	// For /v1/images/edits the caller supplies one or more images via multipart
	// or the request.Image field; we forward them as input_image parts so the
	// upstream image_generation tool can use them for image-to-image editing.
	content := []map[string]any{
		{"type": "input_text", "text": prompt},
	}
	imageParts, err := collectCodexImageInputParts(c, request)
	if err != nil {
		return nil, fmt.Errorf("codex channel: collect input images: %w", err)
	}
	content = append(content, imageParts...)

	inputArr := []map[string]any{
		{
			"type":    "message",
			"role":    "user",
			"content": content,
		},
	}
	inputRaw, err := common.Marshal(inputArr)
	if err != nil {
		return nil, err
	}
	toolsRaw, err := common.Marshal([]map[string]any{tool})
	if err != nil {
		return nil, err
	}
	toolChoiceRaw, err := common.Marshal(map[string]any{"type": "image_generation"})
	if err != nil {
		return nil, err
	}
	instructionsRaw, err := common.Marshal(codexImageGenerationDefaultInstructions)
	if err != nil {
		return nil, err
	}

	mainline := codexImageGenerationMainModel
	streamTrue := true
	responsesReq := &dto.OpenAIResponsesRequest{
		Model:        mainline,
		Input:        inputRaw,
		Instructions: instructionsRaw,
		Tools:        toolsRaw,
		ToolChoice:   toolChoiceRaw,
		Store:        json.RawMessage("false"),
		// Codex backend requires stream:true; we collect the SSE upstream and
		// translate it back into the JSON Images-API shape in DoResponse.
		Stream: &streamTrue,
	}

	// image_handler does not auto-route to /v1/responses, so we explicitly
	// flip info to the codex responses path while preserving the original
	// ImagesGenerations relay mode for billing.
	info.UpstreamModelName = mainline
	info.IsStream = true

	return responsesReq, nil
}

func collectCodexImageInputParts(c *gin.Context, request dto.ImageRequest) ([]map[string]any, error) {
	parts, err := codexImageInputPartsFromRaw(request.Image)
	if err != nil {
		return nil, err
	}

	multipartParts, err := codexMultipartImageInputParts(c)
	if err != nil {
		return nil, err
	}
	parts = append(parts, multipartParts...)
	return parts, nil
}

func codexImageInputPartsFromRaw(raw json.RawMessage) ([]map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}

	switch common.GetJsonType(raw) {
	case "string":
		var value string
		if err := common.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		if part, ok := codexImageInputPartFromReference(value, ""); ok {
			return []map[string]any{part}, nil
		}
	case "array":
		var items []json.RawMessage
		if err := common.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		parts := make([]map[string]any, 0, len(items))
		for _, item := range items {
			itemParts, err := codexImageInputPartsFromRaw(item)
			if err != nil {
				return nil, err
			}
			parts = append(parts, itemParts...)
		}
		return parts, nil
	case "object":
		var obj map[string]json.RawMessage
		if err := common.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		return codexImageInputPartsFromObject(obj)
	}

	return nil, nil
}

func codexImageInputPartsFromObject(obj map[string]json.RawMessage) ([]map[string]any, error) {
	detail := codexStringField(obj, "detail")
	if fileID := codexStringField(obj, "file_id"); fileID != "" {
		part := map[string]any{
			"type":    "input_image",
			"file_id": fileID,
		}
		if detail != "" {
			part["detail"] = detail
		}
		return []map[string]any{part}, nil
	}

	for _, key := range []string{"image_url", "url"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		part, ok, err := codexImageInputPartFromURLRaw(raw, detail)
		if err != nil {
			return nil, err
		}
		if ok {
			return []map[string]any{part}, nil
		}
	}

	return nil, nil
}

func codexImageInputPartFromURLRaw(raw json.RawMessage, detail string) (map[string]any, bool, error) {
	raw = bytes.TrimSpace(raw)
	switch common.GetJsonType(raw) {
	case "string":
		var value string
		if err := common.Unmarshal(raw, &value); err != nil {
			return nil, false, err
		}
		part, ok := codexImageInputPartFromReference(value, detail)
		return part, ok, nil
	case "object":
		var obj map[string]json.RawMessage
		if err := common.Unmarshal(raw, &obj); err != nil {
			return nil, false, err
		}
		if detail == "" {
			detail = codexStringField(obj, "detail")
		}
		if value := codexStringField(obj, "url"); value != "" {
			part, ok := codexImageInputPartFromReference(value, detail)
			return part, ok, nil
		}
	}
	return nil, false, nil
}

func codexStringField(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	var value string
	if err := common.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func codexImageInputPartFromReference(value string, detail string) (map[string]any, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}

	part := map[string]any{"type": "input_image"}
	if strings.HasPrefix(value, "file-") {
		part["file_id"] = value
	} else {
		part["image_url"] = value
	}
	if detail = strings.TrimSpace(detail); detail != "" {
		part["detail"] = detail
	}
	return part, true
}

func codexMultipartImageInputParts(c *gin.Context) ([]map[string]any, error) {
	if c == nil || c.Request == nil || !strings.Contains(strings.ToLower(c.Request.Header.Get("Content-Type")), "multipart/form-data") {
		return nil, nil
	}
	if c.Request.MultipartForm == nil {
		if _, err := c.MultipartForm(); err != nil {
			return nil, fmt.Errorf("parse image edit multipart form: %w", err)
		}
	}
	if c.Request.MultipartForm == nil {
		return nil, nil
	}

	parts := make([]map[string]any, 0)
	for _, fieldName := range []string{"image", "image[]"} {
		for _, fileHeader := range c.Request.MultipartForm.File[fieldName] {
			dataURL, err := codexMultipartImageDataURL(fileHeader)
			if err != nil {
				return nil, err
			}
			if part, ok := codexImageInputPartFromReference(dataURL, ""); ok {
				parts = append(parts, part)
			}
		}
	}
	return parts, nil
}

func codexMultipartImageDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", errors.New("empty image file")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("open image file %q: %w", fileHeader.Filename, err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read image file %q: %w", fileHeader.Filename, err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("image file %q is empty", fileHeader.Filename)
	}

	mimeType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mimeType == "" || strings.EqualFold(mimeType, "application/octet-stream") {
		mimeType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return "", fmt.Errorf("image file %q has unsupported content type %q", fileHeader.Filename, mimeType)
	}

	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/chat/completions endpoint not supported")
}

func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/rerank endpoint not supported")
}

func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("codex channel: /v1/embeddings endpoint not supported")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	isCompact := info != nil && info.RelayMode == relayconstant.RelayModeResponsesCompact

	if err := applyImageGenerationModelAlias(&request); err != nil {
		return nil, err
	}

	// Codex backend rejects scalar `input` ("Input must be a list"). Wrap a
	// bare string into the canonical Responses-API message-array form so that
	// direct /v1/responses callers (e.g. external scripts) keep working.
	if normalized, ok := normalizeCodexInputAsList(request.Input); ok {
		request.Input = normalized
	}

	if info != nil && info.ChannelSetting.SystemPrompt != "" {
		systemPrompt := info.ChannelSetting.SystemPrompt

		if len(request.Instructions) == 0 {
			if b, err := common.Marshal(systemPrompt); err == nil {
				request.Instructions = b
			} else {
				return nil, err
			}
		} else if info.ChannelSetting.SystemPromptOverride {
			var existing string
			if err := common.Unmarshal(request.Instructions, &existing); err == nil {
				existing = strings.TrimSpace(existing)
				if existing == "" {
					if b, err := common.Marshal(systemPrompt); err == nil {
						request.Instructions = b
					} else {
						return nil, err
					}
				} else {
					if b, err := common.Marshal(systemPrompt + "\n" + existing); err == nil {
						request.Instructions = b
					} else {
						return nil, err
					}
				}
			} else {
				if b, err := common.Marshal(systemPrompt); err == nil {
					request.Instructions = b
				} else {
					return nil, err
				}
			}
		}
	}
	// Codex backend requires the `instructions` field to be present.
	if len(request.Instructions) == 0 {
		request.Instructions = json.RawMessage(`""`)
	}

	if isCompact {
		return request, nil
	}
	// codex: store must be false
	request.Store = json.RawMessage("false")
	// rm max_output_tokens / temperature / top_p (codex rejects them)
	request.MaxOutputTokens = nil
	request.Temperature = nil
	request.TopP = nil
	// Codex backend rejects requests when stream != true (HTTP 400
	// "Stream must be set to true"). Capture the caller's original preference
	// so DoResponse can re-fold the upstream SSE back into JSON for clients
	// that did NOT ask for streaming, then force stream:true on the wire.
	clientWantsStream := request.Stream != nil && *request.Stream
	if c != nil {
		c.Set(codexClientWantsStreamKey, clientWantsStream)
	}
	streamTrue := true
	request.Stream = &streamTrue
	if info != nil {
		info.IsStream = true
	}
	return request, nil
}

// isCodexImageGenerationModel reports whether the provided model is the
// gpt-image-2 alias (with optional compact suffix).
func isCodexImageGenerationModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	bare := strings.TrimSuffix(model, ratio_setting.CompactModelSuffix)
	return strings.EqualFold(bare, codexImageGenerationModel)
}

func applyImageGenerationModelAlias(request *dto.OpenAIResponsesRequest) error {
	if request == nil {
		return nil
	}
	rawModel := strings.TrimSpace(request.Model)
	if !isCodexImageGenerationModel(rawModel) {
		return nil
	}

	// Preserve compact-suffix routing if the caller used it, but always swap
	// the bare alias to a real mainline text model that the Codex backend
	// accepts as the request's top-level "model" field.
	mainline := codexImageGenerationMainModel
	if strings.HasSuffix(rawModel, ratio_setting.CompactModelSuffix) {
		mainline = ratio_setting.WithCompactModelSuffix(mainline)
	}
	request.Model = mainline

	tools := make([]map[string]any, 0)
	if len(request.Tools) > 0 {
		if err := common.Unmarshal(request.Tools, &tools); err != nil {
			return err
		}
	}

	hasImageGenerationTool := false
	for _, tool := range tools {
		if common.Interface2String(tool["type"]) != "image_generation" {
			continue
		}
		hasImageGenerationTool = true
		if strings.TrimSpace(common.Interface2String(tool["model"])) == "" {
			tool["model"] = codexImageGenerationModel
		}
		for k, v := range codexImageGenerationToolDefaults {
			if _, ok := tool[k]; !ok {
				tool[k] = v
			}
		}
	}
	if !hasImageGenerationTool {
		tool := map[string]any{
			"type":  "image_generation",
			"model": codexImageGenerationModel,
		}
		for k, v := range codexImageGenerationToolDefaults {
			tool[k] = v
		}
		tools = append(tools, tool)
	}

	toolsRaw, err := common.Marshal(tools)
	if err != nil {
		return err
	}
	request.Tools = toolsRaw

	if len(request.ToolChoice) == 0 {
		toolChoiceRaw, err := common.Marshal(map[string]any{"type": "image_generation"})
		if err != nil {
			return err
		}
		request.ToolChoice = toolChoiceRaw
	}

	if len(request.Instructions) == 0 {
		if b, err := common.Marshal(codexImageGenerationDefaultInstructions); err == nil {
			request.Instructions = b
		}
	}

	return nil
}

// applyImageGenerationAliasToBody is the byte-level twin of
// ConvertOpenAIResponsesRequest. It guarantees that requests reaching the
// codex backend are wire-compatible (model alias + array input + stream:true)
// even on pass-through paths where the struct-level converter is bypassed.
//
// It is intentionally permissive: when the body cannot be parsed as a JSON
// object, it is returned unchanged.
func applyImageGenerationAliasToBody(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body, nil
	}

	var data map[string]any
	if err := common.Unmarshal(body, &data); err != nil {
		return body, nil
	}

	model, _ := data["model"].(string)
	mutated := false

	// Codex backend rejects `"input": "<string>"` with HTTP 400
	// "Input must be a list"; wrap into the message-array form.
	if rawInput, exists := data["input"]; exists {
		if s, ok := rawInput.(string); ok {
			data["input"] = []map[string]any{
				{
					"type": "message",
					"role": "user",
					"content": []map[string]any{
						{"type": "input_text", "text": s},
					},
				},
			}
			mutated = true
		}
	}

	// Force stream:true (codex backend rejects anything else with HTTP 400
	// "Stream must be set to true").
	if v, exists := data["stream"]; !exists || v != true {
		data["stream"] = true
		mutated = true
	}

	if !isCodexImageGenerationModel(model) {
		if !mutated {
			return body, nil
		}
		rewritten, err := common.Marshal(data)
		if err != nil {
			return body, nil
		}
		return rewritten, nil
	}

	mainline := codexImageGenerationMainModel
	if strings.HasSuffix(strings.TrimSpace(model), ratio_setting.CompactModelSuffix) {
		mainline = ratio_setting.WithCompactModelSuffix(mainline)
	}
	data["model"] = mainline

	rawTools, _ := data["tools"].([]any)
	tools := make([]map[string]any, 0, len(rawTools)+1)
	for _, raw := range rawTools {
		if m, ok := raw.(map[string]any); ok {
			tools = append(tools, m)
		}
	}

	hasImageGenerationTool := false
	for _, tool := range tools {
		if common.Interface2String(tool["type"]) != "image_generation" {
			continue
		}
		hasImageGenerationTool = true
		if strings.TrimSpace(common.Interface2String(tool["model"])) == "" {
			tool["model"] = codexImageGenerationModel
		}
		for k, v := range codexImageGenerationToolDefaults {
			if _, ok := tool[k]; !ok {
				tool[k] = v
			}
		}
	}
	if !hasImageGenerationTool {
		tool := map[string]any{
			"type":  "image_generation",
			"model": codexImageGenerationModel,
		}
		for k, v := range codexImageGenerationToolDefaults {
			tool[k] = v
		}
		tools = append(tools, tool)
	}
	data["tools"] = tools

	if _, exists := data["tool_choice"]; !exists {
		data["tool_choice"] = map[string]any{"type": "image_generation"}
	}

	if _, exists := data["instructions"]; !exists {
		data["instructions"] = codexImageGenerationDefaultInstructions
	} else if s, ok := data["instructions"].(string); ok && strings.TrimSpace(s) == "" {
		data["instructions"] = codexImageGenerationDefaultInstructions
	}

	rewritten, err := common.Marshal(data)
	if err != nil {
		return body, nil
	}
	return rewritten, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	// Byte-level safety net: ensure model alias / array-input / stream:true
	// are honored before forwarding upstream, regardless of whether the
	// request reached us via the struct-level converter or pass-through path.
	rewritten, err := rewriteCodexRequestBodyForImageGeneration(requestBody)
	if err != nil {
		return nil, err
	}
	if rewritten != nil {
		requestBody = rewritten
	}
	return channel.DoApiRequest(a, c, info, requestBody)
}

func rewriteCodexRequestBodyForImageGeneration(requestBody io.Reader) (io.Reader, error) {
	if requestBody == nil {
		return nil, nil
	}
	bodyBytes, err := io.ReadAll(requestBody)
	if err != nil {
		return nil, err
	}
	if len(bodyBytes) == 0 {
		return bytes.NewReader(bodyBytes), nil
	}
	rewritten, err := applyImageGenerationAliasToBody(bodyBytes)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(rewritten), nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	switch info.RelayMode {
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact:
		// If the caller did NOT ask for streaming but we forced upstream
		// stream:true (codex backend mandates it), accumulate the SSE events
		// back into a single OpenAIResponsesResponse JSON object so the
		// client gets the JSON shape it expects.
		clientWantsStream, _ := c.Get(codexClientWantsStreamKey)
		if wantStream, _ := clientWantsStream.(bool); !wantStream && info.RelayMode == relayconstant.RelayModeResponses {
			return convertCodexResponsesSSEToJSON(c, resp)
		}
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		return convertCodexImageGenerationResponse(c, resp)
	default:
		return nil, types.NewError(errors.New("codex channel: endpoint not supported"), types.ErrorCodeInvalidRequest)
	}

	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		return openai.OaiResponsesCompactionHandler(c, resp)
	}

	if info.IsStream {
		return openai.OaiResponsesStreamHandler(c, info, resp)
	}
	return openai.OaiResponsesHandler(c, info, resp)
}

func writeCodexImageGenerationResponse(c *gin.Context, images []string) *types.NewAPIError {
	data := make([]map[string]any, 0, len(images))
	for _, b64 := range images {
		data = append(data, map[string]any{"b64_json": b64})
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	}
	payload, err := common.Marshal(out)
	if err != nil {
		return types.NewError(fmt.Errorf("codex channel: marshal image response: %w", err), types.ErrorCodeBadResponse)
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(payload)
	return nil
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	switch info.RelayMode {
	case relayconstant.RelayModeResponses, relayconstant.RelayModeResponsesCompact, relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
	default:
		return "", errors.New("codex channel: only /v1/responses, /v1/responses/compact, /v1/images/generations are supported")
	}
	path := "/backend-api/codex/responses"
	if info.RelayMode == relayconstant.RelayModeResponsesCompact {
		path = "/backend-api/codex/responses/compact"
	}
	return relaycommon.GetFullRequestURL(info.ChannelBaseUrl, path, info.ChannelType), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	channel.SetupApiRequestHeader(info, c, req)

	key := strings.TrimSpace(info.ApiKey)
	if !strings.HasPrefix(key, "{") {
		return errors.New("codex channel: key must be a JSON object")
	}

	oauthKey, err := ParseOAuthKey(key)
	if err != nil {
		return err
	}

	accessToken := strings.TrimSpace(oauthKey.AccessToken)
	accountID := strings.TrimSpace(oauthKey.AccountID)

	if accessToken == "" {
		return errors.New("codex channel: access_token is required")
	}
	if accountID == "" {
		return errors.New("codex channel: account_id is required")
	}

	req.Set("Authorization", "Bearer "+accessToken)
	req.Set("chatgpt-account-id", accountID)

	if req.Get("OpenAI-Beta") == "" {
		req.Set("OpenAI-Beta", "responses=experimental")
	}
	if req.Get("originator") == "" {
		req.Set("originator", "codex_cli_rs")
	}

	// chatgpt.com/backend-api/codex/responses is strict about Content-Type.
	// Force the exact media type to avoid charset suffixes being rejected.
	req.Set("Content-Type", "application/json")
	if info.IsStream {
		req.Set("Accept", "text/event-stream")
	} else if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}

	return nil
}

// normalizeCodexInputAsList wraps a string `input` (used by simple /v1/responses
// clients that send `"input": "draw a cat"`) into the array-of-message form
// the Codex backend requires. Returns (rewritten, true) only when wrapping
// happened.
func normalizeCodexInputAsList(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return raw, false
	}
	var s string
	if err := common.Unmarshal(trimmed, &s); err != nil {
		return raw, false
	}
	wrapped := []map[string]any{
		{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": s},
			},
		},
	}
	out, err := common.Marshal(wrapped)
	if err != nil {
		return raw, false
	}
	return out, true
}

// convertCodexResponsesSSEToJSON consumes the codex SSE upstream and emits a
// single application/json body to the originating client. This bridges the
// gap for clients that hit /v1/responses without `stream:true` but where the
// codex backend mandates SSE on the wire.
//
// It walks the stream, captures the latest `response` object, plus every
// `response.output_item.done` item, then merges those items into the final
// response's `output` array (codex's terminating `response.completed` event
// ships an empty output array since per-item content is delivered via
// `response.output_item.done`).
func convertCodexResponsesSSEToJSON(c *gin.Context, resp *http.Response) (any, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewError(errors.New("codex channel: empty upstream response"), types.ErrorCodeBadResponse)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, types.NewError(fmt.Errorf("codex channel: read upstream body: %w", err), types.ErrorCodeBadResponse)
	}

	var finalRespJSON []byte
	if t := bytes.TrimSpace(bodyBytes); len(t) > 0 && t[0] == '{' {
		finalRespJSON = bodyBytes
	} else {
		var (
			lastResponse json.RawMessage
			doneItems    []json.RawMessage
		)
		for _, line := range bytes.Split(bodyBytes, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			var event map[string]json.RawMessage
			if err := common.Unmarshal(payload, &event); err != nil {
				continue
			}
			if r, ok := event["response"]; ok && len(r) > 0 {
				lastResponse = r
			}
			var eventType string
			if rawType, ok := event["type"]; ok {
				_ = common.Unmarshal(rawType, &eventType)
			}
			if eventType == "response.output_item.done" {
				if rawItem, ok := event["item"]; ok && len(rawItem) > 0 {
					doneItems = append(doneItems, rawItem)
				}
			}
		}
		if len(lastResponse) == 0 {
			preview := bodyBytes
			if len(preview) > 512 {
				preview = preview[:512]
			}
			return nil, types.NewError(fmt.Errorf("codex channel: no response in SSE (preview=%s)", string(preview)), types.ErrorCodeBadResponse)
		}

		var respMap map[string]json.RawMessage
		if err := common.Unmarshal(lastResponse, &respMap); err == nil && len(doneItems) > 0 {
			existing := []json.RawMessage{}
			if rawOut, ok := respMap["output"]; ok && len(rawOut) > 0 {
				_ = common.Unmarshal(rawOut, &existing)
			}
			existing = append(existing, doneItems...)
			merged, err := common.Marshal(existing)
			if err == nil {
				respMap["output"] = merged
				if rebuilt, err := common.Marshal(respMap); err == nil {
					lastResponse = rebuilt
				}
			}
		}
		finalRespJSON = lastResponse
	}

	usage := &dto.Usage{}
	var parsed dto.OpenAIResponsesResponse
	if err := common.Unmarshal(finalRespJSON, &parsed); err == nil && parsed.Usage != nil {
		usage = parsed.Usage
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	_, _ = c.Writer.Write(finalRespJSON)
	return usage, nil
}

// convertCodexImageGenerationResponse reads the upstream Codex /responses
// payload (JSON or SSE) and translates the embedded image_generation_call
// `result` (base64) into the OpenAI Images API response shape that
// /v1/images/generations clients expect.
func convertCodexImageGenerationResponse(c *gin.Context, resp *http.Response) (any, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewError(errors.New("codex channel: empty upstream response"), types.ErrorCodeBadResponse)
	}
	defer resp.Body.Close()

	var bodyBytes []byte
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 128<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			bodyBytes = append(bodyBytes, '\n')
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				images, usage := extractCodexFinalImageResultsFromSSEPayload(payload)
				if len(images) > 0 {
					if err := writeCodexImageGenerationResponse(c, images); err != nil {
						return nil, err
					}
					return usage, nil
				}
			}
		}
		bodyBytes = append(bodyBytes, line...)
		bodyBytes = append(bodyBytes, '\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, types.NewError(fmt.Errorf("codex channel: read upstream body: %w", err), types.ErrorCodeBadResponse)
	}

	images, usage := extractCodexImageResults(bodyBytes)
	if len(images) == 0 {
		preview := string(bodyBytes)
		if len(preview) > 512 {
			preview = preview[:512]
		}
		return nil, types.NewError(fmt.Errorf("codex channel: no image_generation_call result in upstream response (preview=%s)", preview), types.ErrorCodeBadResponse)
	}

	if err := writeCodexImageGenerationResponse(c, images); err != nil {
		return nil, err
	}
	return usage, nil
}

func extractCodexFinalImageResultsFromSSEPayload(payload []byte) ([]string, *dto.Usage) {
	images := make(map[string]string)
	usage := &dto.Usage{}

	collectFromResponse := func(resp *dto.OpenAIResponsesResponse) {
		if resp == nil {
			return
		}
		for _, out := range resp.Output {
			if out.Type == dto.ResponsesOutputTypeImageGenerationCall && strings.TrimSpace(out.Result) != "" {
				images[out.ID] = out.Result
			}
		}
		if resp.Usage != nil {
			usage = resp.Usage
		}
	}

	var event map[string]json.RawMessage
	if err := common.Unmarshal(payload, &event); err != nil {
		return nil, usage
	}
	if rawResp, ok := event["response"]; ok {
		var response dto.OpenAIResponsesResponse
		if err := common.Unmarshal(rawResp, &response); err == nil {
			collectFromResponse(&response)
		}
	}
	if rawItem, ok := event["item"]; ok {
		var item dto.ResponsesOutput
		if err := common.Unmarshal(rawItem, &item); err == nil {
			if item.Type == dto.ResponsesOutputTypeImageGenerationCall && strings.TrimSpace(item.Result) != "" {
				images[item.ID] = item.Result
			}
		}
	}

	var results []string
	for _, b64 := range images {
		results = append(results, b64)
	}
	return results, usage
}

// extractCodexImageResults parses both JSON-object and SSE-stream forms of
// the codex /responses output and pulls the base64 `result` strings out of
// any image_generation_call entries.
func extractCodexImageResults(body []byte) ([]string, *dto.Usage) {
	images := make(map[string]string) // item_id -> base64
	usage := &dto.Usage{}

	tryParseObject := func(buf []byte) {
		buf = bytes.TrimSpace(buf)
		if len(buf) == 0 || buf[0] != '{' {
			return
		}
		var resp dto.OpenAIResponsesResponse
		if err := common.Unmarshal(buf, &resp); err != nil {
			return
		}
		for _, out := range resp.Output {
			if out.Type == dto.ResponsesOutputTypeImageGenerationCall && strings.TrimSpace(out.Result) != "" {
				images[out.ID] = out.Result
			}
		}
		if resp.Usage != nil {
			usage = resp.Usage
		}
	}

	tryParseObject(body)

	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var event map[string]json.RawMessage
		if err := common.Unmarshal(payload, &event); err != nil {
			continue
		}
		if rawResp, ok := event["response"]; ok {
			tryParseObject(rawResp)
		}
		if rawItem, ok := event["item"]; ok {
			var item dto.ResponsesOutput
			if err := common.Unmarshal(rawItem, &item); err == nil {
				if item.Type == dto.ResponsesOutputTypeImageGenerationCall && strings.TrimSpace(item.Result) != "" {
					images[item.ID] = item.Result
				}
			}
		}
		if rawType, ok := event["type"]; ok {
			var eventType string
			_ = common.Unmarshal(rawType, &eventType)
			if eventType == "response.image_generation_call.partial_image" {
				var partial struct {
					ItemID string `json:"item_id"`
					Image  string `json:"partial_image_b64"`
				}
				if err := common.Unmarshal(payload, &partial); err == nil && partial.ItemID != "" {
					images[partial.ItemID] = partial.Image
				}
			}
		}
	}

	var results []string
	for _, b64 := range images {
		results = append(results, b64)
	}
	return results, usage
}
