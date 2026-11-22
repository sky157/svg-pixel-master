package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"svg-master/internal/config"
	"svg-master/internal/llm"
)

// fakeLayerBridge 内存图层工作区，用来验证多图层模式的分层交付行为。
type fakeLayerBridge struct {
	width, height int
	viewBox       string
	layers        []LayerInfo
	content       map[string]string
	active        string
	seq           int
}

func newFakeLayerBridge() *fakeLayerBridge {
	b := &fakeLayerBridge{
		width: 1024, height: 1024, viewBox: "0 0 1024 1024",
		content: map[string]string{},
	}
	b.layers = []LayerInfo{{ID: "L1", Name: "图层 1", Visible: true, Opacity: 1, Blend: "normal", Index: 1}}
	b.content["L1"] = ""
	b.active = "L1"
	return b
}

func (b *fakeLayerBridge) Canvas() (int, int, string) { return b.width, b.height, b.viewBox }
func (b *fakeLayerBridge) List() []LayerInfo {
	out := make([]LayerInfo, len(b.layers))
	copy(out, b.layers)
	return out
}
func (b *fakeLayerBridge) Active() string { return b.active }

func (b *fakeLayerBridge) find(id string) (LayerInfo, bool) {
	for _, l := range b.layers {
		if l.ID == id {
			return l, true
		}
	}
	return LayerInfo{}, false
}

func (b *fakeLayerBridge) Read(id string) (LayerInfo, string, error) {
	if id == "" {
		id = b.active
	}
	l, ok := b.find(id)
	if !ok {
		return LayerInfo{}, "", errLayerNotFound
	}
	return l, b.content[id], nil
}

func (b *fakeLayerBridge) Add(name, content, blend string, index int) (LayerInfo, error) {
	b.seq++
	l := LayerInfo{
		ID:      "L" + string(rune('1'+b.seq)),
		Name:    name,
		Visible: true, Opacity: 1, Blend: "normal", Index: b.seq + 1,
	}
	if blend != "" {
		l.Blend = blend
	}
	b.layers = append(b.layers, l)
	b.content[l.ID] = content
	b.active = l.ID
	return l, nil
}

func (b *fakeLayerBridge) Write(id, content string) error {
	if id == "" {
		id = b.active
	}
	if _, ok := b.find(id); !ok {
		return errLayerNotFound
	}
	b.content[id] = content
	return nil
}

func (b *fakeLayerBridge) Patch(id, oldText, newText string, replaceAll bool) (int, error) {
	if id == "" {
		id = b.active
	}
	if _, ok := b.find(id); !ok {
		return 0, errLayerNotFound
	}
	cur := b.content[id]
	if replaceAll {
		n := strings.Count(cur, oldText)
		b.content[id] = strings.ReplaceAll(cur, oldText, newText)
		return n, nil
	}
	if !strings.Contains(cur, oldText) {
		return 0, errLayerNotFound
	}
	b.content[id] = strings.Replace(cur, oldText, newText, 1)
	return 1, nil
}

func (b *fakeLayerBridge) Delete(id string) error {
	for i, l := range b.layers {
		if l.ID == id {
			b.layers = append(b.layers[:i], b.layers[i+1:]...)
			delete(b.content, id)
			return nil
		}
	}
	return errLayerNotFound
}

func (b *fakeLayerBridge) Props(id string, p LayerPatch) (LayerInfo, error) {
	for i := range b.layers {
		if b.layers[i].ID != id {
			continue
		}
		if p.Name != nil {
			b.layers[i].Name = *p.Name
		}
		if p.Visible != nil {
			b.layers[i].Visible = *p.Visible
		}
		return b.layers[i], nil
	}
	return LayerInfo{}, errLayerNotFound
}

func (b *fakeLayerBridge) SetActive(id string) error {
	if _, ok := b.find(id); !ok {
		return errLayerNotFound
	}
	b.active = id
	return nil
}

