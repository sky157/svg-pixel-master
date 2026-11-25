package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"svg-master/internal/llm"
	"svg-master/internal/pixel"
)

func TestPixelAssetPrefixAcrossTurns(t *testing.T) {
	ctx := context.Background()
	bridge := &pixelTestBridge{}
	ref := PixelReference{ID: "ref_7", Number: 7, Name: "角色", Image: llm.ImageData{Data: "reference-image"}}
	input := PixelInput{
		DraftMode: "vision",
		MainImage: &llm.ImageData{Data: "main-image"},
		RefImages: []PixelReference{ref},
		Composite: &llm.ImageData{Data: "old-canvas"},
	}
	first := runPixelCacheTurn(t, ctx, bridge, Task{Prompt: "画初稿", Pixel: &input})

	bridge.width, bridge.height = 200, 96
	input.Composite = &llm.ImageData{Data: "new-canvas"}
	input.HumanEdits = "人物层的手部新增蓝色"
	input.Selection = &pixel.Selection{Rect: pixel.Rect{X: 9, Y: 3, W: 5, H: 6}}
	input.RestrictToSelection = true
	second := runPixelCacheTurn(t, ctx, bridge, Task{Prompt: "精修手部", Pixel: &input, History: first.history})

	firstReq, secondReq := first.requests[0], second.requests[0]
	if firstReq.System != secondReq.System || !reflect.DeepEqual(firstReq.Tools, secondReq.Tools) ||
		!reflect.DeepEqual(firstReq.Messages[0], secondReq.Messages[0]) {
		t.Fatal("prompt, edit, selection or canvas changes invalidated the stable request prefix")
	}
	checkPixelAssetLabels(t, firstReq.Messages[0], "【主图", "参考图 #7")
	if secondReq.Messages[0].Role != llm.RoleUser {
		t.Fatalf("expected the asset prefix before the conversation, got role %q", secondReq.Messages[0].Role)
	}
	for _, result := range []pixelCacheTurn{first, second} {
		for _, req := range result.requests {
			if !reflect.DeepEqual(firstReq.Messages[0], req.Messages[0]) {
				t.Fatal("asset prefix changed during the tool loop")
			}
			names := pixelMessageImages(req.Messages)
			if names["main-image"] != 1 || names["reference-image"] != 1 {
				t.Fatalf("each request should include each stable asset once, got images %v", names)
			}
		}
		hasUserInstruction := false
		for _, message := range result.history {
			if message.Role != llm.RoleUser {
				continue
			}
			if pixelUserImageBytes(message) != 0 || strings.Contains(message.Text(), "【主图") ||
				strings.Contains(message.Text(), "参考图 #7") {
				t.Fatal("asset prefix or historical image bytes leaked into saved conversation history")
			}
			hasUserInstruction = hasUserInstruction || strings.Contains(message.Text(), "[用户指令]")
		}
		if !hasUserInstruction {
			t.Fatal("the user instruction was lost while omitting asset payloads")
		}
	}

	for _, req := range second.requests {
		names := pixelMessageImages(req.Messages)
		if names["old-canvas"] != 0 || names["new-canvas"] != 1 {
			t.Fatalf("subsequent requests should include only the current canvas snapshot, got images %v", names)
		}
	}
	latest := secondReq.Messages[len(secondReq.Messages)-1]
	checkPixelAssetLabels(t, latest, "当前工程的合成画布")
	for _, expected := range []string{"精修手部", "200×96", "人物层的手部新增蓝色", "x=9", "y=3", "w=5", "h=6"} {
		if !strings.Contains(latest.Text(), expected) {
			t.Errorf("current user message is missing %q", expected)
		}
		if strings.Contains(secondReq.Messages[0].Text(), expected) {
			t.Errorf("dynamic field %q leaked into the stable asset prefix", expected)
		}
	}
}

func TestPixelTurnWithoutAssetsOmitsAssetPrefix(t *testing.T) {
	result := runPixelCacheTurn(t, context.Background(), &pixelTestBridge{}, Task{
		Prompt: "画一只狐狸", Pixel: &PixelInput{DraftMode: "prompt"},
	})
	if req := result.requests[0]; len(req.Messages) != 1 || !strings.Contains(req.Messages[0].Text(), "画一只狐狸") {
		t.Fatalf("expected the first message to be the user instruction with no added asset message: %+v", req.Messages)
	}
}

