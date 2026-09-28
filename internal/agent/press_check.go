package agent

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"time"

	"june/internal/tracker"
)

// pressSettle is the longest a press is given to show on the screen before it is called a miss. A row highlight or a button state lands within a frame or two and is seen by the first picture; the wait only runs its full length when nothing changes.
var pressSettle = 400 * time.Millisecond

// pressPoll is how long to wait between pictures while nothing has changed yet.
var pressPoll = 80 * time.Millisecond

// pressRadius is how far around the pressed point, in screen pixels, the before and after pictures are compared: about the height of two rows, so a row that highlighted or a menu that opened is inside it and a clock ticking in the corner is not.
const pressRadius = 60

// pressChangedShare is the share of compared pixels that must differ for the screen to count as changed. A highlight on one row is many times this; JPEG noise between two shots of the same screen is well under it.
const pressChangedShare = 0.005

// pressChannelStep is how far one colour channel must move for a pixel to count as different, on the 16-bit scale image.Color uses, so that JPEG re-encoding noise on a still screen does not read as a change.
const pressChannelStep = 24 << 8

// beforePress starts taking the picture a press will be judged against, so the camera runs while the overlay's pointer is flying rather than before it. Output: a function that waits for the picture; it hands back an empty capture when there is no camera or it failed, which makes pressCheck pass the press through unjudged.
func (a *Agent) beforePress(ctx context.Context) func() tracker.Capture {
	if a.capture == nil {
		return func() tracker.Capture { return tracker.Capture{} }
	}
	done := make(chan tracker.Capture, 1)
	go func() {
		c, err := a.capture(ctx)
		if err != nil {
			c = tracker.Capture{}
		}
		done <- c
	}()
	return func() tracker.Capture { return <-done }
}

// pressCheck compares the screen around a pressed point before and after the press. Input: a context, the waiter beforePress handed out, and the point in screen pixels. Output: "" as soon as a picture shows the screen changed there, or when nothing can say (no camera, no picture to start from, a picture that will not decode, a point outside both pictures); a tool error naming the point when pressSettle passes with every picture matching, which is a press that landed on nothing; a different tool error saying the check could not be verified when pressSettle passes without ever taking a single picture of the screen after the press, since a run of failed screenshots must never be reported as a successful press.
// This is the one check in the loop that does not share a sensor with the aim: the target came off the accessibility bus or the model's own reading of a picture, and the confirmation comes off the pixels.
func (a *Agent) pressCheck(ctx context.Context, before func() tracker.Capture, x, y int) string {
	if a.capture == nil {
		return ""
	}
	first := before()
	if len(first.Data) == 0 {
		return ""
	}
	deadline := time.Now().Add(pressSettle)
	for {
		after, err := a.capture(ctx)
		if err != nil {
			if !time.Now().Before(deadline) {
				return toolError(fmt.Sprintf("could not verify the press around %d,%d: the screen could not be photographed afterward, so nothing could be compared; look to see what happened", x, y))
			}
			time.Sleep(pressPoll)
			continue
		}
		changed, known := changedAround(first, after, x, y)
		if !known || changed {
			return ""
		}
		if !time.Now().Before(deadline) {
			// The box is 120 pixels wide, so it only sees what happens beside the button: a row highlighting, a menu dropping open. A link that navigates, a tab that switches, a button that opens a panel on the far side of the window all leave it identical and change the rest of the screen, and calling that a miss sent the loop back to look at a press that had worked.
			if wide, known := screenChanged(first, after); known && wide {
				return ""
			}
			return toolError(fmt.Sprintf("nothing on the screen changed around %d,%d after the press, so it landed on nothing; look, then aim again or take another route", x, y))
		}
		time.Sleep(pressPoll)
	}
}

// changedAround reports whether the pixels around one screen point differ between two captures. Input: the two captures and the point in screen pixels. Output: changed, and known; known is false when either picture will not decode or the point is outside either one, in which case changed is meaningless.
func changedAround(before, after tracker.Capture, x, y int) (changed, known bool) {
	b, _, err := image.Decode(bytes.NewReader(before.Data))
	if err != nil {
		return false, false
	}
	a, _, err := image.Decode(bytes.NewReader(after.Data))
	if err != nil {
		return false, false
	}
	rb, ok := boxAround(before, b.Bounds(), x, y)
	if !ok {
		return false, false
	}
	ra, ok := boxAround(after, a.Bounds(), x, y)
	if !ok || rb.Dx() != ra.Dx() || rb.Dy() != ra.Dy() {
		return false, false
	}
	differing, total := 0, rb.Dx()*rb.Dy()
	for dy := 0; dy < rb.Dy(); dy++ {
		for dx := 0; dx < rb.Dx(); dx++ {
			if pixelDiffers(b.At(rb.Min.X+dx, rb.Min.Y+dy), a.At(ra.Min.X+dx, ra.Min.Y+dy)) {
				differing++
			}
		}
	}
	return float64(differing) > pressChangedShare*float64(total), true
}

// boxAround is the rectangle of image pixels within pressRadius screen pixels of a screen point, clipped to the image. Input: the capture whose origin and scale map the point, the decoded image's bounds, and the point. Output: the rectangle, and false when the point is outside the picture.
func boxAround(c tracker.Capture, bounds image.Rectangle, x, y int) (image.Rectangle, bool) {
	scale := c.Scale
	if scale <= 0 {
		scale = 1
	}
	ix, iy := float64(x-c.X)/scale, float64(y-c.Y)/scale
	r := float64(pressRadius) / scale
	box := image.Rect(int(ix-r), int(iy-r), int(ix+r), int(iy+r)).Intersect(bounds)
	if box.Empty() || !image.Pt(int(ix), int(iy)).In(bounds) {
		return image.Rectangle{}, false
	}
	return box, true
}

// pixelDiffers reports whether two colours are further apart than JPEG noise in any channel.
func pixelDiffers(p, q interface{ RGBA() (r, g, b, a uint32) }) bool {
	pr, pg, pb, _ := p.RGBA()
	qr, qg, qb, _ := q.RGBA()
	return absDiff(pr, qr) > pressChannelStep || absDiff(pg, qg) > pressChannelStep || absDiff(pb, qb) > pressChannelStep
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// screenChanged reports whether two pictures of the whole screen differ, sampling every eighth pixel in each direction so a full-screen compare costs about what one press check does. Input: the two captures. Output: changed, and known; known is false when either picture will not decode or the two differ in size.
func screenChanged(before, after tracker.Capture) (changed, known bool) {
	b, _, err := image.Decode(bytes.NewReader(before.Data))
	if err != nil {
		return false, false
	}
	a, _, err := image.Decode(bytes.NewReader(after.Data))
	if err != nil {
		return false, false
	}
	rb, ra := b.Bounds(), a.Bounds()
	if rb.Dx() != ra.Dx() || rb.Dy() != ra.Dy() {
		return false, false
	}
	const step = 8
	differing, total := 0, 0
	for y := 0; y < rb.Dy(); y += step {
		for x := 0; x < rb.Dx(); x += step {
			total++
			if pixelDiffers(b.At(rb.Min.X+x, rb.Min.Y+y), a.At(ra.Min.X+x, ra.Min.Y+y)) {
				differing++
			}
		}
	}
	return float64(differing) > pressChangedShare*float64(total), true
}