func (b *fakeLayerBridge) Render() string {
	var body strings.Builder
	for _, l := range b.layers {
		if !l.Visible {
			continue
		}
		body.WriteString(b.content[l.ID])
	}
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="` + b.viewBox + `">` + body.String() + `</svg>`
}

var errLayerNotFound = &layerNotFoundError{}

type layerNotFoundError struct{}

func (*layerNotFoundError) Error() string { return "找不到图层" }

// layersDeps 多图层模式的依赖。
func layersDeps(c *fakeClient, br LayerBridge, emit func(Event)) Deps {
	deps := testDeps(c, emit)
	deps.Settings.GenMode = config.GenModeLayers
	deps.Settings.MultiStep = true
	deps.LayerTools = true
	deps.Layers = br
	return deps
}

// 多图层模式的系统提示词必须要求分层交付，而不是「emit_svg 一次给全」。
func TestLayersPromptRequiresLayering(t *testing.T) {
	canvas := config.CanvasPreset{Name: "1:1 方形", Width: 1024, Height: 1024, ViewBox: "0 0 1024 1024"}
	sys := buildLayersSystemPrompt(canvas)

	if !strings.Contains(sys, "create_layer") || !strings.Contains(sys, "必须分层") {
		t.Fatalf("多图层提示词必须要求用图层工具分层交付：\n%s", sys)
	}
	// 单图模式的「唯一交付通道是 emit_svg 一次给全」不能同时出现，
	// 否则模型会继续走最简单的路径，多图层模式退化成单图层。
	if strings.Contains(sys, "唯一交付通道是 emit_svg 工具，一次给全") {
		t.Fatalf("多图层提示词不能保留单图输出规范：\n%s", sys)
	}
	if !strings.Contains(sys, "0 0 1024 1024") {
		t.Fatalf("多图层提示词仍须给出画布尺寸：\n%s", sys)
	}
}

// 多图层模式下模型拿到的是分层版提示词与分层版 emit_svg 描述。
func TestLayersModeFeedsLayeringConstraints(t *testing.T) {
	br := newFakeLayerBridge()
	c := &fakeClient{steps: []fakeStep{
		toolStep("c1", ToolCreateLayer, map[string]any{"name": "背景", "content": `<rect width="1024" height="1024" fill="#fff"/>`}),
		textStep("完成"),
	}}
	recorder := &recordingClient{inner: c}
	deps := layersDeps(c, br, nil)
	deps.Client = recorder
	if _, err := New(deps).Run(context.Background(), Task{Prompt: "画个背景"}); err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if len(recorder.requests) == 0 {
		t.Fatal("未发起模型请求")
	}
	req := recorder.requests[0]
	if !strings.Contains(req.System, "必须分层") {
		t.Fatalf("多图层模式未注入分层要求：\n%s", req.System)
	}
	var emit llm.ToolSpec
	for _, s := range req.Tools {
		if s.Name == ToolEmitSVG {
			emit = s
		}
	}
	if !strings.Contains(emit.Description, "当前图层") {
		t.Fatalf("多图层模式下 emit_svg 描述应说明只覆盖当前图层：%q", emit.Description)
	}
}

// 多图层模式下 emit_svg 只覆盖当前图层，不能吞掉其它图层。
func TestEmitSVGInLayersModeWritesActiveLayerOnly(t *testing.T) {
	br := newFakeLayerBridge()
	if _, err := br.Add("背景", `<rect width="1024" height="1024" fill="#fff"/>`, "", 1); err != nil {
		t.Fatal(err)
	}
	// 当前层是刚建的背景层；再建一个主体层并切回去，验证 emit_svg 只动当前层。
	if _, err := br.Add("主体", `<circle cx="10" cy="10" r="5"/>`, "", 2); err != nil {
		t.Fatal(err)
	}
	if err := br.SetActive("L2"); err != nil { // 背景层
		t.Fatal(err)
	}
	body := `<rect width="1024" height="1024" fill="#123456"/>`

	c := &fakeClient{steps: []fakeStep{
		toolStep("c1", ToolEmitSVG, map[string]string{"svg": body}),
		textStep("完成"),
	}}
	s := &sink{}
	if _, err := New(layersDeps(c, br, s.emit)).Run(context.Background(), Task{Prompt: "重画背景"}); err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}

	if got := br.content["L2"]; got != body {
		t.Fatalf("当前图层内容 = %q，期望 %q", got, body)
	}
	if got := br.content["L3"]; !strings.Contains(got, "circle") {
		t.Fatalf("其它图层被改写：%q", got)
	}
	if len(br.layers) != 3 {
		t.Fatalf("emit_svg 不应新建图层，实际 %d 层", len(br.layers))
	}
	// 画布上必须同时看到两层内容。
	svgEv, ok := s.find(EvSVG)
	if !ok || !strings.Contains(svgEv.SVG, "circle") || !strings.Contains(svgEv.SVG, "#123456") {
		t.Fatalf("合成画面应包含全部可见图层：%q", svgEv.SVG)
	}
}

// 多图层模式：模型只用 create_layer 交付、收尾没有正文时，必须算完成而不是空回复重试。
func TestLayersDeliveryWithoutTrailingTextFinishes(t *testing.T) {
	br := newFakeLayerBridge()
	c := &fakeClient{steps: []fakeStep{
		toolStep("c1", ToolCreateLayer, map[string]any{"name": "背景", "content": `<rect width="1024" height="1024" fill="#fff"/>`}),
		textStep(""),
	}}
	s := &sink{}
	stats, err := New(layersDeps(c, br, s.emit)).Run(context.Background(), Task{Prompt: "画个背景"})
	if err != nil {
		t.Fatalf("分层交付后空收尾不应报错：%v", err)
	}
	if c.callCount() != 2 || stats.StreamRetry != 0 {
		t.Fatalf("分层交付后仍被判成空回复重试：calls=%d stats=%+v", c.callCount(), stats)
	}
	if !strings.Contains(br.content["L2"], "rect") {
		t.Fatalf("图层内容未写入：%q", br.content["L2"])
	}
}

// 快速模式 / 动图模式仍然走「整幅 SVG」的 emit_svg 语义。
func TestEmitSVGInQuickModeKeepsWholeCanvas(t *testing.T) {
	br := newFakeLayerBridge()
	c := &fakeClient{steps: []fakeStep{
		toolStep("c1", ToolEmitSVG, map[string]string{"svg": testSVG}),
		textStep("完成"),
	}}
	s := &sink{}
	deps := testDeps(c, s.emit)
	deps.Layers = br
	deps.LayerTools = true // 即便工程就绪，快速模式也不下发图层工具
	eng := New(deps)
	if _, err := eng.Run(context.Background(), Task{Prompt: "画一个圆"}); err != nil {
		t.Fatalf("Run 返回错误：%v", err)
	}
	if got := eng.impl.snapshotSVG(); got != testSVG {
		t.Fatalf("快速模式应原样保存整幅 SVG，实际 %q", got)
	}
	if br.content["L1"] != "" {
		t.Fatalf("快速模式不应写图层：%q", br.content["L1"])
	}
	specs := toolSpecs(deps)
	for _, sp := range specs {
		if sp.Name == ToolCreateLayer {
			t.Fatal("快速模式不应下发图层工具")
		}
		if sp.Name == ToolEmitSVG && !strings.Contains(sp.Description, "唯一的交付通道") {
			t.Fatalf("快速模式的 emit_svg 应保持整幅交付语义：%q", sp.Description)
		}
	}
}

// 无事件看门狗跟随传输层设置：NoTimeout 时不该有看门狗。
func TestStreamWatchdogFollowsTransport(t *testing.T) {
	deps := testDeps(&fakeClient{}, nil)
	if got := New(deps).streamWatchdogTimeout(); got <= 0 {
		t.Fatalf("默认配置应有看门狗，实际 %v", got)
	}
	deps.Transport = llm.Transport{NoTimeout: true}
	if got := New(deps).streamWatchdogTimeout(); got != 0 {
		t.Fatalf("NoTimeout 时不应设看门狗，实际 %v", got)
	}
	deps.Transport = llm.Transport{IdleTimeout: 90 * time.Second}
	if got := New(deps).streamWatchdogTimeout(); got != 90*time.Second {
		t.Fatalf("看门狗应取传输层的空闲超时，实际 %v", got)
	}
}

// 超时设置把秒数正确换算成传输参数。
func TestTransportForTimeout(t *testing.T) {
	if got := llm.TransportForTimeout(600); got.Timeout != 10*time.Minute || got.IdleTimeout != 10*time.Minute {
		t.Fatalf("600 秒应换算成 10 分钟，实际 %+v", got)
	}
	if got := llm.TransportForTimeout(config.TimeoutSecNoLimit); !got.NoTimeout {
		t.Fatalf("-1 应关闭超时，实际 %+v", got)
	}
	if got := llm.TransportForTimeout(0); got.NoTimeout || got.IdleTimeout != 10*time.Minute {
		t.Fatalf("0 应回落到默认值，实际 %+v", got)
	}
}