func TestPixelAssetBytesCountTowardImageBudget(t *testing.T) {
	main := &llm.ImageData{Data: strings.Repeat("A", maxPixelInputImageBytes-maxPixelToolImageBytes)}
	canvas := &llm.ImageData{Data: strings.Repeat("B", maxPixelToolImageBytes)}
	inspection := strings.Repeat("C", maxPixelToolImageBytes)
	input := PixelInput{MainImage: main, Composite: canvas}
	fake := &fakeClient{steps: []fakeStep{
		toolStep("inspect_first", ToolPixelInspect, pixel.InspectOpts{Mode: "composite"}),
		toolStep("inspect_next", ToolPixelInspect, pixel.InspectOpts{Mode: "crop"}),
		textStep("已检查。"),
	}}
	recorder := &recordingClient{inner: fake}
	deps := testDeps(fake, nil)
	deps.Client, deps.Pixel, deps.Vision = recorder, &pixelTestBridge{inspectImage: inspection}, true
	if _, err := New(deps).Run(context.Background(), Task{Prompt: "检查", Pixel: &input}); err != nil {
		t.Fatal(err)
	}
	req := recorder.requests[len(recorder.requests)-1]
	results := pixelTestResults(req.Messages)
	if len(results) != 2 || results[0].CallID != "inspect_first" || len(results[0].Images) != 0 ||
		results[1].CallID != "inspect_next" || len(results[1].Images) != 1 {
		t.Fatal("the image budget should omit only the earlier inspection image and preserve both tool results")
	}
	total := 0
	for _, message := range req.Messages {
		total += pixelUserImageBytes(message)
	}
	total += len(results[1].Images[0].Data)
	if total != maxPixelRequestImageBytes {
		t.Fatalf("expected assets, canvas and latest inspection to use exactly the request budget, got %d bytes", total)
	}

	input.Composite = &llm.ImageData{Data: canvas.Data + "X"}
	engine := New(deps)
	history := []llm.Message{llm.TextMsg(llm.RoleUser, "上一轮")}
	before := len(recorder.requests)
	_, err := engine.Run(context.Background(), Task{Prompt: "检查", Pixel: &input, History: history})
	if err == nil || !strings.Contains(err.Error(), "超出模型请求预算") || len(recorder.requests) != before {
		t.Fatalf("oversized assets and canvas should be rejected before sending a model request: %v", err)
	}
	if !reflect.DeepEqual(engine.History(), history) {
		t.Fatal("the rejected request lost the previous conversation")
	}
}

type pixelCacheTurn struct {
	requests []llm.Request
	history  []llm.Message
}

func runPixelCacheTurn(t *testing.T, ctx context.Context, bridge PixelBridge, task Task) pixelCacheTurn {
	t.Helper()
	fake := &fakeClient{steps: []fakeStep{
		toolStep("inspect", ToolPixelInspect, pixel.InspectOpts{Mode: "composite"}),
		textStep("已检查画布。"),
	}}
	recorder := &recordingClient{inner: fake}
	deps := testDeps(fake, nil)
	deps.Client, deps.Pixel, deps.Vision = recorder, bridge, true
	deps.Settings.CompressOn = true
	engine := New(deps)
	if _, err := engine.Run(ctx, task); err != nil {
		t.Fatal(err)
	}
	if len(recorder.requests) != 2 {
		t.Fatalf("expected initial and tool continuation requests, got %d", len(recorder.requests))
	}
	return pixelCacheTurn{requests: recorder.requests, history: engine.History()}
}

func pixelMessageImages(messages []llm.Message) map[string]int {
	images := make(map[string]int)
	for _, message := range messages {
		for _, block := range message.Blocks {
			if block.Image != nil {
				images[block.Image.Data]++
			}
		}
	}
	return images
}

func checkPixelAssetLabels(t *testing.T, message llm.Message, expected ...string) {
	t.Helper()
	var labels []string
	for i, block := range message.Blocks {
		if block.Type != llm.BlockImage {
			continue
		}
		if i == 0 || message.Blocks[i-1].Type != llm.BlockText {
			t.Fatal("an image has no adjacent role label")
		}
		labels = append(labels, message.Blocks[i-1].Text)
	}
	if len(labels) != len(expected) {
		t.Fatalf("expected image labels %v, got %v", expected, labels)
	}
	for i, label := range labels {
		if !strings.Contains(label, expected[i]) {
			t.Errorf("image label %q should include %q", label, expected[i])
		}
	}
}
