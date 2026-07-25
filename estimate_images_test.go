package pi

import (
	"testing"

	llm "github.com/dev-resolute/resolute-llm-go"
)

func TestEstimateTokensCountsImagesFlat(t *testing.T) {
	plain := NewText("user", "hi")
	withImg := NewText("user", "hi")
	withImg.Images = []llm.ImageContent{{Data: make([]byte, 100_000), MimeType: "image/png"}}

	base := EstimateTokens([]Message{plain})
	got := EstimateTokens([]Message{withImg})
	want := base + 4800/4 // 1200 tokens per image, independent of byte size
	if got != want {
		t.Errorf("EstimateTokens with image = %d, want %d (base %d + 1200)", got, want, base)
	}
}

func TestEstimateMessageTokensCountsImagesFlat(t *testing.T) {
	plain := NewText("user", "hi")
	withImg := NewText("user", "hi")
	withImg.Images = []llm.ImageContent{{Data: make([]byte, 100_000), MimeType: "image/png"}}

	base := estimateMessageTokens(plain)
	got := estimateMessageTokens(withImg)
	want := base + 1200 // 1200 tokens per image (4800 chars / 4), independent of byte size
	if got != want {
		t.Errorf("estimateMessageTokens with image = %d, want %d (base %d + 1200)", got, want, base)
	}
}
