package ui

import (
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Waveform renders a live symmetric audio bar visualizer using Braille characters.
//
// Each column has a fixed variation factor producing irregular bar heights —
// taller bars respond more to loud audio, creating an organic spectrogram feel.
// Bars extend symmetrically above and below a center baseline.
// Silent columns show a dim gray centerline. Speaking columns fill in active color.
//
//	silent:  ⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀⣀
//	         ⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉⠉
//	active:  ⣿⣶⣤⣀⣤⣿⣦⣀⣤⣶⣿⣷⣤⣀⣤⣿⣶⣤⣀⣤
//	         ⣿⣿⣿⣀⣿⣿⣿⣀⣿⣿⣿⣿⣿⣀⣿⣿⣿⣀⣿⣿
type Waveform struct {
	smoothed  float64
	variation []float64
	width     int
}

const dimColor = lipgloss.Color("#3a3a4a")
const ampThreshold = 0.01

// riseAlpha: how fast amplitude rises (0=no rise, 1=instant).
// fallAlpha: how fast amplitude falls — 0.26 reaches near-zero in ~500ms (clear pauses, not jumpy).
const riseAlpha = 0.85
const fallAlpha = 0.26

// row0Levels: braille masks for top char (row0), filling from center upward.
// Index = number of dot-rows filled (0=empty, 4=full block).
var row0Levels = [5]rune{
	0x2800,                      // 0: empty
	0x2800 | 0x40 | 0x80,       // 1: dots 7,8  (bottom of top char = center)
	0x2800 | 0x40 | 0x80 | 0x04 | 0x20, // 2: + dots 3,6
	0x2800 | 0x40 | 0x80 | 0x04 | 0x20 | 0x02 | 0x10, // 3: + dots 2,5
	0x28FF,                      // 4: all dots
}

// row1Levels: braille masks for bottom char (row1), filling from center downward.
var row1Levels = [5]rune{
	0x2800,                      // 0: empty
	0x2800 | 0x01 | 0x08,       // 1: dots 1,4  (top of bottom char = center)
	0x2800 | 0x01 | 0x08 | 0x02 | 0x10, // 2: + dots 2,5
	0x2800 | 0x01 | 0x08 | 0x02 | 0x10 | 0x04 | 0x20, // 3: + dots 3,6
	0x28FF,                      // 4: all dots
}

func NewWaveform(width int) *Waveform {
	if width <= 0 {
		width = 40
	}
	return &Waveform{
		variation: buildVariation(width),
		width:     width,
	}
}

// buildVariation generates per-column multipliers (0.35–1.0) using overlapping
// sine waves so adjacent columns have correlated but irregular heights.
func buildVariation(width int) []float64 {
	v := make([]float64, width)
	for i := range v {
		f := float64(i)
		raw := math.Sin(f*0.9+0.4)*0.5 + math.Sin(f*0.3+1.1)*0.3 + math.Sin(f*1.7)*0.2
		// raw in ~[-1,1]; map to [0.35, 1.0]
		v[i] = 0.35 + 0.65*(raw+1.0)/2.0
	}
	return v
}

// Update pushes a new amplitude sample. Rise is fast, fall is smoothed.
func (w *Waveform) Update(amp float64) {
	if amp > w.smoothed {
		w.smoothed = w.smoothed*(1-riseAlpha) + amp*riseAlpha
	} else {
		w.smoothed = w.smoothed*(1-fallAlpha) + amp*fallAlpha
	}
	if w.smoothed < ampThreshold {
		w.smoothed = 0
	}
}

func (w *Waveform) SetWidth(width int) {
	if width <= 0 || width == w.width {
		return
	}
	w.width = width
	w.variation = buildVariation(width)
}

func (w *Waveform) Render(style lipgloss.Style, label string) string {
	activeColor := style.GetForeground()
	bg := style.GetBackground()

	// 4 rows: [0]=far-top [1]=near-top [2]=near-bottom [3]=far-bottom
	var rows [4]strings.Builder

	for i := 0; i < w.width; i++ {
		colAmp := w.smoothed * w.variation[i]
		active := colAmp >= ampThreshold

		var chs [4]rune
		var color lipgloss.TerminalColor

		if !active {
			chs[0] = row0Levels[0] // empty (far top)
			chs[1] = row0Levels[1] // dim centerline (near top)
			chs[2] = row1Levels[1] // dim centerline (near bottom)
			chs[3] = row1Levels[0] // empty (far bottom)
			color = dimColor
		} else {
			// gamma 0.42: close to original sensitivity, works with scaled RMS.
			// 0.05→0.18, 0.1→0.27, 0.3→0.53, 0.7→0.82, 1.0→1.0
			scaled := math.Pow(colAmp, 0.42)
			n := int(math.Round(scaled * 8))
			if n < 1 {
				n = 1
			}
			if n > 8 {
				n = 8
			}
			near := n
			if near > 4 {
				near = 4
			}
			far := n - 4
			if far < 0 {
				far = 0
			}
			chs[0] = row0Levels[far]  // far top fills outward from row1
			chs[1] = row0Levels[near] // near top fills from center
			chs[2] = row1Levels[near] // near bottom fills from center
			chs[3] = row1Levels[far]  // far bottom fills outward from row2
			color = activeColor
		}

		cs := lipgloss.NewStyle().Foreground(color).Background(bg)
		for r := range rows {
			rows[r].WriteString(cs.Render(string(chs[r])))
		}
	}

	bgFill := lipgloss.NewStyle().Background(bg).Width(w.width)
	lblStyle := lipgloss.NewStyle().
		Foreground(activeColor).
		Background(bg).
		Width(w.width).
		Bold(true)

	elems := make([]string, 5) // label + 4 rows
	elems[0] = lblStyle.Render(label)
	for i := range rows {
		elems[i+1] = bgFill.Render(rows[i].String())
	}
	return lipgloss.JoinVertical(lipgloss.Left, elems...)
}
