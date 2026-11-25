package agent

import (
	"svg-master/internal/llm"
	"svg-master/internal/pixel"
)

// PixelBridge gives the model access to the current pixel project.
// Implementations enforce project revisions, layer locks and restricted selections.
type PixelBridge interface {
	CanvasInfo() PixelCanvasInfo
	SetPalette(colors []string, mode string, maxColors int) ([]string, error)
	CreateLayer(name string, index int, blend string) (LayerInfo, error)
	SetLayer(id string, patch LayerPatch) (LayerInfo, error)
	DeleteLayer(id string) error
	Draw(layerID string, ops []pixel.DrawOp, clip *pixel.Selection) (pixel.DrawSummary, error)
	ReadRegion(layerID string, region pixel.Rect) (pixel.RegionView, error)
	Inspect(opts pixel.InspectOpts) (pngBase64 string, stats pixel.InspectStats, err error)
	CompositePNG() (pngBase64 string, err error)
}

type PixelCanvasInfo struct {
	Width        int              `json:"width"`
	Height       int              `json:"height"`
	Palette      []string         `json:"palette"`
	Layers       []LayerInfo      `json:"layers"`
	ActiveLayer  string           `json:"activeLayer"`
	Selection    *pixel.Selection `json:"selection,omitempty"`
	HasMainImage bool             `json:"hasMainImage"`
	RefIDs       []string         `json:"refIds"`
}

type PixelInput struct {
	DraftMode           string
	MainImage           *llm.ImageData
	RefImages           []PixelReference
	HumanEdits          string
	Selection           *pixel.Selection
	RestrictToSelection bool
	Composite           *llm.ImageData
}

type PixelReference struct {
	ID     string
	Number int
	Name   string
	Image  llm.ImageData
}
