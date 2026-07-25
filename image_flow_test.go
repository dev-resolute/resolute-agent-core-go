package pi

import (
	"testing"

	llm "github.com/dev-resolute/resolute-llm-go"
)

func img(b byte) llm.ImageContent { return llm.ImageContent{Data: []byte{b}, MimeType: "image/png"} }

func TestConvertUserMessageWithImages(t *testing.T) {
	m := NewText("user", "look at this")
	m.Images = []llm.ImageContent{img(1), img(2)}
	out := DefaultConvertToLLM([]Message{m})
	if len(out) != 3 {
		t.Fatalf("llm messages = %d, want 3 (text + 2 image turns)", len(out))
	}
	if _, ok := out[0].Content.(llm.TextContent); !ok {
		t.Errorf("first message not text: %T", out[0].Content)
	}
	for i := 1; i <= 2; i++ {
		ic, ok := out[i].Content.(llm.ImageContent)
		if !ok || out[i].Role != "user" {
			t.Fatalf("message %d = role %q %T, want user ImageContent", i, out[i].Role, out[i].Content)
		}
		if ic.MimeType != "image/png" {
			t.Errorf("mime = %q", ic.MimeType)
		}
	}
}

func TestConvertToolResultWithImages(t *testing.T) {
	m := NewToolResult("call_1", "read", ToolResult{
		Content: "Read image file [image/png]",
		Images:  []llm.ImageContent{img(7)},
	})
	out := DefaultConvertToLLM([]Message{m})
	trc, ok := out[0].Content.(llm.ToolResultContent)
	if !ok {
		t.Fatalf("not a tool result: %T", out[0].Content)
	}
	if len(trc.Images) != 1 || trc.Images[0].Data[0] != 7 {
		t.Errorf("Images not propagated: %+v", trc.Images)
	}
}
