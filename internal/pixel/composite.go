package pixel

import (
	"fmt"
	"image"
	"image/color"
	"math"
)

type Layer struct {
	Image   *image.NRGBA
	Visible bool
	Opacity float64
	Blend   string
}

type blendMode uint8

const (
	normal blendMode = iota
	multiply
	screen
	overlay
	darken
	lighten
	dodge
	burn
	hardLight
	softLight
	difference
	exclusion
	hue
	saturation
	colorBlend
	luminosity
)

var blends = map[string]blendMode{
	"": normal, "normal": normal, "multiply": multiply, "screen": screen, "overlay": overlay, "darken": darken, "lighten": lighten,
	"color-dodge": dodge, "color-burn": burn, "hard-light": hardLight, "soft-light": softLight, "difference": difference,
	"exclusion": exclusion, "hue": hue, "saturation": saturation, "color": colorBlend, "luminosity": luminosity,
}

func ValidBlend(mode string) bool {
	_, ok := blends[mode]
	return ok
}

// Composite flattens straight-alpha layer pixels in bottom-to-top order using Canvas source-over and CSS blend formulas.
func Composite(w, h int, layers []Layer) (*image.NRGBA, error) {
	out, err := New(w, h)
	if err != nil {
		return nil, err
	}
	type source struct {
		img     *image.NRGBA
		opacity float64
		mode    blendMode
	}
	visible := make([]source, 0, len(layers))
	for i, l := range layers {
		if !l.Visible {
			continue
		}
		if l.Image == nil || l.Image.Bounds() != out.Bounds() {
			return nil, fmt.Errorf("图层 %d 的尺寸与画布不一致", i+1)
		}
		mode, ok := blends[l.Blend]
		if !ok {
			return nil, fmt.Errorf("未知图层混合模式 %q", l.Blend)
		}
		if math.IsNaN(l.Opacity) || l.Opacity < 0 || l.Opacity > 1 {
			return nil, fmt.Errorf("图层不透明度必须在 0 到 1 之间")
		}
		if l.Opacity > 0 {
			visible = append(visible, source{img: l.Image, opacity: l.Opacity, mode: mode})
		}
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var back [3]float64
			alpha := 0.0
			for _, l := range visible {
				c := l.img.NRGBAAt(x, y)
				if c.A == 0 {
					continue
				}
				a := float64(c.A) / 255 * l.opacity
				src := [3]float64{float64(c.R) / 255, float64(c.G) / 255, float64(c.B) / 255}
				if alpha == 0 || a == 1 && l.mode == normal {
					back, alpha = src, a
					continue
				}
				mix := blend(back, src, l.mode)
				combined := a + alpha*(1-a)
				for i, v := range src {
					back[i] = (a*((1-alpha)*v+alpha*mix[i]) + (1-a)*alpha*back[i]) / combined
				}
				alpha = combined
			}
			opaque := byteValue(alpha * 255)
			if opaque != 0 {
				out.SetNRGBA(x, y, color.NRGBA{R: byteValue(back[0] * 255), G: byteValue(back[1] * 255), B: byteValue(back[2] * 255), A: opaque})
			}
		}
	}
	return out, nil
}

func blend(back, src [3]float64, mode blendMode) [3]float64 {
	switch mode {
	case hue:
		return setLum(setSat(src, sat(back)), lum(back))
	case saturation:
		return setLum(setSat(back, sat(src)), lum(back))
	case colorBlend:
		return setLum(src, lum(back))
	case luminosity:
		return setLum(back, lum(src))
	case normal:
		return src
	}
	var out [3]float64
	for i, b := range back {
		out[i] = blendComponent(b, src[i], mode)
	}
	return out
}

func blendComponent(b, s float64, mode blendMode) float64 {
	switch mode {
	case multiply:
		return b * s
	case screen:
		return b + s - b*s
	case overlay:
		return blendComponent(s, b, hardLight)
	case darken:
		return min(b, s)
	case lighten:
		return max(b, s)
	case dodge:
		if b == 0 {
			return 0
		}
		if s == 1 {
			return 1
		}
		return min(1, b/(1-s))
	case burn:
		if b == 1 {
			return 1
		}
		if s == 0 {
			return 0
		}
		return 1 - min(1, (1-b)/s)
	case hardLight:
		if s <= 0.5 {
			return 2 * b * s
		}
		return blendComponent(b, 2*s-1, screen)
	case softLight:
		if s <= 0.5 {
			return b - (1-2*s)*b*(1-b)
		}
		d := math.Sqrt(b)
		if b <= 0.25 {
			d = ((16*b-12)*b + 4) * b
		}
		return b + (2*s-1)*(d-b)
	case difference:
		return math.Abs(b - s)
	case exclusion:
		return b + s - 2*b*s
	default:
		return s
	}
}

func lum(c [3]float64) float64 { return 0.3*c[0] + 0.59*c[1] + 0.11*c[2] }

func sat(c [3]float64) float64 { return max(c[0], c[1], c[2]) - min(c[0], c[1], c[2]) }

func setLum(c [3]float64, l float64) [3]float64 {
	d := l - lum(c)
	for i := range c {
		c[i] += d
	}
	low, high := min(c[0], c[1], c[2]), max(c[0], c[1], c[2])
	if low < 0 {
		for i := range c {
			c[i] = l + (c[i]-l)*l/(l-low)
		}
	}
	if high > 1 {
		for i := range c {
			c[i] = l + (c[i]-l)*(1-l)/(high-l)
		}
	}
	return c
}

func setSat(c [3]float64, s float64) [3]float64 {
	low, high := min(c[0], c[1], c[2]), max(c[0], c[1], c[2])
	if high == low {
		return [3]float64{}
	}
	for i := range c {
		c[i] = (c[i] - low) * s / (high - low)
	}
	return c
}
