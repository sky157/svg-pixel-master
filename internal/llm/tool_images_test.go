package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestToolImageProtocolEncoding(t *testing.T) {
	msg := Message{Role: RoleTool, Blocks: []Block{
		{Type: BlockToolResult, ToolResult: &ToolResult{
			CallID: "check", Name: "pixel_inspect", Content: "canvas", Images: []ImageData{{Data: "aW1hZ2U="}},
		}},
		{Type: BlockToolResult, ToolResult: &ToolResult{CallID: "write", Name: "pixel_draw", Content: "painted"}},
	}}
	imgURL := "data:image/png;base64,aW1hZ2U="

	t.Run("chat", func(t *testing.T) {
		out := encodeChatMessage(msg)
		if len(out) != 3 || out[0].ToolCallID != "check" || out[1].ToolCallID != "write" || out[2].Role != RoleUser {
			t.Fatalf("tool calls must be contiguous before their images: %+v", out)
		}
		parts, ok := out[2].Content.([]chatPartOut)
		if !ok || len(parts) != 2 || !strings.Contains(parts[0].Text, "check") || parts[1].ImageURL == nil || parts[1].ImageURL.URL != imgURL {
			t.Fatalf("incorrect tool image or caption: %+v", out[2])
		}
	})

	t.Run("responses", func(t *testing.T) {
		out := encodeRespMessage(msg, false)
		if len(out) != 2 || out[0].CallID != "check" || out[1].Output != "painted" {
			t.Fatalf("incorrect tool result ordering or text: %+v", out)
		}
		parts, ok := out[0].Output.([]respContentPart)
		if !ok || len(parts) != 2 || parts[0].Text != "canvas" || parts[1].ImageURL != imgURL {
			t.Fatalf("image tool result should be a part array: %+v", out[0])
		}
		legacy := encodeRespMessage(msg, true)
		if len(legacy) != 3 || legacy[0].Output != "canvas" || legacy[1].Output != "painted" ||
			legacy[2].Role != RoleUser || len(legacy[2].Content) != 2 || legacy[2].Content[1].ImageURL != imgURL {
			t.Fatalf("incorrect legacy tool result ordering: %+v", legacy)
		}
	})

	t.Run("gemini", func(t *testing.T) {
		newer, err := encodeGeminiMessage(msg, nil, true)
		if err != nil || len(newer) != 1 {
			t.Fatalf("could not encode native function response: %+v %v", newer, err)
		}
		parts := newer[0].Parts.([]geminiPartOut)
		check := parts[0].FunctionResponse
		if len(parts) != 2 || check.ID != "check" || len(check.Parts) != 1 || len(parts[1].FunctionResponse.Parts) != 0 {
			t.Fatalf("incorrect native function response: %+v", parts)
		}
		media := check.Parts[0].InlineData
		refs, ok := check.Response["images"].([]map[string]string)
		if !ok || len(refs) != 1 || refs[0]["$ref"] != media.DisplayName || media.Data != "aW1hZ2U=" {
			t.Fatalf("incorrect bound image parts: %+v", check)
		}
		older, err := encodeGeminiMessage(msg, nil, false)
		if err != nil || len(older) != 1 {
			t.Fatalf("could not encode legacy function response: %+v %v", older, err)
		}
		parts = older[0].Parts.([]geminiPartOut)
		if len(parts) != 4 || parts[0].FunctionResponse == nil || parts[1].FunctionResponse == nil ||
			parts[2].Text == "" || parts[3].InlineData == nil || parts[3].InlineData.Data != "aW1hZ2U=" {
			t.Fatalf("legacy image must follow all tool results: %+v", parts)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		out, ok := encodeAnthropicMessage(msg)
		if !ok {
			t.Fatal("missing tool response")
		}
		encoded, err := json.Marshal(out)
		if err != nil || !strings.Contains(string(encoded), `"type":"image"`) || !strings.Contains(string(encoded), `"data":"aW1hZ2U="`) {
			t.Fatalf("unexpected Anthropic response: %s %v", encoded, err)
		}
	})
	if msg.Blocks[0].ToolResult.Images[0].Data != "aW1hZ2U=" {
		t.Fatal("encoding must leave shared history untouched")
	}
}

func TestPixelCachePrefixProtocolEncoding(t *testing.T) {
	req := Request{
		Pixel: true, PixelAssetPrefix: true, Model: "gpt-6.2-sol", System: "drawing rules",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "reference"},
				{Type: BlockImage, Image: &ImageData{Data: "aW1hZ2U="}},
			}},
			TextMsg(RoleUser, "new drawing instructions"),
		},
	}
	raw, err := (&openaiResponses{}).buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var responses respRequestOut
	if err := json.Unmarshal(raw, &responses); err != nil {
		t.Fatal(err)
	}
	if responses.Instructions != "" || len(responses.Input) != 3 {
		t.Fatalf("expected rules, assets and current user message: %s", raw)
	}
	for i, want := range []string{"drawing rules", "aW1hZ2U=", "new drawing instructions"} {
		if !strings.Contains(string(responses.Input[i]), want) {
			t.Fatalf("input item %d should contain %q: %s", i, want, responses.Input[i])
		}
	}
	if !strings.Contains(string(responses.Input[0]), `"role":"developer"`) {
		t.Fatal("drawing rules lost developer priority")
	}

	raw, err = (&anthropicClient{}).buildBody(req)
	if err != nil {
		t.Fatal(err)
	}
	var anthropic struct {
		Messages []struct{ Content []anthropicBlockOut }
	}
	if err := json.Unmarshal(raw, &anthropic); err != nil {
		t.Fatal(err)
	}
	if len(anthropic.Messages) != 2 || len(anthropic.Messages[0].Content) != 2 {
		t.Fatalf("unexpected Anthropic message layout: %s", raw)
	}
	lastAsset := anthropic.Messages[0].Content[1]
	if lastAsset.Type != "image" || lastAsset.CacheControl == nil || lastAsset.CacheControl.Type != "ephemeral" {
		t.Fatalf("missing stable cache breakpoint after project assets: %s", raw)
	}
}

func TestToolImageResponsesGatewayFallback(t *testing.T) {
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Output  json.RawMessage `json:"output"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			if len(request.Input) != 1 || !strings.HasPrefix(string(request.Input[0].Output), "[") {
				t.Errorf("first request must use a native function-call result array: %+v", request)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"input[0].output expected a string, got array"}}`)
			return
		}
		if len(request.Input) != 2 || string(request.Input[0].Output) != `"canvas"` || request.Input[1].Role != RoleUser ||
			!strings.Contains(string(request.Input[1].Content), "data:image/png;base64,aW1hZ2U=") {
			t.Errorf("fallback lost the textual function result or following image: %+v", request)
		}
		_, _ = io.WriteString(w, `{"status":"completed","output":[]}`)
	}))
	defer gateway.Close()
	client := &openaiResponses{base: gateway.URL, key: "test"}
	stream, err := client.Stream(context.Background(), Request{Messages: []Message{{Role: RoleTool, Blocks: []Block{{
		Type: BlockToolResult,
		ToolResult: &ToolResult{
			CallID: "call_1", Name: "pixel_inspect", Content: "canvas", Images: []ImageData{{Data: "aW1hZ2U="}},
		},
	}}}}})
	if err != nil {
		t.Fatal(err)
	}
	done := false
	for event := range stream {
		if event.Kind == EvError {
			t.Fatal(event.Err)
		}
		done = done || event.Kind == EvDone
	}
	if !done || calls.Load() != 2 {
		t.Fatalf("expected a single compatibility retry and completion, got %d requests, done=%t", calls.Load(), done)
	}
}
