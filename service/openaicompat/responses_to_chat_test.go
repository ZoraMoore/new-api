package openaicompat

import (
	"encoding/json"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/require"
)

func TestResponsesResponseToChatCompletionsResponseIncludesGeneratedImage(t *testing.T) {
	resp := &dto.OpenAIResponsesResponse{
		ID:        "resp_123",
		CreatedAt: 1777440000,
		Model:     "gpt-5.2",
		Output: []dto.ResponsesOutput{
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				Status: "completed",
				Result: "base64-image-data",
			},
		},
		Status: json.RawMessage(`"completed"`),
	}

	chatResp, _, err := ResponsesResponseToChatCompletionsResponse(resp, "chatcmpl_123")

	require.NoError(t, err)
	require.Equal(t, "![image](data:image/png;base64,base64-image-data)", chatResp.Choices[0].Message.Content)
}
