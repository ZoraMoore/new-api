package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cancelOnWriteResponseWriter struct {
	gin.ResponseWriter
	needle string
	cancel context.CancelFunc
	once   sync.Once
}

func (w *cancelOnWriteResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if strings.Contains(string(data), w.needle) {
		w.once.Do(w.cancel)
	}
	return n, err
}

func TestImageGenerationResultMarkdown(t *testing.T) {
	require.Equal(t, "![image](data:image/png;base64,abc123)", imageGenerationResultMarkdown(" abc123 "))
	require.Empty(t, imageGenerationResultMarkdown(" "))
}

func TestOaiResponsesToChatStreamHandlerReturnsPartialUsageWhenClientCancels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	ctx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	c.Writer = &cancelOnWriteResponseWriter{
		ResponseWriter: c.Writer,
		needle:         "partial response text",
		cancel:         cancel,
	}

	info := &relaycommon.RelayInfo{
		OriginModelName:    "gpt-5.4",
		RelayFormat:        types.RelayFormatOpenAI,
		ShouldIncludeUsage: true,
		IsStream:           true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5.4",
		},
	}
	info.SetEstimatePromptTokens(12)

	body := strings.NewReader(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_test","model":"gpt-5.4","created_at":1770000000}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"partial response text"}`,
		"",
	}, "\n"))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(body),
	}

	usage, apiErr := OaiResponsesToChatStreamHandler(c, info, resp)

	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	require.Equal(t, 12, usage.PromptTokens)
	require.Greater(t, usage.CompletionTokens, 0)
	require.Contains(t, recorder.Body.String(), "partial response text")
}
