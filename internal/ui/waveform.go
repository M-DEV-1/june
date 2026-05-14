package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

/*
	Waveform expectations (this one is a sine wave)
	⣤⣶⣶⣿⣿⣿⣿⣿⣶⣶⣤⣤⣀⣀⣀⣀⠀⣀⣀⣀⣀⣤⣶⣶⣿⣿⣿⣿⣿⣶⣶⣤⣤⣀⣀⣀⠀⠀⣀⣀⣀⣀⣤⣶⣶⣿⣿⣿⣿⣿⣶⣶⣤⣤⣀⣀⣀⠀⠀⣀
	⠛⠿⠿⣿⣿⣿⣿⣿⠿⠿⠛⠛⠉⠉⠉⠉⠀⠉⠉⠉⠉⠛⠿⠿⣿⣿⣿⣿⣿⠿⠿⠛⠛⠉⠉⠉⠀⠀⠉⠉⠉⠉⠛⠿⠿⣿⣿⣿⣿⣿⠿⠿⠛⠛⠉⠉⠉⠀⠀⠉
*/

// god bless gemini for this one

type Waveform struct {
	history []float64
	width   int
}

func NewWaveform(width int) *Waveform {
	if width <= 0 {
		width = 40
	}
	return &Waveform{
		history: make([]float64, width),
		width:   width,
	}
}

func (w *Waveform) Update(amp float64) {
	w.history = append(w.history[1:], amp)
}

func (w *Waveform) SetWidth(width int) {
	if width <= 0 || width == w.width {
		return
	}
	newHistory := make([]float64, width)
	copyLen := len(w.history)
	if copyLen > width {
		copyLen = width
	}
	copy(newHistory[width-copyLen:], w.history[len(w.history)-copyLen:])
	w.width = width
	w.history = newHistory
}

// creates a symmetric waveform using multi-row Braille
func (w *Waveform) Render(style lipgloss.Style, label string) string {
	height := 2 // 2 rows of braille characters = 8 dots high. Very high res.
	rows := make([]strings.Builder, height)

	// Braille dot layout:
	// 1 4
	// 2 5
	// 3 6
	// 7 8

	for i := 0; i < w.width; i++ {
		amp := w.history[i]

		// Map 0.0-1.0 to 0-8 dots total height (4 dots per row)
		totalDots := amp * float64(height) * 4.0
		if totalDots < 1.0 && amp > 0.01 {
			totalDots = 1.0
		}

		// We want to fill dots from the middle outwards.
		// Center is between row 0/1 and dots 2/3/4/5

		for r := 0; r < height; r++ {
			var char rune = 0x2800

			// Dots to enable in this row (0 to 4 dots)
			// Row 0 is top, Row 1 is bottom
			// For 2 rows, center is between row 0 (bottom dots) and row 1 (top dots)

			for d := 0; d < 4; d++ {
				// Distance from center line
				// center = row 1 top (dist 0)
				var dist float64
				if r == 0 {
					// Row 0 dots (from bottom to top)
					// dot 7,8: dist 0.5
					// dot 3,6: dist 1.5
					// dot 2,5: dist 2.5
					// dot 1,4: dist 3.5
					dist = float64(3-d) + 0.5
				} else {
					// Row 1 dots (from top to bottom)
					// dot 1,4: dist 0.5
					// dot 2,5: dist 1.5
					// dot 3,6: dist 2.5
					// dot 7,8: dist 3.5
					dist = float64(d) + 0.5
				}

				if dist <= totalDots/2.0 {
					switch d {
					case 0:
						char |= 0x01 | 0x08 // 1,4
					case 1:
						char |= 0x02 | 0x10 // 2,5
					case 2:
						char |= 0x04 | 0x20 // 3,6
					case 3:
						char |= 0x40 | 0x80 // 7,8
					}
				}
			}
			rows[r].WriteRune(char)
		}
	}

	var sb strings.Builder
	// FORCE unbackgrounded color only
	waveStyle := lipgloss.NewStyle().Foreground(style.GetForeground()).UnsetBackground()

	for i := 0; i < height; i++ {
		sb.WriteString(waveStyle.Render(rows[i].String()))
		if i < height-1 {
			sb.WriteString("\n")
		}
	}

	// this is for like speaker title (ORA VOICE) before waveform
	lblStyle := lipgloss.NewStyle().
		Foreground(style.GetForeground()).
		Bold(true).
		UnsetBackground()

	return lblStyle.Render(label) + "\n" + sb.String()
}
