package openai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageGenerationResultMarkdown(t *testing.T) {
	require.Equal(t, "![image](data:image/png;base64,abc123)", imageGenerationResultMarkdown(" abc123 "))
	require.Empty(t, imageGenerationResultMarkdown(" "))
}
