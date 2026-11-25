package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"svg-master/internal/llm"
	"svg-master/internal/pixel"
)

const (
	ToolPixelCanvasInfo  = "pixel_canvas_info"
	ToolPixelPalette     = "pixel_palette"
	ToolPixelCreateLayer = "pixel_create_layer"
	ToolPixelSetLayer    = "pixel_set_layer"
	ToolPixelDeleteLayer = "pixel_delete_layer"
	ToolPixelDraw        = "pixel_draw"
	ToolPixelReadRegion  = "pixel_read_region"
	ToolPixelInspect     = "pixel_inspect"
)

func pixelToolSpecs() []llm.ToolSpec {
	return []llm.ToolSpec{
		{
			Name: ToolPixelCanvasInfo, Description: "读取画布尺寸、图层、调色板、选区和参考图 ID。",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		},
		{
			Name: ToolPixelPalette, Description: "设置、追加调色板，或者从主图提取颜色。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"action":{"type":"string","enum":["set","add","extract_from_main"]},
				"colors":{"type":"array","items":{"type":"string"}},
				"maxColors":{"type":"integer","minimum":4,"maximum":64,"description":"主图提色的最大颜色数，默认 16"}
			},"required":["action"]}`),
		},
		{
			Name: ToolPixelCreateLayer, Description: "创建透明像素图层并设为当前图层。返回图层 ID，后续用 pixel_draw 绘制。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"name":{"type":"string"},"index":{"type":"integer"},"blend":{"type":"string"}
			},"required":["name"]}`),
		},
		{
			Name: ToolPixelSetLayer, Description: "修改未锁定像素图层的属性。省略 layerId 时使用当前图层。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"layerId":{"type":"string"},"name":{"type":"string"},"visible":{"type":"boolean"},
				"locked":{"type":"boolean"},"opacity":{"type":"number","minimum":0,"maximum":1},
				"blend":{"type":"string"},"index":{"type":"integer"}
			}}`),
		},
		{
			Name: ToolPixelDeleteLayer, Description: "删除指定的未锁定图层。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"layerId":{"type":"string"}},"required":["layerId"]}`),
		},
		{
			Name:        ToolPixelDraw,
			Description: "在未锁定图层按顺序批量绘制。坐标从 0 开始，x 向右、y 向下，颜色为 #RRGGBB 或 #RRGGBBAA。每种操作只填写所需字段。rect/erase_rect/dither_rect/mirror 用 x,y,w,h，line 用 x0,y0,x1,y1，ellipse 用 cx,cy,rx,ry，flood_fill/pixel 用 x,y。rect/ellipse/polygon 用 fill 控制填充；blit 用 x,y,legend,rows，点号和空格跳过，单字符映射到颜色，可用 rle:2.3K2.；outline 用 color 与 connectivity；replace_color 用 from,to。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"layerId":{"type":"string"},"clipToSelection":{"type":"boolean"},"note":{"type":"string"},
				"ops":{"type":"array","minItems":1,"items":{"type":"object","properties":{
					"op":{"type":"string","enum":["clear","pixel","rect","line","ellipse","polygon","flood_fill","erase_rect","blit","dither_rect","outline","replace_color","mirror"]},
					"x":{"type":"integer"},"y":{"type":"integer"},"w":{"type":"integer"},"h":{"type":"integer"},
					"x0":{"type":"integer"},"y0":{"type":"integer"},"x1":{"type":"integer"},"y1":{"type":"integer"},
					"cx":{"type":"integer"},"cy":{"type":"integer"},"rx":{"type":"integer"},"ry":{"type":"integer"},
					"color":{"type":"string"},"colorA":{"type":"string"},"colorB":{"type":"string"},
					"from":{"type":"string"},"to":{"type":"string"},"fill":{"type":"boolean"},
					"thickness":{"type":"integer"},"brush":{"type":"string","enum":["round","square"]},
					"points":{"type":"array","items":{"type":"array","items":{"type":"integer"},"minItems":2,"maxItems":2}},
					"tolerance":{"type":"integer"},"contiguous":{"type":"boolean"},
					"legend":{"type":"object","additionalProperties":{"type":"string"}},"rows":{"type":"array","items":{"type":"string"}},
					"pattern":{"type":"string","enum":["checker","bayer2","bayer4","hgrad","vgrad"]},"ratio":{"type":"number","minimum":0,"maximum":1},
					"connectivity":{"type":"integer","enum":[4,8]},"axis":{"type":"string","enum":["x","y"]}
				},"required":["op"]}}
			},"required":["ops"]}`),
		},
		{
			Name: ToolPixelReadRegion, Description: "精确读取最多 32×32 的像素与颜色映射。layerId 可用 composite 读取合成画布。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"layerId":{"type":"string"},"x":{"type":"integer"},"y":{"type":"integer"},
				"w":{"type":"integer","minimum":1,"maximum":32},"h":{"type":"integer","minimum":1,"maximum":32}
			},"required":["x","y","w","h"]}`),
		},
		{
			Name: ToolPixelInspect, Description: "自检当前合成画布、单层、放大区域，或与主图/指定参考图逐区对照。compare_ref 需传 refId。大图局部检查用 crop。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"mode":{"type":"string","enum":["composite","layer","crop","compare_main","compare_ref"]},
				"layerId":{"type":"string"},"refId":{"type":"string"},
				"region":{"type":"object","properties":{"x":{"type":"integer"},"y":{"type":"integer"},"w":{"type":"integer"},"h":{"type":"integer"}},"required":["x","y","w","h"]},
				"showGrid":{"type":"boolean"},"fit":{"type":"string","enum":["fit","cover","stretch"]}
			},"required":["mode"]}`),
		},
	}
}

func isPixelTool(name string) bool {
	switch name {
	case ToolPixelCanvasInfo, ToolPixelPalette, ToolPixelCreateLayer, ToolPixelSetLayer, ToolPixelDeleteLayer, ToolPixelDraw, ToolPixelReadRegion, ToolPixelInspect:
		return true
	}
	return false
}

func (e *Engine) runPixelTool(call llm.ToolCall, args json.RawMessage, input PixelInput) toolOutcome {
	bridge := e.deps.Pixel
	if input.RestrictToSelection && (call.Name == ToolPixelPalette || call.Name == ToolPixelSetLayer || call.Name == ToolPixelDeleteLayer) {
		return toolOutcome{Content: "本轮只允许在选区内绘制，不能修改整层属性或全局调色板。", IsError: true}
	}
	switch call.Name {
	case ToolPixelCanvasInfo:
		info := bridge.CanvasInfo()
		if input.Selection != nil {
			info.Selection = input.Selection
		}
		// A wand can select thousands of disjoint runs. Its exact mask stays in the project operation.
		mask := info.Selection != nil && info.Selection.Runs != nil
		if mask {
			info.Selection = &pixel.Selection{Rect: info.Selection.Rect}
		}
		return pixelJSON(struct {
			PixelCanvasInfo
			SelectionMask bool `json:"nonRectangularSelection,omitempty"`
		}{info, mask})
	case ToolPixelPalette:
		var in struct {
			Action    string   `json:"action"`
			Colors    []string `json:"colors"`
			MaxColors int      `json:"maxColors"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return argErr(call.Name, err)
		}
		switch in.Action {
		case "set", "add", "extract_from_main":
		default:
			return toolOutcome{Content: "action 必须是 set、add 或 extract_from_main。", IsError: true}
		}
		if in.MaxColors == 0 {
			in.MaxColors = 16
		}
		palette, err := bridge.SetPalette(in.Colors, in.Action, min(64, max(4, in.MaxColors)))
		if err != nil {
			return layerErr(call.Name, err)
		}
		return pixelJSON(map[string]any{"palette": palette})
	case ToolPixelCreateLayer:
		var in struct {
			Name  string `json:"name"`
			Index *int   `json:"index"`
			Blend string `json:"blend"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return argErr(call.Name, err)
		}
		index := -1
		if in.Index != nil {
			index = *in.Index
		}
		layer, err := bridge.CreateLayer(strings.TrimSpace(in.Name), index, in.Blend)
		if err != nil {
			return layerErr(call.Name, err)
		}
		return pixelJSON(layer)
	case ToolPixelSetLayer:
		var in struct {
			LayerID string `json:"layerId"`
			LayerPatch
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return argErr(call.Name, err)
		}
		layer, err := bridge.SetLayer(in.LayerID, in.LayerPatch)
		if err != nil {
			return layerErr(call.Name, err)
		}
		return pixelJSON(layer)
	case ToolPixelDeleteLayer:
		var in struct {
			LayerID string `json:"layerId"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return argErr(call.Name, err)
		}
		if in.LayerID == "" {
			return toolOutcome{Content: "layerId 不能为空。", IsError: true}
		}
		if err := bridge.DeleteLayer(in.LayerID); err != nil {
			return layerErr(call.Name, err)
		}
		return pixelJSON(map[string]string{"deleted": in.LayerID})
	case ToolPixelDraw:
		var in struct {
			LayerID         string         `json:"layerId"`
			ClipToSelection bool           `json:"clipToSelection"`
			Ops             []pixel.DrawOp `json:"ops"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return argErr(call.Name, err)
		}
		if len(in.Ops) == 0 {
			return toolOutcome{Content: "ops 至少需要一条绘图指令。", IsError: true}
		}
		var clip *pixel.Selection
		if in.ClipToSelection || input.RestrictToSelection {
			clip = input.Selection
			if clip == nil {
				clip = bridge.CanvasInfo().Selection
			}
			if clip == nil {
				return toolOutcome{Content: "当前没有选区。", IsError: true}
			}
		}
		summary, err := bridge.Draw(in.LayerID, in.Ops, clip)
		if err != nil {
			return layerErr(call.Name, err)
		}
		out := pixelJSON(summary)
		out.PixelChanged = summary.ChangedPixels > 0
		return out
	case ToolPixelReadRegion:
		var in struct {
			LayerID string `json:"layerId"`
			pixel.Rect
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return argErr(call.Name, err)
		}
		if in.W < 1 || in.H < 1 || in.W > pixel.MaxReadSpan || in.H > pixel.MaxReadSpan {
			return toolOutcome{Content: "读取区域的宽和高须在 1–32 像素之间。", IsError: true}
		}
		region, err := bridge.ReadRegion(in.LayerID, in.Rect)
		if err != nil {
			return layerErr(call.Name, err)
		}
		return pixelJSON(region)
	case ToolPixelInspect:
		var opts pixel.InspectOpts
		if err := json.Unmarshal(args, &opts); err != nil {
			return argErr(call.Name, err)
		}
		return e.inspectPixel(opts)
	}
	return toolOutcome{Content: fmt.Sprintf("未知像素工具 %q", call.Name), IsError: true}
}

func (e *Engine) inspectPixel(opts pixel.InspectOpts) toolOutcome {
	bridge := e.deps.Pixel
	if opts.Mode == "" {
		opts.Mode = "composite"
	}
	png, stats, err := bridge.Inspect(opts)
	if err != nil {
		return layerErr(ToolPixelInspect, err)
	}
	if e.deps.Vision {
		if png == "" || len(png) > maxPixelToolImageBytes {
			return toolOutcome{Content: "检验图为空或体积过大，请用 crop 查看较小区域。", IsError: true}
		}
		return toolOutcome{Content: marshalJSON(stats), Images: []llm.ImageData{{MIME: "image/png", Data: png}}}
	}

	info := bridge.CanvasInfo()
	region := pixel.Rect{W: info.Width, H: info.Height}
	if opts.Region != nil {
		region = *opts.Region
	}
	region.W = min(region.W, pixel.MaxReadSpan)
	region.H = min(region.H, pixel.MaxReadSpan)
	layerID := "composite"
	if opts.Mode == "layer" || (opts.Mode == "crop" && opts.LayerID != "") {
		layerID = opts.LayerID
	}
	preview, err := bridge.ReadRegion(layerID, region)
	if err != nil {
		return layerErr(ToolPixelInspect, err)
	}
	return pixelJSON(struct {
		Stats   pixel.InspectStats `json:"stats"`
		Preview pixel.RegionView   `json:"preview"`
	}{stats, preview})
}

func pixelJSON(value any) toolOutcome {
	return toolOutcome{Content: marshalJSON(value)}
}
