package codex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConvertOpenAIResponsesRequestOmitsUnsupportedSamplingParameters(t *testing.T) {
	topP := 0.9
	temperature := 0.7
	maxOutputTokens := uint(512)

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}, dto.OpenAIResponsesRequest{
		Model:           "gpt-5.3-codex",
		Input:           json.RawMessage(`[{"role":"user","content":"hello"}]`),
		MaxOutputTokens: &maxOutputTokens,
		Temperature:     &temperature,
		TopP:            &topP,
	})

	require.NoError(t, err)
	request := converted.(dto.OpenAIResponsesRequest)
	require.Nil(t, request.MaxOutputTokens)
	require.Nil(t, request.Temperature)
	require.Nil(t, request.TopP)
}

func TestConvertOpenAIResponsesRequestMapsGPTImage2ToImageGenerationTool(t *testing.T) {
	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponses,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}, dto.OpenAIResponsesRequest{
		Model: "gpt-image-2",
		Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"draw a cat"}]}]`),
	})

	require.NoError(t, err)
	request := converted.(dto.OpenAIResponsesRequest)
	require.Equal(t, "gpt-5.2", request.Model)

	var tools []map[string]any
	require.NoError(t, json.Unmarshal(request.Tools, &tools))
	require.Len(t, tools, 1)
	require.Equal(t, "image_generation", tools[0]["type"])
	require.Equal(t, "gpt-image-2", tools[0]["model"])
	for _, key := range []string{"size", "quality", "output_format", "background", "action"} {
		require.NotNil(t, tools[0][key], "expected default %q on image_generation tool", key)
	}

	var toolChoice map[string]any
	require.NoError(t, json.Unmarshal(request.ToolChoice, &toolChoice))
	require.Equal(t, "image_generation", toolChoice["type"])

	var instructions string
	require.NoError(t, json.Unmarshal(request.Instructions, &instructions))
	require.NotEmpty(t, instructions)
}

func TestConvertOpenAIResponsesRequestMapsGPTImage2WithCompactSuffix(t *testing.T) {
	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(nil, &relaycommon.RelayInfo{
		RelayMode:   relayconstant.RelayModeResponsesCompact,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}, dto.OpenAIResponsesRequest{
		Model: "gpt-image-2-openai-compact",
		Input: json.RawMessage(`[{"role":"user","content":[{"type":"input_text","text":"draw a cat"}]}]`),
	})

	require.NoError(t, err)
	request := converted.(dto.OpenAIResponsesRequest)
	require.Equal(t, "gpt-5.2-openai-compact", request.Model)

	var tools []map[string]any
	require.NoError(t, json.Unmarshal(request.Tools, &tools))
	require.Len(t, tools, 1)
	require.Equal(t, "image_generation", tools[0]["type"])
	require.Equal(t, "gpt-image-2", tools[0]["model"])
}

func TestApplyImageGenerationAliasToBodyRewritesPassThroughBody(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","input":[{"role":"user","content":"draw a cat"}],"stream":true}`)
	rewritten, err := applyImageGenerationAliasToBody(body)
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &data))
	require.Equal(t, "gpt-5.2", data["model"])

	tools, ok := data["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	require.Equal(t, "image_generation", tool["type"])
	require.Equal(t, "gpt-image-2", tool["model"])

	choice, ok := data["tool_choice"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "image_generation", choice["type"])

	instructions, ok := data["instructions"].(string)
	require.True(t, ok)
	require.NotEmpty(t, instructions)
}

func TestApplyImageGenerationAliasToBodyDoesNotRewriteModelOrTools(t *testing.T) {
	// Non-image requests still get input-as-list normalization and stream:true
	// forced (codex backend mandates both), but the model and tool fields must
	// stay untouched.
	body := []byte(`{"model":"gpt-5.2","input":"hi"}`)
	rewritten, err := applyImageGenerationAliasToBody(body)
	require.NoError(t, err)

	var data map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &data))
	require.Equal(t, "gpt-5.2", data["model"])
	require.Equal(t, true, data["stream"])
	require.Nil(t, data["tools"])
	require.Nil(t, data["tool_choice"])

	inputs, ok := data["input"].([]any)
	require.True(t, ok)
	require.Len(t, inputs, 1)
}

func TestApplyImageGenerationAliasToBodyKeepsAlreadyShapedRequest(t *testing.T) {
	// Already-array input + stream:true means no mutation is required.
	body := []byte(`{"model":"gpt-5.2","input":[{"role":"user","content":"hi"}],"stream":true}`)
	rewritten, err := applyImageGenerationAliasToBody(body)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(rewritten))
}

func TestApplyImageGenerationAliasToBodyHandlesInvalidJSON(t *testing.T) {
	body := []byte(`not-json`)
	rewritten, err := applyImageGenerationAliasToBody(body)
	require.NoError(t, err)
	require.Equal(t, string(body), string(rewritten))
}

func TestConvertCodexImageGenerationResponseReturnsWhenImageResultArrives(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	reader, writer := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       reader,
	}

	done := make(chan error, 1)
	go func() {
		_, apiErr := convertCodexImageGenerationResponse(c, resp)
		if apiErr != nil {
			done <- apiErr
			return
		}
		done <- nil
	}()

	_, err := fmt.Fprint(writer, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"img_1\",\"type\":\"image_generation_call\",\"result\":\"abc123\"}}\n\n")
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		_ = writer.Close()
		t.Fatal("expected image response to return before upstream SSE closes")
	}

	_ = writer.Close()

	var response struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Data, 1)
	require.Equal(t, "abc123", response.Data[0].B64JSON)
}
